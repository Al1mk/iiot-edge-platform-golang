package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
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

var (
	publishSuccess = promauto.NewCounter(prometheus.CounterOpts{
		Name: "iiot_publish_success_total",
		Help: "Total number of telemetry messages successfully published to MQTT.",
	})
	publishFailure = promauto.NewCounter(prometheus.CounterOpts{
		Name: "iiot_publish_failure_total",
		Help: "Total number of telemetry publish attempts that returned an error.",
	})
	publishTimeout = promauto.NewCounter(prometheus.CounterOpts{
		Name: "iiot_publish_timeout_total",
		Help: "Total number of telemetry publish attempts that timed out waiting for ack.",
	})
)

type config struct {
	broker          string
	clientID        string
	topic           string
	deviceID        string
	metricsAddr     string
	publishInterval time.Duration
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func newConfig() config {
	intervalRaw := getEnv("PUBLISH_INTERVAL", "5s")
	interval, err := time.ParseDuration(intervalRaw)
	if err != nil || interval <= 0 {
		logger.Warn("invalid PUBLISH_INTERVAL, using default 5s", "value", intervalRaw)
		interval = 5 * time.Second
	}
	return config{
		broker:          getEnv("MQTT_BROKER", "tcp://localhost:1883"),
		clientID:        getEnv("MQTT_CLIENT_ID", "iiot-collector-1"),
		topic:           getEnv("MQTT_TOPIC", "iiot/telemetry"),
		deviceID:        getEnv("DEVICE_ID", "sensor-001"),
		metricsAddr:     getEnv("METRICS_ADDR", ":9090"),
		publishInterval: interval,
	}
}

func sampleReading(rng *rand.Rand, deviceID string) models.TelemetryReading {
	// TODO: Replace with real Modbus/OPC-UA reads from field hardware.
	return models.TelemetryReading{
		DeviceID:     deviceID,
		Timestamp:    time.Now().UTC(),
		TemperatureC: 20.0 + rng.Float64()*15.0,  // 20–35 °C
		PressureHPA:  1000.0 + rng.Float64()*30.0, // 1000–1030 hPa
		HumidityPct:  40.0 + rng.Float64()*40.0,   // 40–80 %
		Status:       "ok",
	}
}

func newMQTTClient(cfg config) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.broker).
		SetClientID(cfg.clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(_ mqtt.Client) {
			logger.Info("connected to MQTT broker", "broker", cfg.broker)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			logger.Warn("MQTT connection lost, reconnecting", "error", err)
		})

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if ok := token.WaitTimeout(10 * time.Second); !ok {
		return nil, fmt.Errorf("MQTT connect timed out")
	}
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("MQTT connect failed: %w", err)
	}
	return client, nil
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

func main() {
	healthcheck := flag.Bool("healthcheck", false, "Probe the metrics server and exit 0/1.")
	flag.Parse()

	if *healthcheck {
		conn, err := net.DialTimeout("tcp", "localhost:9090", 3*time.Second)
		if err != nil {
			os.Exit(1)
		}
		conn.Close()
		os.Exit(0)
	}

	cfg := newConfig()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	logger.Info("starting collector",
		"version", version,
		"broker", cfg.broker,
		"client_id", cfg.clientID,
		"topic", cfg.topic,
		"device_id", cfg.deviceID,
		"publish_interval", cfg.publishInterval.String(),
	)

	metricsSrv := startMetricsServer(cfg.metricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := newMQTTClient(cfg)
	if err != nil {
		logger.Error("initial MQTT connect failed, shutting down", "error", err)
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
		return
	}

	ticker := time.NewTicker(cfg.publishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down collector")
			client.Disconnect(500)
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsSrv.Shutdown(shutCtx)
			return

		case <-ticker.C:
			reading := sampleReading(rng, cfg.deviceID)
			payload, err := json.Marshal(reading)
			if err != nil {
				logger.Error("failed to marshal telemetry", "error", err)
				continue
			}
			token := client.Publish(cfg.topic, 1, false, payload)
			if ok := token.WaitTimeout(3 * time.Second); !ok {
				logger.Warn("publish timed out")
				publishTimeout.Inc()
				continue
			}
			if err := token.Error(); err != nil {
				logger.Error("publish failed", "error", err)
				publishFailure.Inc()
			} else {
				logger.Debug("published telemetry",
					"topic", cfg.topic,
					"temperature_c", reading.TemperatureC,
					"pressure_hpa", reading.PressureHPA,
					"humidity_pct", reading.HumidityPct,
				)
				publishSuccess.Inc()
			}
		}
	}
}
