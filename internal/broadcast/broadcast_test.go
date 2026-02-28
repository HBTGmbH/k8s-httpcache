package broadcast

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"k8s-httpcache/internal/telemetry"
	"k8s-httpcache/internal/watcher"
)

// errorReader is an io.Reader that always returns an error.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated read error")
}

// newTestServer creates a broadcast Server with default test timeouts.
func newTestServer() *Server {
	return New(Options{
		Addr:              ":0",
		ServerIdleTimeout: 120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ClientTimeout:     10 * time.Second,
		ClientIdleTimeout: 90 * time.Second,
		ShutdownTimeout:   5 * time.Second,
		Metrics:           telemetry.NewMetrics(prometheus.NewRegistry(), nil),
	})
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
		IP:   parts[0],
		Port: port,
	}
}

func TestNoFrontends(t *testing.T) {
	t.Parallel()
	s := newTestServer()

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

	s := newTestServer()
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

	s := newTestServer()
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

	s := newTestServer()
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

	s := newTestServer()
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
	var mu = &sync.Mutex{}

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

	s := newTestServer()
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

	s := newTestServer()
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

	s := newTestServer()
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

	s := newTestServer()
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

	// Drain with a short timeout — should timeout, not hang.
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
	s := newTestServer()
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
	s := newTestServer()

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

	s := newTestServer()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	s := newTestServer()
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

	s := newTestServer()
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
	s := newTestServer()
	s.SetFrontends([]watcher.Frontend{
		{Name: "pod-0", IP: "127.0.0.1", Port: 9999},
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
	s := newTestServer()
	s.draining.Store(true)
	s.SetFrontends([]watcher.Frontend{
		{Name: "pod-0", IP: "127.0.0.1", Port: 9999},
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	s := newTestServer()

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

	s := newTestServer()
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
