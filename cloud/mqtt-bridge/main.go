package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/alimk/iiot-edge-platform/pkg/models"
)

var version = "dev"

var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	messagesReceived = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bridge_messages_received_total",
		Help: "Total MQTT messages received by the bridge.",
	})
	messagesInvalid = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bridge_messages_invalid_total",
		Help: "Total messages that failed JSON decode or Validate().",
	})
	forwardSuccess = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bridge_forward_success_total",
		Help: "Total messages successfully forwarded to the ingestor.",
	})
	forwardFailure = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bridge_forward_failure_total",
		Help: "Total messages that could not be forwarded after all retries.",
	})
	forwardRetry = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bridge_forward_retry_total",
		Help: "Total individual retry attempts (not counting first attempt).",
	})
	forwardDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "bridge_forward_duration_seconds",
		Help:    "End-to-end HTTP POST latency in seconds, including all retries.",
		Buckets: prometheus.DefBuckets,
	})
	queueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bridge_queue_depth",
		Help: "Current number of messages waiting in the processing queue.",
	})
	queueDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bridge_queue_dropped_total",
		Help: "Total messages dropped because the queue was full.",
	})
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type config struct {
	mqttBroker      string
	mqttTopic       string
	ingestorURL     string
	metricsAddr     string
	queueSize       int
	workers         int
	shutdownTimeout time.Duration
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		logger.Warn("invalid env var, using default", "key", key, "value", raw, "default", defaultVal)
		return defaultVal
	}
	return v
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		logger.Warn("invalid env var, using default", "key", key, "value", raw, "default", defaultVal)
		return defaultVal
	}
	return d
}

func newConfig() config {
	return config{
		mqttBroker:      getEnv("MQTT_BROKER", "tcp://mosquitto:1883"),
		mqttTopic:       getEnv("MQTT_TOPIC", "iiot/telemetry"),
		ingestorURL:     getEnv("INGESTOR_URL", "http://ingestor:8080/api/v1/telemetry"),
		metricsAddr:     getEnv("METRICS_ADDR", ":9092"),
		queueSize:       getEnvInt("BRIDGE_QUEUE_SIZE", 1000),
		workers:         getEnvInt("BRIDGE_WORKERS", 16),
		shutdownTimeout: getEnvDuration("SHUTDOWN_TIMEOUT", 10*time.Second),
	}
}

// ---------------------------------------------------------------------------
// Bridge
// ---------------------------------------------------------------------------

// bridge owns all runtime state. Fields are set once in newBridge; only
// the dropLogAt atomic is mutated afterwards (by the MQTT handler goroutine).
type bridge struct {
	cfg       config
	transport *http.Transport
	client    *http.Client
	queue     chan []byte

	// dropLogAt holds the Unix nanosecond timestamp of the last drop log line.
	// Written/read atomically to avoid a mutex on the hot MQTT path.
	dropLogAt atomic.Int64
}

func newBridge(cfg config) *bridge {
	t := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// ResponseHeaderTimeout protects against slow ingestor responses that
		// would pin a connection without counting against http.Client.Timeout.
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &bridge{
		cfg:       cfg,
		transport: t,
		client: &http.Client{
			Timeout:   5 * time.Second,
			Transport: t,
		},
		queue: make(chan []byte, cfg.queueSize),
	}
}

// ---------------------------------------------------------------------------
// MQTT handler (called on paho's internal goroutine — must not block)
// ---------------------------------------------------------------------------

// mqttHandler returns the paho MessageHandler closure. It copies the payload
// immediately (paho reuses the underlying buffer), updates the received
// counter, and tries a non-blocking enqueue. If the queue is full the message
// is dropped and bridge_queue_dropped_total is incremented. Drop log lines are
// rate-limited to at most one per second to avoid log flooding.
func (b *bridge) mqttHandler() mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		messagesReceived.Inc()

		payload := msg.Payload()
		data := make([]byte, len(payload))
		copy(data, payload)

		select {
		case b.queue <- data:
			queueDepth.Inc()
		default:
			queueDropped.Inc()
			b.logDropRateLimited()
		}
	}
}

