package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/alimk/iiot-edge-platform/pkg/models"
)

var version = "dev"

var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "iiot_http_requests_total",
		Help: "Total number of HTTP requests by method, route, and status code.",
	}, []string{"method", "route", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "iiot_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds by method and route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	// lastTelemetryTimestamp is set to the unix-second timestamp of the last
	// successfully accepted telemetry reading. It is 0 until the first reading
	// arrives; Prometheus will emit the metric at 0 in that case.
	lastTelemetryTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "iiot_last_telemetry_timestamp_seconds",
		Help: "Unix timestamp (seconds) of the last successfully accepted telemetry reading. 0 if none received yet.",
	})
)

// lastEntry holds the most-recently accepted telemetry reading and the wall
// clock time it was stored. Protected by lastMu.
type lastEntry struct {
	ReceivedAt int64                  `json:"received_at"` // unix seconds
	Payload    models.TelemetryReading `json:"payload"`
}

var (
	lastMu    sync.RWMutex
	lastStore *lastEntry // nil until first successful POST
)

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// responseRecorder wraps ResponseWriter to capture the written status code.
type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.status = code
	rr.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func makeTelemetryHandler(maxSkew time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Apply body limit before any reads.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

		var reading models.TelemetryReading
		if err := json.NewDecoder(r.Body).Decode(&reading); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}

		if err := reading.Validate(); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
			return
		}

		if maxSkew > 0 {
			now := time.Now().UTC()
			delta := reading.Timestamp.UTC().Sub(now)
			if delta < -maxSkew || delta > maxSkew {
				msg := fmt.Sprintf("timestamp skew %.0fs exceeds limit %.0fs",
					delta.Seconds(), maxSkew.Seconds())
				writeError(w, http.StatusUnprocessableEntity, "validation_failed", msg)
				return
			}
		}

		logger.Info("received telemetry",
			"device_id", reading.DeviceID,
			"timestamp", reading.Timestamp,
			"temperature_c", reading.TemperatureC,
			"pressure_hpa", reading.PressureHPA,
			"humidity_pct", reading.HumidityPct,
			"status", reading.Status,
		)

		// TODO: Forward to Kafka topic or write to TimescaleDB / InfluxDB.

		// Store last accepted reading and update the Prometheus gauge.
		now := time.Now().Unix()
		lastMu.Lock()
		lastStore = &lastEntry{ReceivedAt: now, Payload: reading}
		lastMu.Unlock()
		lastTelemetryTimestamp.Set(float64(now))

		writeJSON(w, http.StatusAccepted, map[string]string{"result": "accepted"})
	}
}

// lastTelemetryHandler serves GET /api/v1/telemetry/last.
// Returns 404 if no telemetry has been received since startup; otherwise 200.
func lastTelemetryHandler(w http.ResponseWriter, _ *http.Request) {
	lastMu.RLock()
	entry := lastStore
	lastMu.RUnlock()

	if entry == nil {
		writeError(w, http.StatusNotFound, "no_telemetry", "no telemetry received")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// routeLabel returns a stable Prometheus label for the request path,
// avoiding cardinality explosion from raw URLs with IDs or query strings.
func routeLabel(r *http.Request) string {
	switch r.URL.Path {
	case "/healthz":
		return "/healthz"
	case "/api/v1/telemetry":
		return "/api/v1/telemetry"
	case "/api/v1/telemetry/last":
		return "/api/v1/telemetry/last"
	default:
		return "other"
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		route := routeLabel(r)
		rr := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rr, r)

		duration := time.Since(start)
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rr.status,
			"remote", r.RemoteAddr,
			"duration_ms", duration.Milliseconds(),
		)

		status := strconv.Itoa(rr.status)
		httpRequestsTotal.WithLabelValues(r.Method, route, status).Inc()
		httpRequestDuration.WithLabelValues(r.Method, route).Observe(duration.Seconds())
	})
}

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

func parseMaxSkew() time.Duration {
	raw := getEnv("MAX_TS_SKEW", "24h")
	d, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn("invalid MAX_TS_SKEW, using default 24h", "value", raw)
		return 24 * time.Hour
	}
	return d
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "Probe the HTTP server and exit 0/1.")
	flag.Parse()

	if *healthcheck {
		conn, err := net.DialTimeout("tcp", "localhost:8080", 3*time.Second)
		if err != nil {
			os.Exit(1)
		}
		conn.Close()
		os.Exit(0)
	}

	addr := ":8080"
	metricsAddr := ":9091"
	maxSkew := parseMaxSkew()

	logger.Info("starting ingestor",
		"version", version,
		"addr", addr,
		"metrics_addr", metricsAddr,
		"max_ts_skew", maxSkew.String(),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("POST /api/v1/telemetry", makeTelemetryHandler(maxSkew))
	mux.HandleFunc("GET /api/v1/telemetry/last", lastTelemetryHandler)

	srv := &http.Server{
		Addr:         addr,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	metricsSrv := startMetricsServer(metricsAddr)

	serverErrCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
	case err := <-serverErrCh:
		logger.Error("server exited unexpectedly", "error", err)
	}

	logger.Info("shutting down ingestor gracefully")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	_ = metricsSrv.Shutdown(shutCtx)
	logger.Info("ingestor stopped")
}
