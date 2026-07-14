package broadcast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HBTGmbH/k8s-httpcache/internal/telemetry"
	"github.com/HBTGmbH/k8s-httpcache/internal/watcher"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// errorReader is an [io.Reader] that always returns an error.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated read error")
}

// newTestServer creates a broadcast Server with default test timeouts. The
// fan-out client's idle keep-alive connections are closed on test cleanup so
// their reader goroutines don't trip the package's goleak check.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := New(Options{
		Addr:              ":0",
		ServerIdleTimeout: 120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ClientTimeout:     10 * time.Second,
		ClientIdleTimeout: 90 * time.Second,
		ShutdownTimeout:   5 * time.Second,
		Metrics:           telemetry.NewMetrics(prometheus.NewRegistry(), nil),
	})
	t.Cleanup(s.client.CloseIdleConnections)

	return s
}

// collectSeriesCount returns the number of metric series a collector exports.
func collectSeriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}

	return n
}

// TestBroadcastRequestsTotalMethodCardinalityBounded verifies that the
// broadcast request counter's "method" label is bounded regardless of client
// input. r.Method is client-controllable (net/http accepts any RFC 7230 token),
// so using it verbatim as a label lets any client grow Prometheus cardinality -
// and the registry's memory - without bound.
func TestBroadcastRequestsTotalMethodCardinalityBounded(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	// No SetFrontends → every request hits the no-frontends 503 branch.
	for i := range 1000 {
		req := httptest.NewRequest(fmt.Sprintf("M%04d", i), "/purge", http.NoBody)
		s.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := collectSeriesCount(s.metrics.BroadcastRequestsTotal); n > 16 {
		t.Fatalf("BroadcastRequestsTotal has %d series after 1000 distinct methods; "+
			"the method label is unbounded (client-controllable cardinality)", n)
	}
}

// frontendFromServer returns a watcher.Frontend pointing at ts.
func frontendFromServer(name string, ts *httptest.Server) watcher.Frontend {
	// ts.URL is like "http://127.0.0.1:PORT"
	host := ts.URL[len("http://"):]
	parts := strings.SplitN(host, ":", 2)
	var port int32
	for _, c := range parts[1] {
		port = port*10 + (c - '0')
	}

	return watcher.Frontend{
		Name: name,
		Host: parts[0],
		Port: port,
	}
}

func TestNoFrontends(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	var body map[string]string

	err := json.NewDecoder(rec.Body).Decode(&body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("expected error in response body")
	}
}

func TestSingleFrontend(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("purged"))
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results map[string]PodResult

	err := json.NewDecoder(rec.Body).Decode(&results)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	r, ok := results["pod-0"]
	if !ok {
		t.Fatal("missing pod-0 in results")
	}
	if r.Status != 200 {
		t.Fatalf("expected status 200, got %d", r.Status)
	}
	if r.Body != "purged" {
		t.Fatalf("expected body %q, got %q", "purged", r.Body)
	}
}

func TestMultipleFrontends(t *testing.T) {
	t.Parallel()
	makeBackend := func(resp string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(resp))
		}))
	}

	b0 := makeBackend("ok-0")
	defer b0.Close()
	b1 := makeBackend("ok-1")
	defer b1.Close()
	b2 := makeBackend("ok-2")
	defer b2.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{
		frontendFromServer("pod-0", b0),
		frontendFromServer("pod-1", b1),
		frontendFromServer("pod-2", b2),
	})

	req := httptest.NewRequest(http.MethodGet, "/purge/bar", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results map[string]PodResult

	err := json.NewDecoder(rec.Body).Decode(&results)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for _, name := range []string{"pod-0", "pod-1", "pod-2"} {
		r, ok := results[name]
		if !ok {
			t.Fatalf("missing %s in results", name)
		}
		if r.Status != 200 {
			t.Fatalf("%s: expected status 200, got %d", name, r.Status)
		}
	}
}

