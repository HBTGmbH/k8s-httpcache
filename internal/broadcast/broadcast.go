// Package broadcast fans out HTTP requests to Varnish frontends.
package broadcast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"k8s-httpcache/internal/telemetry"
	"k8s-httpcache/internal/watcher"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// maxBodySize is the maximum response body size read from each pod.
const maxBodySize = 1 << 20 // 1 MiB

// knownMethods is the set of HTTP methods recorded verbatim in the request
// metric's "method" label. Any other method is collapsed to "other": net/http
// accepts arbitrary RFC 7230 method tokens from clients, so using r.Method
// verbatim as a label would let any client grow the metric's cardinality (and
// the registry's memory) without bound.
var knownMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodConnect: {},
	http.MethodOptions: {}, http.MethodTrace: {}, "PURGE": {}, "BAN": {},
}

// normalizeMethod bounds the request metric's "method" label to a fixed set,
// mapping any unrecognised (client-controllable) method to "other".
func normalizeMethod(method string) string {
	if _, ok := knownMethods[method]; ok {
		return method
	}

	return "other"
}

// Outcome label values for the broadcast_pod_results_total metric. The set is
// fixed (three values) so the metric's cardinality is bounded regardless of how
// the fan-out fails.
const (
	outcomeOK             = "ok"              // pod returned an HTTP status < 400
	outcomeHTTPError      = "http_error"      // pod returned an HTTP status >= 400
	outcomeTransportError = "transport_error" // no HTTP response received (connection or timeout error)
)

// classifyOutcome buckets a per-pod fan-out result by its status code. A zero
// status means forward never obtained an HTTP response (a transport-level
// failure such as a connection error or timeout — see forward). A body-read
// error that occurs after a response was received keeps that response's status
// code, so it is bucketed by that status (ok/http_error), not transport_error:
// the request did reach the pod and was answered.
func classifyOutcome(status int) string {
	switch {
	case status == 0:
		return outcomeTransportError
	case status >= http.StatusBadRequest:
		return outcomeHTTPError
	default:
		return outcomeOK
	}
}

// PodResult holds the response from a single pod.
type PodResult struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// Options configures the broadcast server.
type Options struct {
	Addr              string             // listen address
	TargetPort        int32              // Varnish listen port to target for fan-out (overrides frontend port)
	ServerIdleTimeout time.Duration      // max idle time for client keep-alive connections
	ReadHeaderTimeout time.Duration      // max time to read request headers
	ReadTimeout       time.Duration      // max time to read the whole request (headers + body); bounds slow-body clients
	WriteTimeout      time.Duration      // max time to write the response; bounds slow-read clients
	ClientTimeout     time.Duration      // per-pod fan-out request timeout
	ClientIdleTimeout time.Duration      // max idle time for connections to Varnish pods
	ShutdownTimeout   time.Duration      // grace period for in-flight requests after drain
	Metrics           *telemetry.Metrics // Prometheus metrics
}

// Server fans out incoming HTTP requests to all known Varnish frontend pods
// and returns an aggregated JSON response.
type Server struct {
	srv             *http.Server
	client          *http.Client
	targetPort      int32
	shutdownTimeout time.Duration
	metrics         *telemetry.Metrics

	mu        sync.RWMutex
	frontends []watcher.Frontend

	draining     atomic.Bool
	conns        atomic.Int64
	connsDrained chan struct{}
	drainOnce    sync.Once
}