// logDropRateLimited emits at most one warning log per second regardless of
// how many drops occur within that window.
func (b *bridge) logDropRateLimited() {
	now := time.Now().UnixNano()
	last := b.dropLogAt.Load()
	if now-last >= int64(time.Second) && b.dropLogAt.CompareAndSwap(last, now) {
		logger.Warn("queue full, message dropped — consider increasing BRIDGE_QUEUE_SIZE or BRIDGE_WORKERS")
	}
}

// ---------------------------------------------------------------------------
// Worker pool
// ---------------------------------------------------------------------------

// startWorkers launches cfg.workers goroutines that drain b.queue until it is
// closed. It returns a WaitGroup the caller must Wait on to confirm all
// in-flight items have been processed before proceeding with shutdown.
func (b *bridge) startWorkers(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	for range b.cfg.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for data := range b.queue {
				queueDepth.Dec()
				b.process(ctx, data)
			}
		}()
	}
	return &wg
}

// ---------------------------------------------------------------------------
// Processing pipeline
// ---------------------------------------------------------------------------

// process decodes, validates, and forwards a single raw JSON payload.
// It is safe to call from multiple goroutines concurrently.
func (b *bridge) process(ctx context.Context, data []byte) {
	var reading models.TelemetryReading
	if err := json.Unmarshal(data, &reading); err != nil {
		messagesInvalid.Inc()
		logger.Warn("failed to decode MQTT payload", "error", err)
		return
	}

	if err := reading.Validate(); err != nil {
		messagesInvalid.Inc()
		logger.Warn("invalid telemetry reading",
			"device_id", reading.DeviceID,
			"error", err,
		)
		return
	}

	if err := b.forward(ctx, data, reading.DeviceID); err != nil {
		forwardFailure.Inc()
		logger.Error("forward failed after all retries",
			"device_id", reading.DeviceID,
			"error", err,
		)
		return
	}

	forwardSuccess.Inc()
	// Successes are not logged individually to avoid log spam under load.
	// bridge_forward_success_total is the canonical signal.
}

// ---------------------------------------------------------------------------
// HTTP forwarding with exponential backoff
// ---------------------------------------------------------------------------

const (
	maxAttempts = 5
	baseDelay   = 200 * time.Millisecond
)

// forward POSTs data to the ingestor. It retries on network errors and 5xx
// responses, with exponential backoff starting at baseDelay. 4xx responses
// are non-retryable. Every backoff sleep is cancellable via ctx.
func (b *bridge) forward(ctx context.Context, payload []byte, deviceID string) error {
	start := time.Now()
	var lastErr error

	for attempt := range maxAttempts {
		if ctx.Err() != nil {
			forwardDuration.Observe(time.Since(start).Seconds())
			return fmt.Errorf("context cancelled before attempt %d: %w", attempt, ctx.Err())
		}

		if attempt > 0 {
			forwardRetry.Inc()
			// Delay: 200ms, 400ms, 800ms, 1.6s for attempts 1–4.
			delay := time.Duration(float64(baseDelay) * math.Pow(2, float64(attempt-1)))
			logger.Info("retrying forward",
				"device_id", deviceID,
				"retry_count", attempt,
				"delay", delay.String(),
			)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				forwardDuration.Observe(time.Since(start).Seconds())
				return fmt.Errorf("context cancelled during backoff: %w", ctx.Err())
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.ingestorURL,
			bytes.NewReader(payload))
		if err != nil {
			// Request construction failure is non-retryable (bad URL etc.).
			forwardDuration.Observe(time.Since(start).Seconds())
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := b.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http do attempt %d: %w", attempt, err)
			logger.Warn("forward attempt failed",
				"device_id", deviceID,
				"attempt", attempt,
				"error", err,
			)
			continue // network error → retry
		}
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusAccepted:
			forwardDuration.Observe(time.Since(start).Seconds())
			return nil

		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			// 4xx: the payload is bad; retrying the same bytes will always fail.
			forwardDuration.Observe(time.Since(start).Seconds())
			return fmt.Errorf("ingestor rejected payload: HTTP %d (non-retryable)", resp.StatusCode)

		default:
			// 5xx or unexpected 2xx/3xx → retry.
			lastErr = fmt.Errorf("ingestor returned HTTP %d on attempt %d", resp.StatusCode, attempt)
			logger.Warn("forward attempt got unexpected status",
				"device_id", deviceID,
				"attempt", attempt,
				"status", resp.StatusCode,
			)
		}
	}

	forwardDuration.Observe(time.Since(start).Seconds())
	return fmt.Errorf("all %d attempts failed, last error: %w", maxAttempts, lastErr)
}