func TestFrontendDown(t *testing.T) {
	t.Parallel()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer healthy.Close()

	// Create and immediately close a server to get a dead endpoint.
	dead := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	deadFe := frontendFromServer("pod-dead", dead)
	dead.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{
		frontendFromServer("pod-ok", healthy),
		deadFe,
	})

	req := httptest.NewRequest(http.MethodGet, "/purge/x", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results map[string]PodResult

	err := json.NewDecoder(rec.Body).Decode(&results)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	okResult := results["pod-ok"]
	if okResult.Status != 200 {
		t.Fatalf("pod-ok: expected status 200, got %d", okResult.Status)
	}

	deadResult := results["pod-dead"]
	if deadResult.Status != 0 {
		t.Fatalf("pod-dead: expected status 0, got %d", deadResult.Status)
	}
	if !strings.Contains(deadResult.Body, "request error:") {
		t.Fatalf("pod-dead: expected error message, got %q", deadResult.Body)
	}
}

func TestClassifyOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		want   string
	}{
		{0, outcomeTransportError},
		{200, outcomeOK},
		{204, outcomeOK},
		{301, outcomeOK},
		{399, outcomeOK},
		{400, outcomeHTTPError},
		{404, outcomeHTTPError},
		{500, outcomeHTTPError},
		{503, outcomeHTTPError},
	}
	for _, c := range cases {
		if got := classifyOutcome(c.status); got != c.want {
			t.Errorf("classifyOutcome(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

// TestBroadcastPodResultMetrics verifies the per-pod fan-out telemetry: one
// duration observation per pod, and one increment per pod bucketed into the
// fixed ok/http_error/transport_error outcome set.
func TestBroadcastPodResultMetrics(t *testing.T) {
	t.Parallel()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer serverErr.Close()

	// Create and immediately close a server to get a dead (transport-error) endpoint.
	dead := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	deadFe := frontendFromServer("pod-dead", dead)
	dead.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{
		frontendFromServer("pod-ok", ok),
		frontendFromServer("pod-5xx", serverErr),
		deadFe,
	})

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PURGE", "/x", http.NoBody))

	// One duration observation per pod.
	var dm dto.Metric
	err := s.metrics.BroadcastPodDurationSeconds.Write(&dm)
	if err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 3 {
		t.Errorf("broadcast_pod_duration_seconds sample count = %d, want 3", got)
	}

	// Exactly three outcome series exist (bounded regardless of fleet size).
	// Checked before reading individual series so the reads can't inflate it.
	if n := collectSeriesCount(s.metrics.BroadcastPodResultsTotal); n != 3 {
		t.Fatalf("broadcast_pod_results_total has %d series, want 3 (ok/http_error/transport_error)", n)
	}
	for _, outcome := range []string{outcomeOK, outcomeHTTPError, outcomeTransportError} {
		if got := podResultCount(t, s, outcome); got != 1 {
			t.Errorf("broadcast_pod_results_total{outcome=%q} = %v, want 1", outcome, got)
		}
	}
}

// podResultCount reads the value of a single broadcast_pod_results_total series.
func podResultCount(t *testing.T, s *Server, outcome string) float64 {
	t.Helper()
	var m dto.Metric
	err := s.metrics.BroadcastPodResultsTotal.WithLabelValues(outcome).Write(&m)
	if err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

func TestMethodAndHeadersPreserved(t *testing.T) {
	t.Parallel()
	var gotMethod string
	var gotHeaders http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	req := httptest.NewRequest("PURGE", "/cache/item", http.NoBody)
	req.Header.Set("X-Custom", "test-value")
	req.Header.Set("X-Another", "another-value")

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotMethod != "PURGE" {
		t.Fatalf("expected method PURGE, got %s", gotMethod)
	}
	if gotHeaders.Get("X-Custom") != "test-value" {
		t.Fatalf("expected X-Custom header, got %q", gotHeaders.Get("X-Custom"))
	}
	if gotHeaders.Get("X-Another") != "another-value" {
		t.Fatalf("expected X-Another header, got %q", gotHeaders.Get("X-Another"))
	}
}

func TestRequestBodyForwarded(t *testing.T) {
	t.Parallel()
	var bodies []string
	mu := &sync.Mutex{}

	makeBackend := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))
	}

	b0 := makeBackend()
	defer b0.Close()
	b1 := makeBackend()
	defer b1.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{
		frontendFromServer("pod-0", b0),
		frontendFromServer("pod-1", b1),
	})

	req := httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader("purge-payload"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected 2 bodies, got %d", len(bodies))
	}
	for i, b := range bodies {
		if b != "purge-payload" {
			t.Fatalf("backend %d: expected body %q, got %q", i, "purge-payload", b)
		}
	}
}

