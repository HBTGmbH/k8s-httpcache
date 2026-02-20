// Package broadcast fans out HTTP requests to Varnish frontends.
package broadcast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"k8s-httpcache/internal/watcher"
)

// maxBodySize is the maximum response body size read from each pod.
const maxBodySize = 1 << 20 // 1 MiB

// PodResult holds the response from a single pod.
type PodResult struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// Options configures the broadcast server.
type Options struct {
	Addr              string        // listen address
	TargetPort        int32         // Varnish listen port to target for fan-out (overrides frontend port)
	ServerIdleTimeout time.Duration // max idle time for client keep-alive connections
	ReadHeaderTimeout time.Duration // max time to read request headers
	ClientTimeout     time.Duration // per-pod fan-out request timeout
	ClientIdleTimeout time.Duration // max idle time for connections to Varnish pods
	ShutdownTimeout   time.Duration // grace period for in-flight requests after drain
}

// Server fans out incoming HTTP requests to all known Varnish frontend pods
// and returns an aggregated JSON response.
type Server struct {
	srv             *http.Server
	client          *http.Client
	targetPort      int32
	shutdownTimeout time.Duration

	mu        sync.RWMutex
	frontends []watcher.Frontend

	draining     atomic.Bool
	conns        atomic.Int64
	connsDrained chan struct{}
	drainOnce    sync.Once
}

// New creates a broadcast server with the given options.
func New(opts Options) *Server {
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
		connsDrained:    make(chan struct{}),
	}
	s.srv = &http.Server{
		Addr:              opts.Addr,
		Handler:           s,
		ConnState:         s.connState,
		IdleTimeout:       opts.ServerIdleTimeout,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
	}
	return s
}

// connState tracks active client connections via the http.Server callback.
func (s *Server) connState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		s.conns.Add(1)
	case http.StateClosed, http.StateHijacked:
		if s.conns.Add(-1) <= 0 && s.draining.Load() {
			s.signalDrained()
		}
	}
}

func (s *Server) signalDrained() {
	s.drainOnce.Do(func() { close(s.connsDrained) })
}

// SetFrontends updates the list of frontend pods to broadcast to.
func (s *Server) SetFrontends(frontends []watcher.Frontend) {
	s.mu.Lock()
	s.frontends = frontends
	s.mu.Unlock()
}

// ServeHTTP fans out the incoming request to all frontends in parallel.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	frontends := s.frontends
	s.mu.RUnlock()

	if len(frontends) == 0 {
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
		w.Header().Set("Content-Type", "application/json")
		if s.draining.Load() {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		//nolint:errchkjson // best-effort error response; nothing to do if encoding fails
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to read request body"})
		return
	}

	type namedResult struct {
		name   string
		result PodResult
	}

	results := make(chan namedResult, len(frontends))
	var wg sync.WaitGroup

	for _, fe := range frontends {
		wg.Add(1)
		go func(fe watcher.Frontend) {
			defer wg.Done()
			nr := namedResult{name: fe.Name}
			nr.result = s.forward(r, fe, body)
			results <- nr
		}(fe)
	}

	wg.Wait()
	close(results)

	out := make(map[string]PodResult, len(frontends))
	for nr := range results {
		out[nr.name] = nr.result
	}

	w.Header().Set("Content-Type", "application/json")
	if s.draining.Load() {
		w.Header().Set("Connection", "close")
	}
	w.WriteHeader(http.StatusOK)
	//nolint:errchkjson // best-effort JSON response; nothing to do if encoding fails
	_ = json.NewEncoder(w).Encode(out)
}

// forward sends the request to a single frontend pod and returns the result.
func (s *Server) forward(origReq *http.Request, fe watcher.Frontend, body []byte) PodResult {
	port := fe.Port
	if s.targetPort > 0 {
		port = s.targetPort
	}
	url := "http://" + net.JoinHostPort(fe.IP, strconv.FormatInt(int64(port), 10)) + origReq.RequestURI

	ctx := origReq.Context()
	req, err := http.NewRequestWithContext(ctx, origReq.Method, url, nil)
	if err != nil {
		return PodResult{Status: 0, Body: fmt.Sprintf("request error: %v", err)}
	}

	// Copy headers from the original request.
	for k, vv := range origReq.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	if len(body) > 0 {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	resp, err := s.client.Do(req)
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

// ListenAndServe starts the broadcast HTTP server.
func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
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
	return s.srv.Shutdown(ctx)
}

// Shutdown gracefully shuts down the broadcast server without draining.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}