// ---------------------------------------------------------------------------
// MQTT client
// ---------------------------------------------------------------------------

// newMQTTClient dials the broker and returns a connected client. The provided
// handler is re-subscribed inside OnConnectHandler so it survives reconnects
// (paho's AutoReconnect does not restore subscriptions automatically).
func newMQTTClient(cfg config, handler mqtt.MessageHandler) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.mqttBroker).
		SetClientID("iiot-mqtt-bridge").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			logger.Info("connected to MQTT broker", "broker", cfg.mqttBroker)
			tok := c.Subscribe(cfg.mqttTopic, 1, handler)
			if ok := tok.WaitTimeout(10 * time.Second); !ok {
				logger.Warn("subscribe timed out after reconnect", "topic", cfg.mqttTopic)
				return
			}
			if err := tok.Error(); err != nil {
				logger.Error("subscribe failed after reconnect", "topic", cfg.mqttTopic, "error", err)
				return
			}
			logger.Info("subscribed to MQTT topic", "topic", cfg.mqttTopic, "qos", 1)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			logger.Warn("MQTT connection lost, will reconnect", "error", err)
		})

	client := mqtt.NewClient(opts)
	tok := client.Connect()
	if ok := tok.WaitTimeout(10 * time.Second); !ok {
		return nil, fmt.Errorf("MQTT connect timed out")
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("MQTT connect: %w", err)
	}
	return client, nil
}

// ---------------------------------------------------------------------------
// Metrics server
// ---------------------------------------------------------------------------

func startMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", "error", err)
		}
	}()
	return srv
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	healthcheck := flag.Bool("healthcheck", false, "Probe the metrics server and exit 0/1.")
	flag.Parse()

	if *healthcheck {
		conn, err := net.DialTimeout("tcp", "localhost:9092", 3*time.Second)
		if err != nil {
			os.Exit(1)
		}
		conn.Close()
		os.Exit(0)
	}

	cfg := newConfig()

	logger.Info("starting mqtt-bridge",
		"version", version,
		"broker", cfg.mqttBroker,
		"topic", cfg.mqttTopic,
		"ingestor_url", cfg.ingestorURL,
		"metrics_addr", cfg.metricsAddr,
		"queue_size", cfg.queueSize,
		"workers", cfg.workers,
		"shutdown_timeout", cfg.shutdownTimeout.String(),
	)

	metricsSrv := startMetricsServer(cfg.metricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b := newBridge(cfg)

	// Start workers before connecting to MQTT so no message is lost between
	// the first subscribe ACK and the first worker becoming ready.
	wg := b.startWorkers(ctx)

	mqttClient, err := newMQTTClient(cfg, b.mqttHandler())
	if err != nil {
		logger.Error("initial MQTT connect failed, shutting down", "error", err)
		close(b.queue)
		wg.Wait()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
		return
	}

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info("shutdown signal received, draining queue")

	// 1. Disconnect MQTT: stops new messages arriving.
	//    500 ms quiesce lets paho deliver any already-received QoS-1 messages.
	mqttClient.Disconnect(500)

	// 2. Close the queue channel: signals workers to drain and exit after the
	//    last item. No send to b.queue occurs after mqttClient.Disconnect.
	close(b.queue)

	// 3. Wait for workers to finish, bounded by shutdownTimeout.
	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workersDone)
	}()

	select {
	case <-workersDone:
		logger.Info("all workers finished cleanly")
	case <-time.After(cfg.shutdownTimeout):
		logger.Warn("shutdown timeout reached before workers finished",
			"timeout", cfg.shutdownTimeout.String())
	}

	// 4. Release idle HTTP connections.
	b.transport.CloseIdleConnections()

	// 5. Shut down the metrics server.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutCtx)

	logger.Info("mqtt-bridge stopped")
}