func TestTargetPortOverride(t *testing.T) {
	t.Parallel()
	// Start a backend on a specific port. The frontend's Port field will
	// differ from the TargetPort, verifying the override works.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	}))
	defer backend.Close()

	fe := frontendFromServer("pod-0", backend)
	actualPort := fe.Port

	// Set the frontend port to something wrong; the TargetPort should override it.
	fe.Port = 1 // unreachable

	s := New(Options{
		Addr:              ":0",
		TargetPort:        actualPort,
		ServerIdleTimeout: 120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ClientTimeout:     10 * time.Second,
		ClientIdleTimeout: 90 * time.Second,
		ShutdownTimeout:   5 * time.Second,
		Metrics:           telemetry.NewMetrics(prometheus.NewRegistry(), nil),
	})
	s.SetFrontends([]watcher.Frontend{fe})

	req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results map[string]PodResult

	err := json.NewDecoder(rec.Body).Decode(&results)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	r := results["pod-0"]
	if r.Status != 200 || r.Body != "reached" {
		t.Fatalf("expected status=200 body=reached, got status=%d body=%q", r.Status, r.Body)
	}
}

func TestDrainingConnectionClose(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	// Before draining: no Connection: close header.
	req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Header().Get("Connection") == "close" {
		t.Fatal("expected no Connection: close before draining")
	}

	// Set draining.
	s.draining.Store(true)

	// After draining: Connection: close header must be present.
	req = httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Header().Get("Connection") != "close" {
		t.Fatalf("expected Connection: close during drain, got %q", rec.Header().Get("Connection"))
	}
}

