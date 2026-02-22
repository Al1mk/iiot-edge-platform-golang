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

	// dbUp is 1 when the SQLite database is reachable, 0 otherwise.
	// It is updated on every successful/failed DB write.
	dbUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "iiot_db_up",
		Help: "1 if the ingestor SQLite database is reachable, 0 otherwise.",
	})

	// dbWriteFailTotal counts failed INSERT attempts.
	dbWriteFailTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "iiot_db_write_fail_total",
		Help: "Total number of failed telemetry INSERT operations.",
	})

	// dbRowsTotal tracks the current number of rows in the telemetry table.
	dbRowsTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "iiot_db_rows_total",
		Help: "Current number of rows in the telemetry table.",
	})

	// dbFileBytes is the size of the SQLite file in bytes (0 for :memory:).
	dbFileBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "iiot_db_file_bytes",
		Help: "Size of the SQLite database file in bytes. 0 for in-memory databases.",
	})

	// dbLastWriteUnix is the unix timestamp of the most-recent successful INSERT.
	dbLastWriteUnix = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "iiot_db_last_write_unix",
		Help: "Unix timestamp (seconds) of the most-recent successful telemetry INSERT. 0 if none.",
	})
)

// lastEntry holds the most-recently accepted telemetry reading and the wall
// clock time it was stored. Protected by lastMu.
// Retained for backward-compat with existing tests and the in-process /last
// fast path (avoids a DB read on every request when DB is unavailable).
type lastEntry struct {
	ReceivedAt int64                   `json:"received_at"` // unix seconds
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

func makeTelemetryHandler(maxSkew time.Duration, st *store) http.HandlerFunc {
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

		receivedAt := time.Now().Unix()

		// Persist to SQLite. Log but do not fail the request on DB error.
		if st != nil {
			if err := st.insert(reading, receivedAt); err != nil {
				logger.Error("db insert failed", "error", err)
				dbWriteFailTotal.Inc()
				dbUp.Set(0)
			} else {
				dbUp.Set(1)
				snap := st.statsSnapshot()
				dbRowsTotal.Set(float64(snap.RowsTotal))
				dbLastWriteUnix.Set(float64(snap.LastWriteUnix))
				dbFileBytes.Set(float64(snap.FileBytes))
			}
		}

		// Update in-memory last entry and gauge (fast path, always runs).
		lastMu.Lock()
		lastStore = &lastEntry{ReceivedAt: receivedAt, Payload: reading}
		lastMu.Unlock()
		lastTelemetryTimestamp.Set(float64(receivedAt))

		writeJSON(w, http.StatusAccepted, map[string]string{"result": "accepted"})
	}
}

// makeLastHandler serves GET /api/v1/telemetry/last[?device_id=<id>].
// When a DB is available it queries there (durable across restarts).
// Falls back to the in-memory store if st is nil.
func makeLastHandler(st *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.URL.Query().Get("device_id")

		// DB path.
		if st != nil {
			row, err := st.queryLast(deviceID)
			if err != nil {
				logger.Error("db queryLast failed", "error", err)
				writeError(w, http.StatusInternalServerError, "db_error", "database query failed")
				return
			}
			if row == nil {
				writeError(w, http.StatusNotFound, "no_telemetry", "no telemetry received")
				return
			}
			writeJSON(w, http.StatusOK, row)
			return
		}

		// In-memory fallback (no DB).
		lastMu.RLock()
		entry := lastStore
		lastMu.RUnlock()
		if entry == nil {
			writeError(w, http.StatusNotFound, "no_telemetry", "no telemetry received")
			return
		}
		writeJSON(w, http.StatusOK, entry)
	}
}

// makeRecentHandler serves GET /api/v1/telemetry/recent[?limit=N&device_id=<id>].
// Requires a DB; returns 503 if none is configured.
func makeRecentHandler(st *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if st == nil {
			writeError(w, http.StatusServiceUnavailable, "no_db", "persistence not configured")
			return
		}

		deviceID := r.URL.Query().Get("device_id")

		limit := 100
		if s := r.URL.Query().Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				n = 1
			} else if n > 500 {
				n = 500
			}
			limit = n
		}

		rows, err := st.queryRecent(deviceID, limit)
		if err != nil {
			logger.Error("db queryRecent failed", "error", err)
			writeError(w, http.StatusInternalServerError, "db_error", "database query failed")
			return
		}
		// Return an empty array rather than null when no rows.
		if rows == nil {
			rows = []telemetryRow{}
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

// telemetryStatsResponse is the JSON body returned by GET /api/v1/telemetry/stats.
type telemetryStatsResponse struct {
	DBUp          int   `json:"db_up"`
	RowsTotal     int64 `json:"rows_total"`
	LastWriteUnix int64 `json:"last_write_unix"`
	DBFileBytes   int64 `json:"db_file_bytes"`
}

// makeStatsHandler serves GET /api/v1/telemetry/stats.
// Returns 200 with current DB statistics when the DB is available, 503 otherwise.
// All fields fall back to zero when st is nil so callers never get a panic.
func makeStatsHandler(st *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if st == nil {
			writeJSON(w, http.StatusServiceUnavailable, telemetryStatsResponse{})
			return
		}
		if err := st.ping(); err != nil {
			logger.Error("db ping failed in stats handler", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, telemetryStatsResponse{})
			return
		}
		snap := st.statsSnapshot()
		writeJSON(w, http.StatusOK, telemetryStatsResponse{
			DBUp:          1,
			RowsTotal:     snap.RowsTotal,
			LastWriteUnix: snap.LastWriteUnix,
			DBFileBytes:   snap.FileBytes,
		})
	}
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
	case "/api/v1/telemetry/recent":
		return "/api/v1/telemetry/recent"
	case "/api/v1/telemetry/stats":
		return "/api/v1/telemetry/stats"
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
	dbPath := getEnv("INGESTOR_DB_PATH", "/tmp/iiot.db")

	logger.Info("starting ingestor",
		"version", version,
		"addr", addr,
		"metrics_addr", metricsAddr,
		"max_ts_skew", maxSkew.String(),
		"db_path", dbPath,
	)

	// Open SQLite store. Non-fatal: if it fails the ingestor still serves
	// requests; DB metrics will reflect the outage.
	st, err := openStore(dbPath)
	if err != nil {
		logger.Error("failed to open db; running without persistence", "error", err)
		dbUp.Set(0)
	} else {
		dbUp.Set(1)
		snap := st.statsSnapshot()
		dbRowsTotal.Set(float64(snap.RowsTotal))
		dbLastWriteUnix.Set(float64(snap.LastWriteUnix))
		dbFileBytes.Set(float64(snap.FileBytes))
		defer st.close()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("POST /api/v1/telemetry", makeTelemetryHandler(maxSkew, st))
	mux.HandleFunc("GET /api/v1/telemetry/last", makeLastHandler(st))
	mux.HandleFunc("GET /api/v1/telemetry/recent", makeRecentHandler(st))
	mux.HandleFunc("GET /api/v1/telemetry/stats", makeStatsHandler(st))

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