// New creates a broadcast server with the given options.
// Panics if opts.Metrics is nil.
func New(opts Options) *Server { //nolint:gocritic // hugeParam: Options is copied once at startup; the copy cost is irrelevant and a value receiver keeps the call site clean
	if opts.Metrics == nil {
		panic("broadcast: Options.Metrics must not be nil")
	}
	s := &Server{
		client: &http.Client{
			Timeout: opts.ClientTimeout,
			Transport: &http.Transport{
				IdleConnTimeout: opts.ClientIdleTimeout,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		targetPort:      opts.TargetPort,
		shutdownTimeout: opts.ShutdownTimeout,
		metrics:         opts.Metrics,
		connsDrained:    make(chan struct{}),
	}
	s.srv = &http.Server{
		Addr:              opts.Addr,
		Handler:           s,
		ConnState:         s.connState,
		IdleTimeout:       opts.ServerIdleTimeout,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
		ReadTimeout:       opts.ReadTimeout,
		WriteTimeout:      opts.WriteTimeout,
	}

	return s
}

// SetFrontends updates the list of frontend pods to broadcast to.
func (s *Server) SetFrontends(frontends []watcher.Frontend) {
	s.mu.Lock()
	s.frontends = frontends
	s.mu.Unlock()
}

// ServeHTTP fans out the incoming request to all frontends in parallel.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Bound the metric's method label; r.Method is client-controllable. The
	// real method is still forwarded to pods verbatim (see forward).
	method := normalizeMethod(r.Method)

	s.mu.RLock()
	frontends := s.frontends
	s.mu.RUnlock()

	if len(frontends) == 0 {
		s.metrics.BroadcastRequestsTotal.WithLabelValues(method, "503").Inc()
		w.Header().Set("Content-Type", "application/json")
		if s.draining.Load() {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		//nolint:errchkjson // best-effort error response; nothing to do if encoding fails
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no frontends available"})

		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.metrics.BroadcastRequestsTotal.WithLabelValues(method, "400").Inc()
		w.Header().Set("Content-Type", "application/json")
		if s.draining.Load() {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		//nolint:errchkjson // best-effort error response; nothing to do if encoding fails
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to read request body"})

		return
	}

	fanoutStart := time.Now()

	type namedResult struct {
		name   string
		result PodResult
	}

	results := make(chan namedResult, len(frontends))
	var wg sync.WaitGroup

	s.metrics.BroadcastFanoutTargets.Set(float64(len(frontends)))

	for _, fe := range frontends {
		wg.Go(func() {
			start := time.Now()
			nr := namedResult{name: fe.Name}
			nr.result = s.forward(r, &fe, body)
			// Per-pod fan-out telemetry: latency distribution and a
			// bounded-cardinality outcome bucket. Prometheus metric methods are
			// safe for concurrent use, so recording here (one goroutine per pod)
			// needs no extra synchronisation.
			s.metrics.BroadcastPodDurationSeconds.Observe(time.Since(start).Seconds())
			s.metrics.BroadcastPodResultsTotal.WithLabelValues(classifyOutcome(nr.result.Status)).Inc()
			results <- nr
		})
	}

	wg.Wait()
	close(results)

	out := make(map[string]PodResult, len(frontends))
	for nr := range results {
		out[nr.name] = nr.result
	}

	s.metrics.BroadcastDurationSeconds.Observe(time.Since(fanoutStart).Seconds())

	s.metrics.BroadcastRequestsTotal.WithLabelValues(method, "200").Inc()
	w.Header().Set("Content-Type", "application/json")
	if s.draining.Load() {
		w.Header().Set("Connection", "close")
	}
	w.WriteHeader(http.StatusOK)
	//nolint:errchkjson // best-effort JSON response; nothing to do if encoding fails
	_ = json.NewEncoder(w).Encode(out)
}

// ListenAndServe starts the broadcast HTTP server.
func (s *Server) ListenAndServe() error {
	err := s.srv.ListenAndServe()
	if err != nil {
		return fmt.Errorf("broadcast listen: %w", err)
	}

	return nil
}

// Serve serves on an already-bound listener. The caller pre-binds with
// [net.Listen] so a port conflict fails fast (before varnishd starts) rather
// than from inside the serving goroutine.
func (s *Server) Serve(ln net.Listener) error {
	err := s.srv.Serve(ln)
	if err != nil {
		return fmt.Errorf("broadcast serve: %w", err)
	}

	return nil
}

// Drain initiates a graceful drain of the broadcast server. It sets the
// draining flag (causing responses to include Connection: close), then waits
// up to timeout for all client connections to close before shutting down.
func (s *Server) Drain(timeout time.Duration) error {
	s.draining.Store(true)
	slog.Info("draining broadcast connections", "timeout", timeout)

	if s.conns.Load() <= 0 {
		s.signalDrained()
	}

	select {
	case <-s.connsDrained:
		slog.Info("all broadcast connections drained")
	case <-time.After(timeout):
		slog.Warn("broadcast drain timeout reached", "remaining", s.conns.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	err := s.srv.Shutdown(ctx)
	if err != nil {
		return fmt.Errorf("broadcast shutdown after drain: %w", err)
	}

	return nil
}

// Shutdown gracefully shuts down the broadcast server without draining.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil {
		return fmt.Errorf("broadcast shutdown: %w", err)
	}

	return nil
}

// connState tracks active client connections via the [http.Server] callback.
func (s *Server) connState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		s.conns.Add(1)
	case http.StateClosed, http.StateHijacked:
		if s.conns.Add(-1) <= 0 && s.draining.Load() {
			s.signalDrained()
		}
	default:
		// StateActive, StateIdle — no action needed.
	}
}

func (s *Server) signalDrained() {
	s.drainOnce.Do(func() { close(s.connsDrained) })
}

// forward sends the request to a single frontend pod and returns the result.
func (s *Server) forward(origReq *http.Request, fe *watcher.Frontend, body []byte) PodResult {
	port := fe.Port
	if s.targetPort > 0 {
		port = s.targetPort
	}
	target := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(fe.Host, strconv.FormatInt(int64(port), 10)),
		Path:     origReq.URL.Path,
		RawPath:  origReq.URL.RawPath,
		RawQuery: origReq.URL.RawQuery,
	}

	ctx := origReq.Context()
	req, err := http.NewRequestWithContext(ctx, origReq.Method, target.String(), http.NoBody)
	if err != nil {
		return PodResult{Status: 0, Body: fmt.Sprintf("request error: %v", err)}
	}

	// Copy headers from the original request.
	for k, vv := range origReq.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	// The incoming Host header is promoted to origReq.Host and removed from
	// the Header map, so it is not covered by the copy above. Preserve it:
	// Varnish PURGE/BAN VCL matches on req.http.host, and the pod's ip:port
	// (the default otherwise) would never match cached objects.
	req.Host = origReq.Host

	if len(body) > 0 {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	resp, err := s.client.Do(req) //nolint:gosec // G704: URL is constructed from trusted peer pod IPs via Kubernetes EndpointSlices.
	if err != nil {
		return PodResult{Status: 0, Body: fmt.Sprintf("request error: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return PodResult{Status: resp.StatusCode, Body: fmt.Sprintf("read error: %v", err)}
	}

	return PodResult{Status: resp.StatusCode, Body: string(respBody)}
}