func TestDrainWaitsForConnections(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	// Start listening on a real port so ConnState fires.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.srv.Serve(ln) }()

	addr := ln.Addr().String()

	// Use a client that closes connections immediately (no keep-alive)
	// so the server sees StateClosed promptly.
	cl := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	resp, err := cl.Get("http://" + addr + "/purge/foo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Give the server a moment to process the StateClosed callback.
	time.Sleep(50 * time.Millisecond)

	// Drain should complete quickly since the connection is already closed.
	done := make(chan error, 1)
	go func() {
		done <- s.Drain(5 * time.Second)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not complete in time")
	}
}

func TestDrainTimeoutWithHeldConnection(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.srv.Serve(ln) }()

	addr := ln.Addr().String()

	// Open a raw TCP connection and keep it open (never close).
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send a minimal HTTP request to move the conn past StateNew.
	_, err = conn.Write([]byte("GET /purge/foo HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read the response but keep the connection open (keep-alive).
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(buf)

	// Drain with a short timeout - should timeout, not hang.
	drainTimeout := 200 * time.Millisecond
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- s.Drain(drainTimeout)
	}()

	select {
	case <-done:
		elapsed := time.Since(start)
		if elapsed < drainTimeout {
			t.Fatalf("drain returned too quickly: %v", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("drain did not complete after timeout")
	}
}

func TestNoFrontendsDraining(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	s.draining.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if rec.Header().Get("Connection") != "close" {
		t.Fatal("expected Connection: close during drain with no frontends")
	}
}

func TestDrainNoConnections(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)

	// Drain with no connections should return immediately.
	done := make(chan error, 1)
	go func() {
		done <- s.Drain(5 * time.Second)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain with no connections should complete quickly")
	}
}

func TestShutdown(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.srv.Serve(ln) }()

	addr := ln.Addr().String()

	// Verify the server is working.
	resp, err := http.Get("http://" + addr + "/purge/foo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Shutdown the server.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = s.Shutdown(ctx)
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Subsequent requests should fail.
	resp, err = http.Get("http://" + addr + "/purge/foo")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected error after shutdown, got nil")
	}
}

func TestMaxBodySizeTruncation(t *testing.T) {
	t.Parallel()
	// Create a backend that returns more than maxBodySize (1 MiB).
	bigBody := strings.Repeat("X", 2<<20) // 2 MiB
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	req := httptest.NewRequest(http.MethodGet, "/big", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results map[string]PodResult

	err := json.NewDecoder(rec.Body).Decode(&results)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	r := results["pod-0"]
	if r.Status != 200 {
		t.Fatalf("expected status 200, got %d", r.Status)
	}
	// Body should be truncated to maxBodySize (1 MiB = 1048576 bytes).
	if len(r.Body) != maxBodySize {
		t.Fatalf("expected body length %d, got %d", maxBodySize, len(r.Body))
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	req := httptest.NewRequest(http.MethodGet, "/redirect-me", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results map[string]PodResult

	err := json.NewDecoder(rec.Body).Decode(&results)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	r := results["pod-0"]
	if r.Status != http.StatusFound {
		t.Fatalf("expected pod result status 302, got %d", r.Status)
	}
}

func TestRequestBodyReadError(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{
		{Name: "pod-0", Host: "127.0.0.1", Port: 9999},
	})

	req := httptest.NewRequest(http.MethodPost, "/purge/foo", &errorReader{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	var body map[string]string

	err := json.NewDecoder(rec.Body).Decode(&body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body["error"], "failed to read request body") {
		t.Fatalf("expected body read error, got %q", body["error"])
	}
}

func TestRequestBodyReadErrorDraining(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	s.draining.Store(true)
	s.SetFrontends([]watcher.Frontend{
		{Name: "pod-0", Host: "127.0.0.1", Port: 9999},
	})

	req := httptest.NewRequest(http.MethodPost, "/purge/foo", &errorReader{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if rec.Header().Get("Connection") != "close" {
		t.Fatal("expected Connection: close during drain on body read error")
	}
}

func TestListenAndServe(t *testing.T) {
	t.Parallel()
	s := New(Options{
		Addr:              "127.0.0.1:0",
		ServerIdleTimeout: 120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ClientTimeout:     10 * time.Second,
		ClientIdleTimeout: 90 * time.Second,
		ShutdownTimeout:   5 * time.Second,
		Metrics:           telemetry.NewMetrics(prometheus.NewRegistry(), nil),
	})

	// ListenAndServe blocks, so start it in a goroutine and then shut down.
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ListenAndServe()
	}()

	// Give the server a moment to start.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := s.Shutdown(ctx)
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	err = <-errCh
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("ListenAndServe: %v", err)
	}
}

func TestConcurrentSetFrontends(t *testing.T) {
	t.Parallel()
	b0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-0"))
	}))
	defer b0.Close()

	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-1"))
	}))
	defer b1.Close()

	s := newTestServer(t)

	frontendSets := [][]watcher.Frontend{
		{frontendFromServer("pod-0", b0)},
		{frontendFromServer("pod-1", b1)},
		{frontendFromServer("pod-0", b0), frontendFromServer("pod-1", b1)},
		{},
	}

	var wg sync.WaitGroup
	// Concurrently update frontends.
	for i := range 20 {
		wg.Go(func() {
			s.SetFrontends(frontendSets[i%len(frontendSets)])
		})
	}

	// Concurrently make requests.
	for range 20 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			// Status must be 200 (frontends present) or 503 (no frontends).
			if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
				t.Errorf("unexpected status %d, want 200 or 503", rec.Code)
			}
			// Response must always be valid JSON.
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var raw json.RawMessage
			err := json.NewDecoder(rec.Body).Decode(&raw)
			if err != nil {
				t.Errorf("response is not valid JSON: %v", err)
			}
		})
	}

	wg.Wait()
}

