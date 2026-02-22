package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// newTestMux returns a fresh ServeMux wired identically to main(), but with
// maxSkew=0 (no skew enforcement) and a fresh in-memory SQLite store so
// tests are fully independent and don't touch the filesystem.
func newTestMux(t *testing.T) *http.ServeMux {
	t.Helper()

	// Each test gets its own in-memory SQLite database (no cross-test bleed).
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatalf("openStore(:memory:): %v", err)
	}
	t.Cleanup(func() { st.close() })

	// Also reset the package-level in-memory lastStore.
	lastMu.Lock()
	lastStore = nil
	lastMu.Unlock()
	t.Cleanup(func() {
		lastMu.Lock()
		lastStore = nil
		lastMu.Unlock()
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("POST /api/v1/telemetry", makeTelemetryHandler(0, st))
	mux.HandleFunc("GET /api/v1/telemetry/last", makeLastHandler(st))
	mux.HandleFunc("GET /api/v1/telemetry/recent", makeRecentHandler(st))
	return mux
}

// validPayload returns a JSON body for a valid telemetry POST.
func validPayload(deviceID string) string {
	ts := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf(
		`{"device_id":%q,"timestamp":%q,"temperature_c":22.5,"pressure_hpa":1013.0,"humidity_pct":55.0,"status":"ok"}`,
		deviceID, ts,
	)
}

// TestLastBeforeAnyPost checks that GET /api/v1/telemetry/last returns 404
// with the expected error code before any telemetry has been received.
// Not marked t.Parallel(): shares package-level lastStore with other stateful tests.
func TestLastBeforeAnyPost(t *testing.T) {
	srv := httptest.NewServer(newTestMux(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/telemetry/last")
	if err != nil {
		t.Fatalf("GET /last: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "no_telemetry" {
		t.Fatalf("want error=no_telemetry, got %q", body["error"])
	}
}

// TestPostThenGetLast checks the happy path: POST valid telemetry → 202,
// then GET /last → 200 with matching device_id.
// Not marked t.Parallel(): shares package-level lastStore with other stateful tests.
func TestPostThenGetLast(t *testing.T) {
	srv := httptest.NewServer(newTestMux(t))
	defer srv.Close()

	// POST
	body := strings.NewReader(validPayload("test-device"))
	resp, err := http.Post(srv.URL+"/api/v1/telemetry", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 202, got %d: %s", resp.StatusCode, raw)
	}

	// GET /last — with DB the response is a telemetryRow (device_id at root).
	resp2, err := http.Get(srv.URL + "/api/v1/telemetry/last")
	if err != nil {
		t.Fatalf("GET /last: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		t.Fatalf("want 200, got %d: %s", resp2.StatusCode, raw)
	}

	var result struct {
		DeviceID   string `json:"device_id"`
		ReceivedAt int64  `json:"received_at_unix"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&result); err != nil {
		t.Fatalf("decode /last body: %v", err)
	}
	if result.DeviceID != "test-device" {
		t.Fatalf("want device_id=test-device, got %q", result.DeviceID)
	}
	if result.ReceivedAt <= 0 {
		t.Fatalf("want received_at_unix > 0, got %d", result.ReceivedAt)
	}
}

// TestRecentEndpoint checks GET /api/v1/telemetry/recent returns an array
// with the most-recent record first and respects the device_id filter.
// Not marked t.Parallel(): shares package-level lastStore with other stateful tests.
func TestRecentEndpoint(t *testing.T) {
	srv := httptest.NewServer(newTestMux(t))
	defer srv.Close()

	// POST two records for different devices.
	for _, id := range []string{"dev-a", "dev-b"} {
		body := strings.NewReader(validPayload(id))
		resp, err := http.Post(srv.URL+"/api/v1/telemetry", "application/json", body)
		if err != nil {
			t.Fatalf("POST %s: %v", id, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("want 202 for %s, got %d", id, resp.StatusCode)
		}
	}

	// GET /recent — no filter, should return both (newest first).
	resp, err := http.Get(srv.URL + "/api/v1/telemetry/recent?limit=10")
	if err != nil {
		t.Fatalf("GET /recent: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, raw)
	}

	var rows []struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode /recent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// Newest first: dev-b was posted last.
	if rows[0].DeviceID != "dev-b" {
		t.Fatalf("want newest device=dev-b, got %q", rows[0].DeviceID)
	}

	// GET /recent?device_id=dev-a — should return exactly 1 row.
	resp2, err := http.Get(srv.URL + "/api/v1/telemetry/recent?device_id=dev-a&limit=10")
	if err != nil {
		t.Fatalf("GET /recent?device_id=dev-a: %v", err)
	}
	defer resp2.Body.Close()
	var filtered []struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&filtered); err != nil {
		t.Fatalf("decode filtered /recent: %v", err)
	}
	if len(filtered) != 1 || filtered[0].DeviceID != "dev-a" {
		t.Fatalf("want 1 row for dev-a, got %v", filtered)
	}
}

// TestGaugeAfterPost checks that iiot_last_telemetry_timestamp_seconds is > 0
// after a successful POST, by reading directly from the default gatherer.
// Not marked t.Parallel(): shares package-level lastStore with other stateful tests.
func TestGaugeAfterPost(t *testing.T) {
	srv := httptest.NewServer(newTestMux(t))
	defer srv.Close()

	body := strings.NewReader(validPayload("gauge-device"))
	resp, err := http.Post(srv.URL+"/api/v1/telemetry", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	// Gather from the default registry (where promauto registered the gauge).
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	const name = "iiot_last_telemetry_timestamp_seconds"
	var found *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == name {
			found = mf
			break
		}
	}
	if found == nil {
		t.Fatalf("metric %s not found in gathered output", name)
	}
	if len(found.Metric) == 0 {
		t.Fatalf("metric %s has no samples", name)
	}
	val := found.Metric[0].GetGauge().GetValue()
	if val <= 0 {
		t.Fatalf("want %s > 0, got %v", name, val)
	}
}

// TestRouteLabelStability confirms routeLabel returns the expected stable
// strings for all known paths (prevents silent Prometheus cardinality drift).
func TestRouteLabelStability(t *testing.T) {
	t.Parallel()
	cases := []struct{ path, want string }{
		{"/healthz", "/healthz"},
		{"/api/v1/telemetry", "/api/v1/telemetry"},
		{"/api/v1/telemetry/last", "/api/v1/telemetry/last"},
		{"/api/v1/telemetry/recent", "/api/v1/telemetry/recent"},
		{"/unknown", "other"},
		{"/api/v1/telemetry/123", "other"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodGet, c.path, nil)
		got := routeLabel(req)
		if got != c.want {
			t.Errorf("routeLabel(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestConcurrentPostsNoRace fires N concurrent valid POSTs and then reads
// /last. Its primary purpose is to expose data races under -race; the exact
// "last" value is non-deterministic but must always be a valid entry.
// Not marked t.Parallel(): shares package-level lastStore with other stateful tests.
func TestConcurrentPostsNoRace(t *testing.T) {
	srv := httptest.NewServer(newTestMux(t))
	defer srv.Close()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("racer-%02d", i)
			body := bytes.NewBufferString(validPayload(deviceID))
			resp, err := http.Post(srv.URL+"/api/v1/telemetry", "application/json", body)
			if err != nil {
				return // server closed mid-test is acceptable
			}
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	resp, err := http.Get(srv.URL + "/api/v1/telemetry/last")
	if err != nil {
		t.Fatalf("GET /last after concurrent posts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	// With DB path, /last returns a telemetryRow (flat, not nested lastEntry).
	var result struct {
		DeviceID   string `json:"device_id"`
		ReceivedAt int64  `json:"received_at_unix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode /last: %v", err)
	}
	if !strings.HasPrefix(result.DeviceID, "racer-") {
		t.Fatalf("unexpected device_id %q", result.DeviceID)
	}
	if result.ReceivedAt <= 0 {
		t.Fatalf("want received_at_unix > 0, got %d", result.ReceivedAt)
	}
}
