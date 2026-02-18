package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alimk/iiot-edge-platform/pkg/models"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testBridge(ingestorURL string) *bridge {
	cfg := config{
		mqttBroker:      "tcp://localhost:1883",
		mqttTopic:       "iiot/telemetry",
		ingestorURL:     ingestorURL,
		metricsAddr:     ":0",
		queueSize:       64,
		workers:         2,
		shutdownTimeout: 2 * time.Second,
	}
	return newBridge(cfg)
}

func validPayload(t *testing.T) []byte {
	t.Helper()
	r := models.TelemetryReading{
		DeviceID:     "test-device",
		Timestamp:    time.Now().UTC(),
		TemperatureC: 25.0,
		PressureHPA:  1013.0,
		HumidityPct:  50.0,
		Status:       "ok",
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// config parsing
// ---------------------------------------------------------------------------

func TestGetEnvInt_Defaults(t *testing.T) {
	if got := getEnvInt("BRIDGE_QUEUE_SIZE_UNSET_XYZ", 1000); got != 1000 {
		t.Fatalf("expected 1000, got %d", got)
	}
}

func TestGetEnvInt_Invalid(t *testing.T) {
	t.Setenv("BRIDGE_QUEUE_SIZE_TEST", "not-a-number")
	if got := getEnvInt("BRIDGE_QUEUE_SIZE_TEST", 42); got != 42 {
		t.Fatalf("expected fallback 42, got %d", got)
	}
}

func TestGetEnvInt_Zero(t *testing.T) {
	t.Setenv("BRIDGE_WORKERS_TEST", "0")
	if got := getEnvInt("BRIDGE_WORKERS_TEST", 16); got != 16 {
		t.Fatalf("expected fallback 16 for zero value, got %d", got)
	}
}

func TestGetEnvDuration_Invalid(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT_TEST", "notaduration")
	if got := getEnvDuration("SHUTDOWN_TIMEOUT_TEST", 10*time.Second); got != 10*time.Second {
		t.Fatalf("expected fallback 10s, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// process — invalid payloads
// ---------------------------------------------------------------------------

func TestProcess_InvalidJSON(t *testing.T) {
	b := testBridge("http://127.0.0.1:0") // unreachable; should not be called
	// Garbage bytes must not panic and must not call the ingestor.
	b.process(context.Background(), []byte("{not json}"))
	// If we reach here without panic the test passes.
}

func TestProcess_FailsValidation(t *testing.T) {
	b := testBridge("http://127.0.0.1:0")
	// Empty device_id fails Validate().
	bad := models.TelemetryReading{
		DeviceID:     "",
		Timestamp:    time.Now().UTC(),
		TemperatureC: 25.0,
		PressureHPA:  1013.0,
		HumidityPct:  50.0,
		Status:       "ok",
	}
	data, _ := json.Marshal(bad)
	b.process(context.Background(), data)
}

// ---------------------------------------------------------------------------
// forward — HTTP status classification
// ---------------------------------------------------------------------------

func TestForward_SuccessOn202(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	b := testBridge(srv.URL)
	err := b.forward(context.Background(), validPayload(t), "dev-1")
	if err != nil {
		t.Fatalf("expected nil error on 202, got: %v", err)
	}
}

func TestForward_NonRetryableOn422(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	b := testBridge(srv.URL)
	err := b.forward(context.Background(), validPayload(t), "dev-1")
	if err == nil {
		t.Fatal("expected error for 422")
	}
	if calls.Load() != 1 {
		t.Fatalf("422 should not be retried, got %d calls", calls.Load())
	}
}

func TestForward_NonRetryableOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	b := testBridge(srv.URL)
	err := b.forward(context.Background(), validPayload(t), "dev-1")
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if calls.Load() != 1 {
		t.Fatalf("400 should not be retried, got %d calls", calls.Load())
	}
}

func TestForward_RetriesOn503(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	b := testBridge(srv.URL)
	err := b.forward(context.Background(), validPayload(t), "dev-1")
	if err == nil {
		t.Fatal("expected error after exhausting retries on 503")
	}
	if got := calls.Load(); got != maxAttempts {
		t.Fatalf("expected %d attempts on 503, got %d", maxAttempts, got)
	}
}

func TestForward_RetriesOn500ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	b := testBridge(srv.URL)
	err := b.forward(context.Background(), validPayload(t), "dev-1")
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 calls (2 failures + 1 success), got %d", got)
	}
}

func TestForward_ContextCancelledDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	b := testBridge(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after the first attempt so we enter the backoff sleep, then cancel.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := b.forward(ctx, validPayload(t), "dev-1")
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
	// Must have made exactly 1 call (the first attempt) before ctx was cancelled.
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 call before cancel, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// queue drop rate limiting
// ---------------------------------------------------------------------------

func TestDropLog_RateLimited(t *testing.T) {
	b := testBridge("http://127.0.0.1:0")

	// First call must set the timestamp.
	b.logDropRateLimited()
	first := b.dropLogAt.Load()
	if first == 0 {
		t.Fatal("expected dropLogAt to be set after first call")
	}

	// Immediate second call must not update the timestamp (within 1 s window).
	b.logDropRateLimited()
	if b.dropLogAt.Load() != first {
		t.Fatal("dropLogAt must not change within the 1-second rate-limit window")
	}
}

// ---------------------------------------------------------------------------
// worker pool drains queue on close
// ---------------------------------------------------------------------------

func TestWorkers_DrainOnClose(t *testing.T) {
	// Use a server that always returns 202.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	cfg := config{
		mqttBroker:      "tcp://localhost:1883",
		mqttTopic:       "iiot/telemetry",
		ingestorURL:     srv.URL,
		metricsAddr:     ":0",
		queueSize:       64,
		workers:         4,
		shutdownTimeout: 5 * time.Second,
	}
	b := newBridge(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	wg := b.startWorkers(ctx)

	// Enqueue 10 valid messages.
	const n = 10
	payload := validPayload(t)
	for range n {
		b.queue <- payload
		queueDepth.Inc()
	}

	// Cancel context and close queue: workers must drain and exit.
	cancel()
	close(b.queue)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// pass
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not finish draining within 5 s")
	}
}