func TestForwardResponseBodyReadError(t *testing.T) {
	t.Parallel()

	// Create a backend that declares a large Content-Length but closes
	// the connection after sending only a few bytes, causing io.ReadAll
	// to return an unexpected EOF error.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "999999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))

		// Hijack and close the connection immediately.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	var results map[string]PodResult

	err := json.NewDecoder(rec.Body).Decode(&results)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	r, ok := results["pod-0"]
	if !ok {
		t.Fatal("missing pod-0 in results")
	}

	if !strings.Contains(r.Body, "read error:") {
		t.Errorf("expected 'read error:' in body, got %q", r.Body)
	}

	// The response status (200) was received before the body read failed, so
	// it is preserved in the client-facing result and the outcome is bucketed
	// by that status - ok, not transport_error. See classifyOutcome.
	if r.Status != http.StatusOK {
		t.Errorf("read-error result Status = %d, want 200 (status received before the body read failed)", r.Status)
	}
	if n := collectSeriesCount(s.metrics.BroadcastPodResultsTotal); n != 1 {
		t.Fatalf("broadcast_pod_results_total has %d series, want 1 (only ok)", n)
	}
	if got := podResultCount(t, s, outcomeOK); got != 1 {
		t.Errorf("broadcast_pod_results_total{outcome=ok} = %v, want 1 (read error after 200 is bucketed by status)", got)
	}
}

func TestNewPanicsOnNilMetrics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil Metrics")
		}
	}()
	New(Options{
		Addr:              ":0",
		ServerIdleTimeout: 1 * time.Second,
		ReadHeaderTimeout: 1 * time.Second,
		ClientTimeout:     1 * time.Second,
		ClientIdleTimeout: 1 * time.Second,
		ShutdownTimeout:   1 * time.Second,
		Metrics:           nil,
	})
}

func TestNewPropagatesServerTimeouts(t *testing.T) {
	t.Parallel()
	s := New(Options{
		Addr:              ":0",
		ServerIdleTimeout: 120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		ClientTimeout:     3 * time.Second,
		ClientIdleTimeout: 4 * time.Second,
		ShutdownTimeout:   5 * time.Second,
		Metrics:           telemetry.NewMetrics(prometheus.NewRegistry(), nil),
	})

	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", s.srv.ReadHeaderTimeout, 10 * time.Second},
		{"ReadTimeout", s.srv.ReadTimeout, 15 * time.Second},
		{"WriteTimeout", s.srv.WriteTimeout, 30 * time.Second},
		{"IdleTimeout", s.srv.IdleTimeout, 120 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("srv.%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestForwardPreservesHostHeader(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		gotHost string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost = r.Host
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", backend)})

	// PURGE-style request: VCL on each pod matches on req.http.host, so the
	// client's original Host must reach the pod - not the pod's ip:port.
	// (Go promotes the incoming Host header to Request.Host and removes it
	// from the Header map, so copying headers alone loses it.)
	req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotHost != "example.com" {
		t.Fatalf("forwarded Host = %q, want %q (purges keyed by host would miss)", gotHost, "example.com")
	}
}

// TestDrainRacesNewConnections races a burst of new client connections against a
// concurrent Drain. New connections keep arriving while Drain flips the draining
// flag, samples conns.Load(), and waits on connsDrained - exactly the window
// guarded by the conns atomic, drainOnce, and the connsDrained channel. Run under
// -race it asserts Drain still completes (no lost wake-up, no hang) and the
// connection counter balances back to zero.
func TestDrainRacesNewConnections(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.srv.Serve(ln) }()
	addr := ln.Addr().String()

	// Two fake upstream pods so the fan-out has real targets.
	newPod := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	p0, p1 := newPod(), newPod()
	defer p0.Close()
	defer p1.Close()
	s.SetFrontends([]watcher.Frontend{
		frontendFromServer("pod-0", p0),
		frontendFromServer("pod-1", p1),
	})

	// Fire continuous client requests, each on a fresh connection
	// (DisableKeepAlives), so the server sees rapid StateNew→StateClosed
	// transitions racing the drain. Clients ignore errors - Drain's Shutdown
	// makes later requests fail, which is expected.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, reqErr := cl.Get("http://" + addr + "/purge/foo")
				if reqErr == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}
		})
	}

	// Let connections build up in flight, then drain while they keep arriving.
	time.Sleep(75 * time.Millisecond)
	drainErr := make(chan error, 1)
	go func() { drainErr <- s.Drain(5 * time.Second) }()

	// Keep racing new connections against the drain for a moment, then stop the
	// clients so conns can fall to zero and signalDrained can fire.
	time.Sleep(75 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Drain must complete (not hang) and report success.
	select {
	case drainResult := <-drainErr:
		if drainResult != nil {
			t.Fatalf("Drain returned error: %v", drainResult)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Drain did not return; possible lost wake-up under the connection race")
	}

	// Connection accounting must balance back to zero (no double-count or negative
	// drift from the StateNew/StateClosed race). Poll briefly: the final
	// StateClosed callbacks may trail Drain's return by a moment.
	deadline := time.Now().Add(2 * time.Second)
	for s.conns.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("conns = %d after drain, want 0", s.conns.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Idempotent safety-net shutdown (Drain already called srv.Shutdown).
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

// TestServeHTTPRacesSetFrontends races SetFrontends churn against concurrent
// ServeHTTP fan-out: the fan-out captures the frontend slice under s.mu.RLock
// while the churner replaces it under s.mu.Lock. Run under -race it validates the
// RWMutex and the immutable-snapshot design (SetFrontends replaces, never
// mutates), and asserts every response is a valid 200 (frontends present) or 503
// (empty set) - never a panic or corrupt status.
func TestServeHTTPRacesSetFrontends(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)

	newPod := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	p0, p1 := newPod(), newPod()
	defer p0.Close()
	defer p1.Close()

	two := []watcher.Frontend{frontendFromServer("pod-0", p0), frontendFromServer("pod-1", p1)}
	one := []watcher.Frontend{frontendFromServer("pod-0", p0)}
	var empty []watcher.Frontend
	s.SetFrontends(two)

	var wg sync.WaitGroup
	var mu sync.Mutex
	badStatus := 0

	// Churner: cycle the frontend list under concurrent fan-out.
	wg.Go(func() {
		sets := [][]watcher.Frontend{two, one, empty}
		for i := range 3000 {
			s.SetFrontends(sets[i%3])
		}
	})

	// Requesters: fan-out concurrently; every response must be 200 or 503.
	for range 8 {
		wg.Go(func() {
			for range 300 {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/purge/foo", http.NoBody)
				s.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
					mu.Lock()
					if badStatus == 0 {
						badStatus = rec.Code
					}
					mu.Unlock()

					return
				}
			}
		})
	}

	wg.Wait()
	if badStatus != 0 {
		t.Fatalf("unexpected response status under SetFrontends race: %d", badStatus)
	}
}

// TestRequestBodyTooLargeRejected verifies the broadcast request body is
// capped: bodies are held in memory for the fan-out, so an uncapped read
// would let a single request exhaust memory.
func TestRequestBodyTooLargeRejected(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{{Name: "pod-0", Host: "127.0.0.1", Port: 1}})

	req := httptest.NewRequest("PURGE", "/", strings.NewReader(strings.Repeat("x", maxBodySize+1)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for an oversized body, got %d", rec.Code)
	}
}

// TestForwardStripsHopByHopHeaders verifies the fan-out drops hop-by-hop
// headers (RFC 9110 §7.6.1) plus any header named in the client's Connection
// header, while end-to-end headers pass through. A copied "Connection: close"
// would otherwise force a fresh TCP connection to every pod per broadcast.
func TestForwardStripsHopByHopHeaders(t *testing.T) {
	t.Parallel()

	gotCh := make(chan http.Header, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCh <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := newTestServer(t)
	s.SetFrontends([]watcher.Frontend{frontendFromServer("pod-0", ts)})

	req := httptest.NewRequest("PURGE", "/x", http.NoBody)
	req.Header.Set("X-Keep", "1")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Authorization", "creds")
	req.Header.Set("Connection", "x-hop")
	req.Header.Set("X-Hop", "gone")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	var got http.Header
	select {
	case got = <-gotCh:
	case <-time.After(5 * time.Second):
		t.Fatal("pod never received the fan-out request")
	}

	if got.Get("X-Keep") != "1" {
		t.Error("end-to-end header X-Keep missing at the pod")
	}
	for _, h := range []string{"Keep-Alive", "Proxy-Authorization", "X-Hop"} {
		if got.Get(h) != "" {
			t.Errorf("hop-by-hop header %s forwarded to the pod: %q", h, got.Get(h))
		}
	}
}
