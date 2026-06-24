// Package main is a Kubernetes-native HTTP caching proxy built on Varnish.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"k8s-httpcache/internal/broadcast"
	"k8s-httpcache/internal/config"
	"k8s-httpcache/internal/redact"
	"k8s-httpcache/internal/renderer"
	"k8s-httpcache/internal/telemetry"
	"k8s-httpcache/internal/varnish"
	"k8s-httpcache/internal/watcher"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
)

// version is set at build time via -ldflags.
var version = "dev"

const (
	// drainBackendName is the VCL backend name used internally for graceful
	// connection draining. Users do not configure this — it is auto-injected
	// into the rendered VCL when --drain is enabled.
	drainBackendName = "drain_flag"

	// podKind is the Kubernetes object kind for the Pod resource, used when
	// constructing event-recorder object references.
	podKind = "Pod"
)

// vclRenderer abstracts VCL template operations (satisfied by *renderer.Renderer).
type vclRenderer interface {
	Reload() error
	Render(frontends []watcher.Frontend, backends map[string]renderer.BackendGroup, values, secrets map[string]map[string]any) (string, error)
	RenderToFile(frontends []watcher.Frontend, backends map[string]renderer.BackendGroup, values, secrets map[string]map[string]any) (string, error)
	Rollback()
}

// varnishProcess abstracts varnishd lifecycle (satisfied by *varnish.Manager).
type varnishProcess interface {
	Reload(vclPath string) error
	Done() <-chan struct{}
	Err() error
	ForwardSignal(sig os.Signal)
	MarkBackendSick(name string) error
	ActiveSessions() (uint64, error)
	LoadCert(name string, cert, key, ca []byte) error
	TLSSupported() bool
}

// broadcaster abstracts broadcast server operations (satisfied by *broadcast.Server).
type broadcaster interface {
	SetFrontends(frontends []watcher.Frontend)
	Drain(timeout time.Duration) error
}

// debounceState tracks the timer and deadline for one debounce group.
type debounceState struct {
	timer      *time.Timer
	deadline   time.Time
	firstEvent time.Time // wall-clock time of the first event in the current burst
	capped     bool      // true when debounce-max forced a shorter timer
}

// reset stops any pending timer and clears the group back to its zero-burst
// state. Called after a group fires so the next event starts a fresh burst.
func (s *debounceState) reset() {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = nil
	s.deadline = time.Time{}
	s.firstEvent = time.Time{}
	s.capped = false
}

// resetDebounce stops the current timer and starts a new one,
// capping the duration at the deadline when debounceMax is active.
func resetDebounce(s *debounceState, debounce, debounceMax time.Duration) {
	if s.firstEvent.IsZero() {
		s.firstEvent = time.Now()
	}
	if s.deadline.IsZero() && debounceMax > 0 {
		s.deadline = time.Now().Add(debounceMax)
	}
	d := debounce
	s.capped = false
	if !s.deadline.IsZero() {
		if remaining := time.Until(s.deadline); remaining < d {
			d = remaining
			s.capped = true
		}
	}
	if d <= 0 {
		d = 1 // fire immediately on next iteration
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.NewTimer(d)
}

// recoveryState drives an exponential-backoff retry timer used to re-attempt a
// failed reload even when no new events arrive. The first retry waits initial;
// each consecutive failure doubles the delay, capped at maxDelay. A zero initial
// disables retries entirely. Shared by the independent VCL and TLS reload paths.
type recoveryState struct {
	initial  time.Duration
	maxDelay time.Duration
	timer    *time.Timer
	delay    time.Duration
}

// next returns the delay for the next retry: initial the first time, then
// double the previous delay (capped at maxDelay).
func (r *recoveryState) next() time.Duration {
	if r.delay == 0 {
		return r.initial
	}
	next := r.delay * 2
	if r.maxDelay > 0 && next > r.maxDelay {
		next = r.maxDelay
	}

	return next
}

// arm schedules a retry after next() and returns the chosen delay so the caller
// can log it with a path-specific (static) message. It returns 0 and arms
// nothing when retries are disabled (initial <= 0).
func (r *recoveryState) arm() time.Duration {
	if r.initial <= 0 {
		return 0
	}
	delay := r.next()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.NewTimer(delay)
	r.delay = delay

	return delay
}

// reset stops any pending retry and resets the backoff.
func (r *recoveryState) reset() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.delay = 0
}

// fired clears the timer reference after its channel has fired (so the drained
// timer is not selected again) and returns the delay that had elapsed.
func (r *recoveryState) fired() time.Duration {
	delay := r.delay
	r.timer = nil

	return delay
}

// channel returns the retry timer's channel, or nil when no retry is armed so
// the select case is never taken.
func (r *recoveryState) channel() <-chan time.Time {
	if r.timer == nil {
		return nil
	}

	return r.timer.C
}

// statusStore holds runtime state for the /status endpoint.
// The event loop writes via update methods; the HTTP handler reads via snapshot().
type statusStore struct {
	mu sync.RWMutex

	// Populated during startup. The metrics server (serving /status) starts
	// early, so fields assigned after that point must go through the locked
	// setters below.
	version             string
	goVersion           string
	varnishMajorVersion int
	serviceName         string
	serviceNamespace    string
	drainEnabled        bool
	broadcastEnabled    bool
	startedAt           time.Time

	// Dynamic (updated by the event loop).
	frontendCount int
	backendCounts map[string]int
	valuesCount   int
	secretsCount  int
	lastReloadAt  time.Time
	reloadCount   int64
	varnishdUp    bool

	// TLS. tlsEnabled/tlsSupported/tlsConfiguredCerts are set once at startup;
	// tlsCerts and tlsLastReloadAt are updated as certificates (re)load.
	tlsEnabled         bool
	tlsSupported       bool
	tlsConfiguredCerts int
	tlsLastReloadAt    time.Time
	tlsCerts           map[string]tlsCertInfo
}

// tlsCertInfo is the parsed metadata for one loaded TLS certificate.
type tlsCertInfo struct {
	Name     string    `json:"name"`
	Subject  string    `json:"subject"`
	DNSNames []string  `json:"dnsNames"`
	NotAfter time.Time `json:"notAfter"`
	Issuer   string    `json:"issuer"`
}

// tlsStatus is the TLS section of the /status response.
type tlsStatus struct {
	Enabled         bool          `json:"enabled"`
	Supported       bool          `json:"supported"`
	ConfiguredCerts int           `json:"configuredCerts"`
	ActiveCerts     int           `json:"activeCerts"`
	LastReloadAt    *time.Time    `json:"lastReloadAt"`
	Certificates    []tlsCertInfo `json:"certificates"`
}

// statusResponse is the JSON structure returned by the /status endpoint.
type statusResponse struct {
	Version             string         `json:"version"`
	GoVersion           string         `json:"goVersion"`
	VarnishMajorVersion int            `json:"varnishMajorVersion"`
	ServiceName         string         `json:"serviceName"`
	ServiceNamespace    string         `json:"serviceNamespace"`
	DrainEnabled        bool           `json:"drainEnabled"`
	BroadcastEnabled    bool           `json:"broadcastEnabled"`
	StartedAt           time.Time      `json:"startedAt"`
	UptimeSeconds       float64        `json:"uptimeSeconds"`
	FrontendCount       int            `json:"frontendCount"`
	BackendCounts       map[string]int `json:"backendCounts"`
	ValuesCount         int            `json:"valuesCount"`
	SecretsCount        int            `json:"secretsCount"`
	LastReloadAt        *time.Time     `json:"lastReloadAt"`
	ReloadCount         int64          `json:"reloadCount"`
	VarnishdUp          bool           `json:"varnishdUp"`
	TLS                 tlsStatus      `json:"tls"`
}

// snapshot returns a JSON-ready copy of the current status.
func (s *statusStore) snapshot() statusResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var lastReload *time.Time
	if !s.lastReloadAt.IsZero() {
		t := s.lastReloadAt
		lastReload = &t
	}

	var tlsLastReload *time.Time
	if !s.tlsLastReloadAt.IsZero() {
		t := s.tlsLastReloadAt
		tlsLastReload = &t
	}
	certs := make([]tlsCertInfo, 0, len(s.tlsCerts))
	for _, c := range s.tlsCerts {
		certs = append(certs, c)
	}
	slices.SortFunc(certs, func(a, b tlsCertInfo) int { return strings.Compare(a.Name, b.Name) })

	return statusResponse{
		Version:             s.version,
		GoVersion:           s.goVersion,
		VarnishMajorVersion: s.varnishMajorVersion,
		ServiceName:         s.serviceName,
		ServiceNamespace:    s.serviceNamespace,
		DrainEnabled:        s.drainEnabled,
		BroadcastEnabled:    s.broadcastEnabled,
		StartedAt:           s.startedAt,
		UptimeSeconds:       time.Since(s.startedAt).Seconds(),
		FrontendCount:       s.frontendCount,
		BackendCounts:       maps.Clone(s.backendCounts),
		ValuesCount:         s.valuesCount,
		SecretsCount:        s.secretsCount,
		LastReloadAt:        lastReload,
		ReloadCount:         s.reloadCount,
		VarnishdUp:          s.varnishdUp,
		TLS: tlsStatus{
			Enabled:         s.tlsEnabled,
			Supported:       s.tlsSupported,
			ConfiguredCerts: s.tlsConfiguredCerts,
			ActiveCerts:     len(certs),
			LastReloadAt:    tlsLastReload,
			Certificates:    certs,
		},
	}
}

// setEndpointCounts updates the frontend and backend counts.
func (s *statusStore) setEndpointCounts(frontendCount int, backendCounts map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frontendCount = frontendCount
	s.backendCounts = backendCounts
}

// recordReload records that a successful VCL reload occurred.
func (s *statusStore) recordReload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastReloadAt = time.Now()
	s.reloadCount++
}

// setVarnishdUp updates the varnishd liveness flag.
func (s *statusStore) setVarnishdUp(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.varnishdUp = up
}

// setVarnishMajorVersion records the detected varnishd major version.
func (s *statusStore) setVarnishMajorVersion(v int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.varnishMajorVersion = v
}

// setServiceInfo records the service configuration shown by /status.
func (s *statusStore) setServiceInfo(name, namespace string, drainEnabled, broadcastEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serviceName = name
	s.serviceNamespace = namespace
	s.drainEnabled = drainEnabled
	s.broadcastEnabled = broadcastEnabled
}

// setSourceCounts records the number of configured values and secrets sources.
func (s *statusStore) setSourceCounts(values, secrets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.valuesCount = values
	s.secretsCount = secrets
}

// setTLSConfig records the static TLS configuration shown by /status.
func (s *statusStore) setTLSConfig(enabled, supported bool, configured int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tlsEnabled = enabled
	s.tlsSupported = supported
	s.tlsConfiguredCerts = configured
}

// recordTLSCert records metadata for a successfully (re)loaded TLS certificate.
func (s *statusStore) recordTLSCert(info *tlsCertInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tlsCerts == nil {
		s.tlsCerts = make(map[string]tlsCertInfo)
	}
	s.tlsCerts[info.Name] = *info
	s.tlsLastReloadAt = time.Now()
}

// errNoPEMBlock is returned when a certificate has no decodable PEM block.
var errNoPEMBlock = errors.New("no PEM block in certificate")

// parseTLSCertInfo extracts /status metadata from a leaf certificate PEM (the
// first PEM block of a kubernetes.io/tls `tls.crt`).
func parseTLSCertInfo(name string, certPEM []byte) (tlsCertInfo, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return tlsCertInfo{}, errNoPEMBlock
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tlsCertInfo{}, fmt.Errorf("parse certificate: %w", err)
	}
	dnsNames := cert.DNSNames
	if dnsNames == nil {
		dnsNames = []string{}
	}

	return tlsCertInfo{
		Name:     name,
		Subject:  cert.Subject.String(),
		DNSNames: dnsNames,
		NotAfter: cert.NotAfter,
		Issuer:   cert.Issuer.String(),
	}, nil
}

// recordTLSCertInfo parses a leaf certificate and records its metadata in the
// status store and the cert-expiry gauge. Parse failures are logged but never
// fatal — the certificate has already been validated and loaded by the manager.
//
// The expiry gauge is keyed by the configured certificate name, of which there
// is a fixed set (one per --tls-cert flag), so the series count is bounded and
// no per-cert cleanup is needed: certificate names are never removed at runtime
// (an emptied Secret keeps the previously-loaded certificate active).
func recordTLSCertInfo(store *statusStore, metrics *telemetry.Metrics, name string, certPEM []byte) {
	info, err := parseTLSCertInfo(name, certPEM)
	if err != nil {
		slog.Warn("could not parse TLS certificate for /status", "cert", name, "error", err)

		return
	}
	if store != nil {
		store.recordTLSCert(&info)
	}
	if metrics != nil {
		metrics.TLSCertExpiryTimestampSeconds.WithLabelValues(name).Set(float64(info.NotAfter.Unix()))
	}
}

// statusHandler returns an HTTP handler that serves the /status JSON endpoint.
func statusHandler(store *statusStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.snapshot())
	}
}

// healthzHandler returns an HTTP handler that serves the /healthz liveness endpoint.
func healthzHandler(store *statusStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}
		store.mu.RLock()
		up := store.varnishdUp
		store.mu.RUnlock()

		if up {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "varnishd not running\n")
		}
	}
}

// readyzHandler returns an HTTP handler that serves the /readyz readiness endpoint.
// In addition to checking the varnishdUp flag, it performs a TCP dial to listenAddr
// to verify that the Varnish child process is accepting connections.
func readyzHandler(store *statusStore, listenAddr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}
		store.mu.RLock()
		up := store.varnishdUp
		store.mu.RUnlock()

		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready\n")

			return
		}

		dialer := net.Dialer{Timeout: 1 * time.Second}
		conn, err := dialer.DialContext(r.Context(), "tcp", listenAddr)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready: varnish not accepting connections\n")

			return
		}
		_ = conn.Close()

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}
}

// backendCountsMap converts a backend endpoint map to a count map.
func backendCountsMap(backends map[string]renderer.BackendGroup) map[string]int {
	m := make(map[string]int, len(backends))
	for name, bg := range backends {
		m[name] = len(bg.Endpoints)
	}

	return m
}

// loopConfig holds all inputs for the main event loop.
type loopConfig struct {
	rend     vclRenderer
	mgr      varnishProcess
	bcast    broadcaster  // nil when broadcast is disabled
	status   *statusStore // nil when status endpoint is disabled
	metrics  *telemetry.Metrics
	redactor *redact.Redactor // nil when no secrets are configured
	log      *slog.Logger     // nil falls back to slog.Default(); injectable for tests

	frontendCh  <-chan []watcher.Frontend
	backendCh   chan backendChange
	valuesCh    chan valuesChange
	secretsCh   chan secretsChange
	tlsCertCh   chan tlsCertChange
	templateCh  <-chan struct{}
	sigCh       <-chan os.Signal
	ncsaEvents  <-chan varnish.NCSAEvent // nil when disabled
	ncsaCrashed <-chan struct{}          // closed when ncsa crash-loops (nil when disabled)
	stopNCSA    func()                   // called during shutdown (nil when disabled)
	serverErrCh <-chan error             // fatal metrics/broadcast HTTP serve error; nil (e.g. in tests) is never selected

	serviceName           string
	frontendDebounce      time.Duration
	frontendDebounceMax   time.Duration
	backendDebounce       time.Duration
	backendDebounceMax    time.Duration
	tlsCertDebounce       time.Duration
	tlsCertDebounceMax    time.Duration
	shutdownTimeout       time.Duration
	broadcastDrainTimeout time.Duration

	// reloadRecoveryInitial is the first delay used after a reload failure.
	// On each consecutive failure the delay doubles, capped at
	// reloadRecoveryMax. On a successful reload both are reset so that the
	// next failure starts again at reloadRecoveryInitial. Zero disables the
	// recovery loop.
	reloadRecoveryInitial time.Duration
	reloadRecoveryMax     time.Duration

	// backendTombstoneTTL bounds how long a removed discovered backend's
	// generation tombstone (removedGen) is retained. A periodic sweep evicts
	// entries older than this so the map cannot grow unbounded under churn.
	// Zero disables the sweep (tombstones are kept for the process lifetime).
	backendTombstoneTTL time.Duration

	recorder record.EventRecorder
	podRef   *v1.ObjectReference

	drainBackend      string
	drainDelay        time.Duration
	drainPollInterval time.Duration
	drainTimeout      time.Duration

	latestFrontends []watcher.Frontend
	latestBackends  map[string]renderer.BackendGroup
	latestValues    map[string]map[string]any
	latestSecrets   map[string]map[string]any

	lastVCLHash [sha256.Size]byte
}

// debug emits a DEBUG-level diagnostic for an event-loop event or decision. It
// uses lc.log when set (tests inject a capturing logger) and otherwise the
// process default logger. Callers must pass raw structured attributes (names,
// generations, counts) and never secret values or certificate material.
func (lc *loopConfig) debug(msg string, args ...any) {
	l := lc.log
	if l == nil {
		l = slog.Default()
	}
	//nolint:sloglint // msg is forwarded from call sites that all pass string literals ("event"/"decision")
	l.Debug(msg, args...)
}

// setupMetricsServer starts the Prometheus metrics / status / health HTTP
// server when cfg.MetricsAddr is set, serving /metrics, /status, /healthz and
// /readyz. The listener is bound synchronously so a port conflict fails startup
// cleanly (deferred cleanup runs) rather than from inside the goroutine via
// [os.Exit]; a fatal serve error is delivered on serverErrCh. It returns the
// running server so the caller can [http.Server.Shutdown] it on exit, or
// (nil, nil) when no metrics address is configured.
func setupMetricsServer(cfg *config.Config, status *statusStore, serverErrCh chan<- error) (*http.Server, error) {
	if cfg.MetricsAddr == "" {
		//nolint:nilnil // (nil, nil) signals "no metrics server configured"; absence is not an error
		return nil, nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(&telemetry.ZeroCounterFilter{Inner: prometheus.DefaultGatherer}, promhttp.HandlerOpts{}))
	mux.HandleFunc("/status", statusHandler(status))
	mux.HandleFunc("/healthz", healthzHandler(status))
	readyzAddr := net.JoinHostPort("localhost", strconv.FormatInt(int64(cfg.ListenAddrs[0].Port), 10))
	mux.HandleFunc("/readyz", readyzHandler(status, readyzAddr))
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.MetricsReadHeaderTimeout,
		ReadTimeout:       cfg.MetricsReadTimeout,
		WriteTimeout:      cfg.MetricsWriteTimeout,
		IdleTimeout:       cfg.MetricsIdleTimeout,
	}
	// Pre-bind so a port conflict fails startup cleanly (deferred cleanup
	// runs) rather than from inside the goroutine via [os.Exit].
	ln, listenErr := (&net.ListenConfig{}).Listen(context.Background(), "tcp", cfg.MetricsAddr)
	if listenErr != nil {
		return nil, fmt.Errorf("metrics server listen on %s: %w", cfg.MetricsAddr, listenErr)
	}
	go func() {
		slog.Info("starting metrics server", "addr", cfg.MetricsAddr)

		serveErr := metricsSrv.Serve(ln)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			select {
			case serverErrCh <- fmt.Errorf("metrics server: %w", serveErr):
			default:
			}
		}
	}()

	return metricsSrv, nil
}

// main is a thin wrapper around run. It exists so that run's deferred cleanups
// (the initial VCL temp file and the TLS temp dir) execute before the process
// exits: [os.Exit] does not run deferred functions, so the cleanup must live in
// a function that returns its exit code rather than calling [os.Exit] directly.
func main() {
	os.Exit(run())
}

// run wires up every component and drives the event loop, returning the process
// exit code. All cleanup is registered with defer here so it runs on every
// return path (including startup failures), which [os.Exit] in main would skip.
func run() int {
	cfg, err := config.Parse(version, os.Args)
	if errors.Is(err, config.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}

	logOpts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var logHandler slog.Handler
	if cfg.LogFormat == "json" {
		logHandler = slog.NewJSONHandler(os.Stderr, logOpts)
	} else {
		logHandler = slog.NewTextHandler(os.Stderr, logOpts)
	}
	slog.SetDefault(slog.New(logHandler))

	// Create metrics instance on the default registry.
	metrics := telemetry.NewMetrics(prometheus.DefaultRegisterer, cfg.DebounceLatencyBuckets)
	metrics.BuildInfo.WithLabelValues(version, runtime.Version()).Set(1)

	// Create status store for the /status endpoint.
	status := &statusStore{
		version:   version,
		goVersion: runtime.Version(),
		startedAt: time.Now(),
	}

	// serverErrCh carries a fatal serve error from the metrics or broadcast HTTP
	// server goroutine into the event loop, so a serving failure triggers the
	// normal orderly shutdown (cancel watchers, signal varnishd, run deferred
	// cleanup) instead of os.Exit, which would skip the defers and orphan the
	// varnishd child. Buffered for both servers so the send never blocks, even
	// if a server dies before the event loop starts selecting on it.
	serverErrCh := make(chan error, 2)

	// Start Prometheus metrics server if configured.
	metricsSrv, metricsErr := setupMetricsServer(cfg, status, serverErrCh)
	if metricsErr != nil {
		slog.Error("metrics server", "error", metricsErr)

		return 1
	}
	if metricsSrv != nil {
		// Shut the metrics server down last (this defer is registered early, so
		// it runs after all other cleanup): /healthz and /readyz stay reachable
		// for kube probes throughout drain. The event loop has already exited by
		// the time defers run, so a fresh background context is used rather than
		// the cancelled run ctx.
		defer func() {
			shutdownCtx, cancelShutdown := kubeContext(cfg.ShutdownTimeout)
			defer cancelShutdown()
			shutdownErr := metricsSrv.Shutdown(shutdownCtx)
			if shutdownErr != nil {
				slog.Warn("metrics server shutdown", "error", shutdownErr)
			}
		}()
	}

	// Build Kubernetes client.
	clientset, err := buildClientset()
	if err != nil {
		slog.Error("kubernetes client", "error", err)

		return 1
	}

	// Set up Kubernetes event recorder if POD_NAME is available.
	var (
		eventRecorder record.EventRecorder
		podRef        *v1.ObjectReference
	)
	if podName := os.Getenv("POD_NAME"); podName != "" {
		eventBroadcaster := record.NewBroadcaster()
		eventBroadcaster.StartRecordingToSink(&warnOnceEventSink{
			inner: &typedcorev1.EventSinkImpl{
				Interface: clientset.CoreV1().Events(cfg.ServiceNamespace),
			},
		})
		eventRecorder = eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "k8s-httpcache"})
		podRef = &v1.ObjectReference{
			Kind:       podKind,
			APIVersion: "v1",
			Name:       podName,
			Namespace:  cfg.ServiceNamespace,
		}
		podCtx, podCancel := kubeContext(cfg.KubeAPITimeout)
		pod, getErr := clientset.CoreV1().Pods(cfg.ServiceNamespace).Get(podCtx, podName, metav1.GetOptions{})
		podCancel()
		if getErr != nil {
			slog.Warn("could not look up pod UID for event recording; events may not appear in kubectl describe pod", "error", getErr)
		} else {
			podRef.UID = pod.UID
		}
		slog.Info("kubernetes event recorder enabled", "pod", podName, "namespace", cfg.ServiceNamespace)
	} else {
		slog.Info("POD_NAME not set, kubernetes event recording disabled")
	}

	// Determine topology zone: use explicit --zone flag if set, otherwise auto-detect.
	var localZone string
	if cfg.Zone != "" {
		localZone = cfg.Zone
		slog.Info("using explicit topology zone from --zone", "zone", localZone)
	} else {
		zoneCtx, zoneCancel := kubeContext(cfg.KubeAPITimeout)
		localZone = detectLocalZone(zoneCtx, slog.Default(), clientset, os.Getenv("NODE_NAME"))
		zoneCancel()
	}

	// Create cache manager and detect version before rendering VCL,
	// because the renderer needs the version to generate compatible VCL.
	slog.Info("using cache binaries", "daemon", cfg.VarnishdPath, "admin", cfg.VarnishadmPath, "stat", cfg.VarnishstatPath)
	listenAddrs := make([]string, len(cfg.ListenAddrs))
	for i, la := range cfg.ListenAddrs {
		listenAddrs[i] = la.Raw
	}
	mgr := varnish.New(cfg.VarnishdPath, cfg.VarnishadmPath, listenAddrs, cfg.ExtraVarnishd, cfg.VarnishstatPath, metrics)
	mgr.AdminTimeout = cfg.AdminTimeout
	mgr.ReloadRetries = cfg.VCLReloadRetries
	mgr.ReloadRetryInterval = cfg.VCLReloadRetryInterval
	mgr.VCLKept = cfg.VCLKept

	secretRedactor := redact.NewRedactor()
	mgr.SetRedactor(secretRedactor)

	err = mgr.DetectVersion()
	if err != nil {
		slog.Error("cache version", "error", err)

		return 1
	}
	status.setVarnishMajorVersion(mgr.MajorVersion())

	// Native TLS requires Varnish/Vinyl 9+. Fail fast rather than serving an
	// https listener that has no certificate (every handshake would fail).
	if len(cfg.TLSCerts) > 0 && !mgr.TLSSupported() {
		slog.Error("--tls-cert requires Varnish/Vinyl major version >= 9", "detected", mgr.MajorVersion())

		return 1
	}

	// Register varnishstat Prometheus exporter if enabled.
	if cfg.VarnishstatExport {
		vsc := telemetry.NewVarnishstatCollector(mgr.VarnishstatFunc(), cfg.VarnishstatExportFilter)
		prometheus.DefaultRegisterer.MustRegister(vsc)
		slog.Info("varnishstat prometheus exporter enabled", "filter", cfg.VarnishstatExportFilter)
	}

	// Parse VCL template.
	rend, err := renderer.New(cfg.VCLTemplate, cfg.TemplateDelimLeft, cfg.TemplateDelimRight, cfg.TemplateFuncs)
	if err != nil {
		slog.Error("renderer", "error", err)

		return 1
	}
	if cfg.Drain {
		rend.SetDrainBackend(drainBackendName)
	}
	if localZone != "" {
		rend.SetLocalZone(localZone)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start frontend watcher.
	w := watcher.New(clientset, cfg.ServiceNamespace, cfg.ServiceName, "")
	go func() {
		runErr := w.Run(ctx)
		if runErr != nil {
			slog.Error("watcher error", "error", runErr)
		}
	}()

	// Build annotation exclusion filter (defaults + user patterns).
	excludeAnnotationPatterns := slices.Concat(watcher.DefaultExcludeAnnotations, cfg.ExcludeAnnotations)
	excludeAnnotation := watcher.BuildAnnotationFilter(excludeAnnotationPatterns)

	// Start backend watchers.
	var (
		bwNames    = make([]string, 0, len(cfg.Backends))
		bwWatchers = make([]*watcher.BackendWatcher, 0, len(cfg.Backends))
	)
	for _, b := range cfg.Backends {
		bw := watcher.NewBackendWatcher(clientset, b.Namespace, b.ServiceName, b.Port)
		bw.SetExcludeAnnotations(excludeAnnotation)
		name := b.Name
		go func() {
			runErr := bw.Run(ctx)
			if runErr != nil {
				slog.Error("backend watcher error", "backend", name, "error", runErr)
			}
		}()
		bwNames = append(bwNames, name)
		bwWatchers = append(bwWatchers, bw)
	}

	// Start discovery watchers for --backend-selector.
	explicitNames := make(map[string]bool, len(cfg.Backends))
	for _, b := range cfg.Backends {
		explicitNames[b.Name] = true
	}
	// Shared across all discovery watchers so a backend name is managed by at
	// most one Service even when selectors overlap (the consumer keys backends
	// by bare name). Selector overlap is a runtime property, so it cannot be
	// rejected at startup; the first watcher to discover a name owns it and the
	// others skip it.
	nameRegistry := watcher.NewNameRegistry()
	discoveryWatchers := make([]*watcher.BackendDiscoveryWatcher, 0, len(cfg.BackendSelectors))
	for _, bs := range cfg.BackendSelectors {
		sel, _ := labels.Parse(bs.Selector) // already validated in config
		dw := watcher.NewBackendDiscoveryWatcher(clientset, bs.Namespace, bs.AllNamespaces, sel, bs.Port, explicitNames)
		dw.SetExcludeAnnotations(excludeAnnotation)
		dw.SetNameRegistry(nameRegistry)
		go func() {
			runErr := dw.Run(ctx)
			if runErr != nil {
				slog.Error("discovery watcher error", "selector", bs.Selector, "error", runErr)
			}
		}()
		discoveryWatchers = append(discoveryWatchers, dw)
	}

	// Start ConfigMap watchers for --values.
	var (
		vwNames    = make([]string, 0, len(cfg.Values))
		vwWatchers = make([]*watcher.ConfigMapWatcher, 0, len(cfg.Values))
	)
	for _, v := range cfg.Values {
		vw := watcher.NewConfigMapWatcher(clientset, v.Namespace, v.ConfigMapName)
		name := v.Name
		go func() {
			runErr := vw.Run(ctx)
			if runErr != nil {
				slog.Error("values watcher error", "values", name, "error", runErr)
			}
		}()
		vwNames = append(vwNames, name)
		vwWatchers = append(vwWatchers, vw)
	}

	// Start SecretWatchers for --secrets.
	var (
		swNames    = make([]string, 0, len(cfg.Secrets))
		swWatchers = make([]*watcher.SecretWatcher, 0, len(cfg.Secrets))
	)
	for _, s := range cfg.Secrets {
		sw := watcher.NewSecretWatcher(clientset, s.Namespace, s.SecretName)
		name := s.Name
		go func() {
			runErr := sw.Run(ctx)
			if runErr != nil {
				slog.Error("secrets watcher error", "secrets", name, "error", runErr)
			}
		}()
		swNames = append(swNames, name)
		swWatchers = append(swWatchers, sw)
	}

	// Start TLSCertWatchers for --tls-cert.
	var (
		twNames    = make([]string, 0, len(cfg.TLSCerts))
		twWatchers = make([]*watcher.TLSCertWatcher, 0, len(cfg.TLSCerts))
	)
	for _, tc := range cfg.TLSCerts {
		tw := watcher.NewTLSCertWatcher(clientset, tc.Namespace, tc.SecretName)
		name := tc.Name
		go func() {
			runErr := tw.Run(ctx)
			if runErr != nil {
				slog.Error("tls-cert watcher error", "tls_cert", name, "error", runErr)
			}
		}()
		twNames = append(twNames, name)
		twWatchers = append(twWatchers, tw)
	}

	// Start directory watchers for --values-dir (polling only when enabled).
	fvwWatchers, fvwNames := startValuesDirWatchers(ctx, cfg)

	// Populate remaining static status fields now that config is known.
	status.setServiceInfo(cfg.ServiceName, cfg.ServiceNamespace, cfg.Drain, cfg.BroadcastAddr != "")

	// Collect the initial endpoint snapshot from every watcher before
	// starting varnishd, so it launches with a complete configuration.
	// The watcher guarantees at least one send after cache sync (even if
	// the endpoint list is empty). The collection is bounded by
	// --startup-timeout so a watcher that never syncs (e.g. the Kubernetes API
	// is unreachable, or AddEventHandler failed before the guaranteed initial
	// send) cannot hang startup indefinitely.
	slog.Info("waiting for initial endpoint data", "timeout", cfg.StartupTimeout)
	startupCtx := ctx
	if cfg.StartupTimeout > 0 {
		var startupCancel context.CancelFunc
		startupCtx, startupCancel = context.WithTimeout(ctx, cfg.StartupTimeout)
		defer startupCancel()
	}
	initial, err := awaitInitial(startupCtx, func() initialEndpoints {
		data := initialEndpoints{
			backends: make(map[string]renderer.BackendGroup),
			values:   make(map[string]map[string]any),
			secrets:  make(map[string]map[string]any),
			tls:      make(map[string]watcher.TLSCertData, len(twWatchers)),
		}
		data.frontends = <-w.Changes()
		for i, bw := range bwWatchers {
			data.backends[bwNames[i]] = renderer.BackendGroup{
				Endpoints:   <-bw.Changes(),
				Labels:      bw.Labels(),
				Annotations: bw.Annotations(),
			}
		}
		for _, dw := range discoveryWatchers {
			<-dw.Initial()
			for name, eps := range dw.InitialState() {
				data.backends[name] = renderer.BackendGroup{
					Endpoints:   eps,
					Labels:      dw.InitialLabels()[name],
					Annotations: dw.InitialAnnotations()[name],
				}
			}
		}
		for i, vw := range vwWatchers {
			data.values[vwNames[i]] = <-vw.Changes()
		}
		for i, fvw := range fvwWatchers {
			data.values[fvwNames[i]] = <-fvw.Changes()
		}
		for i, sw := range swWatchers {
			data.secrets[swNames[i]] = <-sw.Changes()
		}
		for i, tw := range twWatchers {
			data.tls[twNames[i]] = <-tw.Changes()
		}

		return data
	})
	if err != nil {
		slog.Error("timed out waiting for initial endpoint data; is the Kubernetes API reachable?",
			"timeout", cfg.StartupTimeout, "error", err)

		return 1
	}
	latestFrontends := initial.frontends
	latestBackends := initial.backends
	latestValues := initial.values
	latestSecrets := initial.secrets
	initialTLS := initial.tls
	if len(latestFrontends) == 0 {
		slog.Warn("frontend Service has no ready endpoints at startup",
			"namespace", cfg.ServiceNamespace, "service", cfg.ServiceName)
	}
	secretRedactor.Update(latestSecrets)
	slog.Info("received initial endpoints", "frontends", len(latestFrontends), "backend_groups", len(latestBackends), "values", len(latestValues), "secrets", len(latestSecrets))

	// Set initial status counts.
	status.setEndpointCounts(len(latestFrontends), backendCountsMap(latestBackends))
	status.setSourceCounts(len(latestValues), len(latestSecrets))

	// Set initial endpoint gauges.
	metrics.Endpoints.WithLabelValues("frontend", cfg.ServiceName).Set(float64(len(latestFrontends)))
	for name, bg := range latestBackends {
		metrics.Endpoints.WithLabelValues("backend", name).Set(float64(len(bg.Endpoints)))
	}

	// Start broadcast server if configured.
	var bcast *broadcast.Server
	if cfg.BroadcastAddr != "" {
		bcast = broadcast.New(broadcast.Options{
			Addr:              cfg.BroadcastAddr,
			TargetPort:        cfg.BroadcastTargetPort,
			ServerIdleTimeout: cfg.BroadcastServerIdleTimeout,
			ReadHeaderTimeout: cfg.BroadcastReadHeaderTimeout,
			ReadTimeout:       cfg.BroadcastReadTimeout,
			WriteTimeout:      cfg.BroadcastWriteTimeout,
			ClientTimeout:     cfg.BroadcastClientTimeout,
			ClientIdleTimeout: cfg.BroadcastClientIdleTimeout,
			ShutdownTimeout:   cfg.BroadcastShutdownTimeout,
			Metrics:           metrics,
		})
		bcast.SetFrontends(latestFrontends)
		// Pre-bind so a port conflict fails startup cleanly (before varnishd
		// starts) rather than from inside the goroutine via [os.Exit].
		ln, listenErr := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.BroadcastAddr)
		if listenErr != nil {
			slog.Error("broadcast server listen", "addr", cfg.BroadcastAddr, "error", listenErr)

			return 1
		}
		go func() {
			slog.Info("starting broadcast server", "addr", cfg.BroadcastAddr)

			serveErr := bcast.Serve(ln)
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				select {
				case serverErrCh <- serveErr:
				default:
				}
			}
		}()
	}

	// Render VCL with real endpoint data and start varnishd.
	renderStart := time.Now()
	initialVCLStr, err := rend.Render(latestFrontends, latestBackends, latestValues, latestSecrets)
	metrics.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
	if err != nil {
		slog.Error("initial render", "error", err)

		return 1
	}
	initialVCLHash := sha256.Sum256([]byte(initialVCLStr))
	initialVCL, cleanupVCL, err := writeInitialVCLFile(initialVCLStr)
	if err != nil {
		slog.Error("initial VCL temp file", "error", err)

		return 1
	}
	defer cleanupVCL()

	err = mgr.Start(initialVCL)
	if err != nil {
		slog.Error("varnish start", "error", err)

		return 1
	}
	defer mgr.CleanupTLS()

	status.setTLSConfig(len(cfg.TLSCerts) > 0, mgr.TLSSupported(), len(cfg.TLSCerts))

	// Install initial TLS certificates before the pod becomes Ready: the https
	// listener exists but has no certificate until loaded. A present-but-broken
	// certificate is fatal (don't go Ready with a failing https listener); an
	// absent Secret is a no-op (LoadCert returns nil) and is loaded hot once it
	// appears.
	loadedCerts := 0
	for _, name := range twNames {
		d := initialTLS[name]
		err = mgr.LoadCert(name, d.Cert, d.Key, d.CA)
		if err != nil {
			slog.Error("loading initial TLS certificate", "cert", name, "error", err)

			return 1
		}
		if !d.Empty() {
			loadedCerts++
			recordTLSCertInfo(status, metrics, name, d.Cert)
		} else {
			slog.Warn("TLS certificate Secret is empty at startup; https listener has no certificate until it appears", "cert", name)
		}
	}
	if loadedCerts > 0 && eventRecorder != nil && podRef != nil {
		eventRecorder.Event(podRef, v1.EventTypeNormal, "TLSCertLoaded", fmt.Sprintf("Loaded %d initial TLS certificate(s)", loadedCerts))
	}

	metrics.VarnishdUp.Set(1)
	status.setVarnishdUp(true)
	if eventRecorder != nil && podRef != nil {
		eventRecorder.Event(podRef, v1.EventTypeNormal, "VCLReloaded", "Initial VCL loaded and varnishd started")
	}

	// Start access logging subprocess (opt-in).
	if cfg.VarnishncsaEnabled {
		mgr.StartNCSA(cfg.VarnishncsaPath, buildNCSAArgs(cfg), cfg.VarnishncsaPrefix)
		slog.Info("access logging enabled", "path", cfg.VarnishncsaPath)
	}

	// Watch VCL template file for changes (only when --file-watch is enabled).
	templateCh := startTemplateWatch(ctx, cfg)

	// Fan-in backend watcher updates to a single channel.
	var backendCh chan backendChange
	if len(bwWatchers) > 0 || len(discoveryWatchers) > 0 {
		backendCh = make(chan backendChange, len(bwWatchers)+len(discoveryWatchers)*8)
		for i, bw := range bwWatchers {
			name := bwNames[i]
			go func() {
				for eps := range bw.Changes() {
					backendCh <- backendChange{name: name, endpoints: eps, labels: bw.Labels(), annotations: bw.Annotations()}
				}
			}()
		}
		for _, dw := range discoveryWatchers {
			go func() {
				for update := range dw.Changes() {
					backendCh <- backendChange{
						name:        update.Name,
						endpoints:   update.Endpoints,
						labels:      update.Labels,
						annotations: update.Annotations,
						removed:     update.Endpoints == nil,
						gen:         update.Gen,
					}
				}
			}()
		}
	}

	// Fan-in --values (ConfigMap) and --values-dir updates to a single channel
	// (the latter only when --values-dir-watch is enabled).
	valuesCh := wireValuesFanIn(cfg, vwWatchers, vwNames, fvwWatchers, fvwNames)

	// Fan-in SecretWatcher updates to a single channel.
	var secretsCh chan secretsChange
	if len(swWatchers) > 0 {
		secretsCh = make(chan secretsChange, len(swWatchers))
		for i, sw := range swWatchers {
			name := swNames[i]
			go func() {
				for data := range sw.Changes() {
					secretsCh <- secretsChange{name: name, data: data}
				}
			}()
		}
	}

	// Fan-in TLSCertWatcher updates to a single channel.
	var tlsCertCh chan tlsCertChange
	if len(twWatchers) > 0 {
		tlsCertCh = make(chan tlsCertChange, len(twWatchers))
		for i, tw := range twWatchers {
			name := twNames[i]
			go func() {
				for data := range tw.Changes() {
					tlsCertCh <- tlsCertChange{name: name, data: data}
				}
			}()
		}
	}

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Main event loop with debounce.
	lc := &loopConfig{
		rend:     rend,
		mgr:      mgr,
		status:   status,
		metrics:  metrics,
		redactor: secretRedactor,
		log:      slog.Default(),

		frontendCh:  w.Changes(),
		backendCh:   backendCh,
		valuesCh:    valuesCh,
		secretsCh:   secretsCh,
		tlsCertCh:   tlsCertCh,
		templateCh:  templateCh,
		sigCh:       sigCh,
		ncsaEvents:  mgr.NCSAEvents(),
		ncsaCrashed: mgr.NCSACrashed(),
		stopNCSA:    func() { mgr.StopNCSA() },
		serverErrCh: serverErrCh,

		serviceName:           cfg.ServiceName,
		frontendDebounce:      cfg.FrontendDebounce,
		frontendDebounceMax:   cfg.FrontendDebounceMax,
		backendDebounce:       cfg.BackendDebounce,
		backendDebounceMax:    cfg.BackendDebounceMax,
		tlsCertDebounce:       cfg.TLSCertDebounce,
		tlsCertDebounceMax:    cfg.TLSCertDebounceMax,
		shutdownTimeout:       cfg.ShutdownTimeout,
		broadcastDrainTimeout: cfg.BroadcastDrainTimeout,

		reloadRecoveryInitial: 10 * time.Second,
		reloadRecoveryMax:     time.Minute,

		backendTombstoneTTL: 10 * time.Minute,

		recorder: eventRecorder,
		podRef:   podRef,

		drainBackend:      drainBackendForLoop(cfg.Drain),
		drainDelay:        cfg.DrainDelay,
		drainPollInterval: cfg.DrainPollInterval,
		drainTimeout:      cfg.DrainTimeout,

		latestFrontends: latestFrontends,
		latestBackends:  latestBackends,
		latestValues:    latestValues,
		latestSecrets:   latestSecrets,

		lastVCLHash: initialVCLHash,
	}
	// Assign the broadcaster only when it is enabled. A nil *broadcast.Server
	// stored in the broadcaster interface field would be a typed nil, making
	// `lc.bcast != nil` true and panicking when the loop calls into it.
	setBroadcaster(lc, bcast)

	return runLoop(ctx, cancel, lc)
}

// setBroadcaster assigns bcast to lc.bcast only when bcast is non-nil, so that
// disabling broadcast (`--broadcast-addr=none`) leaves lc.bcast as a true nil
// interface rather than a typed-nil that would defeat the loop's nil guards.
func setBroadcaster(lc *loopConfig, bcast *broadcast.Server) {
	if bcast != nil {
		lc.bcast = bcast
	}
}

// runLoop runs the main event loop, returning 0 for clean shutdown and 1 for error.
// rollbackReload re-renders VCL with the rolled-back template and reloads
// Varnish. Called after the primary reload fails on a new template. Returns
// true iff the render+reload completed successfully.
func rollbackReload(lc *loopConfig, reasons string) bool {
	renderStart := time.Now()
	vclStr, err := lc.rend.Render(lc.latestFrontends, lc.latestBackends, lc.latestValues, lc.latestSecrets)
	lc.metrics.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
	if err != nil {
		lc.metrics.VCLRenderErrorsTotal.Inc()
		slog.Error("render error after rollback", "error", err)
		emitEvent(lc, v1.EventTypeWarning, "VCLRenderFailed", fmt.Sprintf("VCL render error after rollback: %v", err))

		return false
	}

	f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
	if err != nil {
		slog.Error("creating temp VCL file after rollback", "error", err)

		return false
	}
	vclPath := f.Name()
	_, err = f.WriteString(vclStr)
	_ = f.Close()
	if err != nil {
		slog.Error("writing temp VCL file after rollback", "error", err)
		_ = os.Remove(vclPath)

		return false
	}
	defer func() { _ = os.Remove(vclPath) }()

	err = lc.mgr.Reload(vclPath)
	if err != nil {
		lc.metrics.VCLReloadsTotal.WithLabelValues("error").Inc()
		slog.Error("reload error after rollback", "error", err)
		emitEvent(lc, v1.EventTypeWarning, "VCLReloadFailed", fmt.Sprintf("VCL reload failed after rollback: %v", err))

		return false
	}
	lc.lastVCLHash = sha256.Sum256([]byte(vclStr))
	lc.metrics.VCLReloadsTotal.WithLabelValues("success").Inc()
	emitEvent(lc, v1.EventTypeNormal, "VCLReloaded", fmt.Sprintf("VCL reloaded successfully after rollback (%s)", reasons))
	if lc.status != nil {
		lc.status.recordReload()
		lc.status.setEndpointCounts(len(lc.latestFrontends), backendCountsMap(lc.latestBackends))
	}

	return true
}

// handleDrain runs the graceful drain sequence: mark backend sick, wait for
// endpoint removal, then optionally poll active sessions until they reach 0.
func handleDrain(lc *loopConfig, sig os.Signal) {
	emitEvent(lc, v1.EventTypeNormal, "DrainStarted", fmt.Sprintf("Received %v, starting graceful drain", sig))

	// Mark the VCL backend sick so Varnish starts responding
	// with Connection: close, causing clients to disconnect.
	err := lc.mgr.MarkBackendSick(lc.drainBackend)
	if err != nil {
		slog.Error("failed to mark backend sick", "backend", lc.drainBackend, "error", err)
	} else {
		slog.Info("marked backend sick", "backend", lc.drainBackend)
	}

	// Sleep drainDelay to let Kubernetes remove this pod from
	// endpoints, interruptible by a second signal.
	slog.Info("waiting for endpoint removal", "delay", lc.drainDelay)
	interrupted := false
	select {
	case <-time.After(lc.drainDelay):
	case <-lc.mgr.Done():
		slog.Info("varnishd exited during drain, abandoning drain wait")

		return
	case <-lc.sigCh:
		slog.Info("second signal received, skipping drain wait")
		interrupted = true
	}

	// Poll ActiveSessions every 1s until 0 or drainTimeout,
	// unless interrupted by a second signal. When drainTimeout
	// is 0, skip polling entirely (useful when clients hold
	// long-lived connections that may never close).
	if interrupted || lc.drainTimeout <= 0 {
		return
	}
	deadline := time.After(lc.drainTimeout)
	ticker := time.NewTicker(lc.drainPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sessions, err := lc.mgr.ActiveSessions()
			if err != nil {
				slog.Warn("failed to read active sessions", "error", err)

				continue
			}
			slog.Info("active sessions", "count", sessions)
			if sessions == 0 {
				slog.Info("all connections drained")
				emitEvent(lc, v1.EventTypeNormal, "DrainCompleted", "All connections drained")

				return
			}
		case <-lc.mgr.Done():
			slog.Info("varnishd exited during drain, abandoning drain poll")

			return
		case <-deadline:
			slog.Warn("drain timeout reached, proceeding with shutdown")
			emitEvent(lc, v1.EventTypeWarning, "DrainTimeout", "Drain timeout reached, proceeding with forced shutdown")

			return
		case <-lc.sigCh:
			slog.Info("second signal received, aborting drain")

			return
		}
	}
}

func runLoop(_ context.Context, cancel context.CancelFunc, lc *loopConfig) int {
	var (
		frontend        debounceState
		backend         debounceState
		pendingReload   bool
		templateChanged bool
		pendingReasons  []string

		// Recovery retry: when a reload fails we arm vclRecovery so the reload
		// is retried even if no new Kubernetes events arrive. The delay starts
		// at reloadRecoveryInitial, doubles on each consecutive failure, and is
		// capped at reloadRecoveryMax. Reset on success.
		vclRecovery = recoveryState{initial: lc.reloadRecoveryInitial, maxDelay: lc.reloadRecoveryMax}

		// TLS certificate reloads are fully decoupled from VCL reloads: their
		// own debounce group and independent recovery timer, so a cert
		// rotation never re-renders VCL and a VCL reload never touches certs.
		tlsCert     debounceState
		tlsRecovery = recoveryState{initial: lc.reloadRecoveryInitial, maxDelay: lc.reloadRecoveryMax}
	)

	// latestTLS holds the most recent certificate material per logical name;
	// pendingCertNames tracks names awaiting a (re)load.
	latestTLS := make(map[string]watcher.TLSCertData)
	pendingCertNames := make(map[string]struct{})

	// removedGen tombstones the generation of the most recently removed
	// discovery incarnation per backend name. A late endpoint update emitted by
	// a cancelled forwarding goroutine carries that incarnation's gen, so an ADD
	// whose gen is <= the tombstone is dropped instead of resurrecting a removed
	// backend. Explicit --backend watchers carry gen 0 and are never tombstoned.
	//
	// Each tombstone also records when it was last set; a periodic sweep (see
	// backendTombstoneTTL) evicts entries older than the TTL so the map stays
	// bounded under churn. Eviction is safe because the stale-update window after
	// a removal is sub-second, far shorter than the TTL.
	removedGen := make(map[string]backendTombstone)

	// liveGen records the generation of the discovery incarnation that currently
	// owns each live backend name. When a name is handed between two
	// --backend-selector watchers, the new owner's ADD and the old owner's
	// REMOVAL reach this loop through independent fan-in goroutines and can be
	// reordered: the gen-2 ADD may be processed before the gen-1 REMOVAL. Without
	// liveGen the stale lower-gen removal would delete the backend the newer
	// incarnation owns (and a stale lower-gen ADD would overwrite its endpoints).
	// Any update whose gen is below liveGen is from a superseded incarnation and
	// dropped. Entries track only live backends (deleted on removal), so the map
	// stays bounded the same way latestBackends does. Explicit --backend watchers
	// carry gen 0 and never populate liveGen.
	liveGen := make(map[string]uint64)

	// tombstoneSweep periodically triggers eviction of expired removedGen
	// entries. The channel stays nil (and is never selected) when the TTL is 0,
	// preserving the keep-forever behaviour.
	var tombstoneSweep <-chan time.Time
	if lc.backendTombstoneTTL > 0 {
		ticker := time.NewTicker(lc.backendTombstoneTTL)
		defer ticker.Stop()
		tombstoneSweep = ticker.C
	}

	// handleReload executes the VCL reload logic, clearing both groups'
	// timers and deadlines. On success it clears any pending recovery
	// retry; on failure it keeps pendingReload/pendingReasons set and arms
	// the recovery timer so the reload is retried later.
	handleReload := func() {
		// Clear both groups.
		frontend.reset()
		backend.reset()

		if !pendingReload {
			lc.debug("decision", "action", "reload-skipped", "reason", "no-pending")

			return
		}
		pendingReload = false
		reasons := strings.Join(pendingReasons, ", ")
		lc.debug("decision", "action", "reload-triggered", "reasons", reasons, "templateChanged", templateChanged)

		// markRetry keeps pendingReload/pendingReasons set and arms the
		// recovery timer so the reload is retried even if no new event
		// arrives. Called only for mgr.Reload failures, which may be
		// transient (e.g. varnishd CLI issues during child restart).
		// Render errors bypass this because re-rendering the same inputs
		// will fail the same way; they recover when new inputs arrive.
		markRetry := func() {
			pendingReload = true
			if d := vclRecovery.arm(); d > 0 {
				slog.Warn("scheduling reload retry after failure", "delay", d)
			}
		}

		// clearPending marks the reload attempt complete (success or
		// non-retryable failure): pendingReasons is truncated and any
		// pending recovery retry is stopped.
		clearPending := func() {
			pendingReasons = pendingReasons[:0]
			vclRecovery.reset()
		}

		// If the template file changed, try to parse the new version.
		reloadedTemplate := false
		if templateChanged {
			err := lc.rend.Reload()
			if err != nil {
				lc.metrics.VCLTemplateParseErrorsTotal.Inc()
				slog.Error("template parse error, keeping old template", "error", err)
				emitEvent(lc, v1.EventTypeWarning, "VCLTemplateParseFailed", fmt.Sprintf("Template parse error: %v", err))
			} else {
				reloadedTemplate = true
			}
		}

		templateChanged = false

		renderStart := time.Now()
		vclStr, err := lc.rend.Render(lc.latestFrontends, lc.latestBackends, lc.latestValues, lc.latestSecrets)
		lc.metrics.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
		if err != nil {
			lc.metrics.VCLRenderErrorsTotal.Inc()
			slog.Error("render error", "error", err)
			emitEvent(lc, v1.EventTypeWarning, "VCLRenderFailed", fmt.Sprintf("VCL render error: %v", err))
			if reloadedTemplate {
				// A new template parsed but failed to render. Roll back to the
				// known-good template and re-render the current inputs with it,
				// so frontend/backend changes coalesced into this reload still
				// take effect (mirroring the reload-failure path below). A
				// render error with the already-active template is not retried:
				// re-rendering the same inputs would fail the same way and
				// recovers when new inputs arrive.
				lc.metrics.VCLRollbacksTotal.Inc()
				lc.rend.Rollback()
				emitEvent(lc, v1.EventTypeWarning, "VCLRolledBack", "Template rollback after render error")
				if rollbackReload(lc, reasons) {
					clearPending()
				} else {
					markRetry()
				}

				return
			}
			clearPending()

			return
		}

		vclHash := sha256.Sum256([]byte(vclStr))
		if vclHash == lc.lastVCLHash {
			slog.Info("VCL unchanged, skipping reload", "reasons", reasons)
			lc.metrics.VCLReloadsTotal.WithLabelValues("skipped").Inc()
			clearPending()

			return
		}

		f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
		if err != nil {
			slog.Error("creating temp VCL file", "error", err)
			markRetry()

			return
		}
		vclPath := f.Name()
		_, err = f.WriteString(vclStr)
		_ = f.Close()
		if err != nil {
			slog.Error("writing temp VCL file", "error", err)
			_ = os.Remove(vclPath)
			markRetry()

			return
		}

		err = lc.mgr.Reload(vclPath)
		if err != nil {
			lc.metrics.VCLReloadsTotal.WithLabelValues("error").Inc()
			slog.Error("reload error", "error", err)
			emitEvent(lc, v1.EventTypeWarning, "VCLReloadFailed", fmt.Sprintf("VCL reload failed: %v", err))
			_ = os.Remove(vclPath)

			if !reloadedTemplate {
				markRetry()

				return
			}

			// New template produced VCL that Varnish rejected; revert and
			// retry so that any concurrent frontend/backend changes still
			// take effect with the old (known-good) template.
			lc.metrics.VCLRollbacksTotal.Inc()
			lc.rend.Rollback()
			slog.Warn("rolled back to previous template")
			emitEvent(lc, v1.EventTypeWarning, "VCLRolledBack", "Rolled back to previous template after reload failure")

			if rollbackReload(lc, reasons) {
				clearPending()
			} else {
				markRetry()
			}

			return
		}

		lc.lastVCLHash = vclHash
		lc.metrics.VCLReloadsTotal.WithLabelValues("success").Inc()
		lc.debug("decision", "action", "reload-applied", "reasons", reasons,
			"frontends", len(lc.latestFrontends), "backends", len(lc.latestBackends))
		emitEvent(lc, v1.EventTypeNormal, "VCLReloaded", fmt.Sprintf("VCL reloaded successfully (%s)", reasons))
		if lc.status != nil {
			lc.status.recordReload()
			lc.status.setEndpointCounts(len(lc.latestFrontends), backendCountsMap(lc.latestBackends))
		}
		_ = os.Remove(vclPath)
		clearPending()
	}

	// handleCertReload installs every pending TLS certificate via the manager,
	// independently of the VCL reload path. Names that fail stay pending and a
	// recovery retry is armed; names that succeed are cleared.
	handleCertReload := func() {
		tlsCert.reset()

		if len(pendingCertNames) == 0 {
			return
		}
		lc.debug("decision", "action", "cert-reload-triggered", "pending", len(pendingCertNames))

		failed := false
		for name := range pendingCertNames {
			d := latestTLS[name]
			// LoadCert owns the metric increments (success / error / noop).
			err := lc.mgr.LoadCert(name, d.Cert, d.Key, d.CA)
			if err != nil {
				failed = true
				slog.Error("TLS certificate reload failed", "cert", name, "error", err)
				emitEvent(lc, v1.EventTypeWarning, "TLSCertReloadFailed", fmt.Sprintf("TLS certificate %q reload failed: %v", name, err))

				continue
			}
			delete(pendingCertNames, name)
			if d.Empty() {
				// Secret deleted or not yet populated: previous cert stays active.
				slog.Warn("TLS certificate Secret is empty, keeping previous certificate active", "cert", name)
				emitEvent(lc, v1.EventTypeWarning, "TLSCertAbsent", fmt.Sprintf("TLS certificate Secret %q is empty; keeping previous certificate active", name))

				continue
			}
			recordTLSCertInfo(lc.status, lc.metrics, name, d.Cert)
			slog.Info("TLS certificate reloaded", "cert", name)
			emitEvent(lc, v1.EventTypeNormal, "TLSCertReloaded", fmt.Sprintf("TLS certificate %q reloaded", name))
		}

		if failed {
			if d := tlsRecovery.arm(); d > 0 {
				slog.Warn("scheduling TLS certificate reload retry after failure", "delay", d)
			}
		} else {
			tlsRecovery.reset()
		}
	}

	for {
		select {
		case <-lc.templateCh:
			lc.debug("event", "kind", "template")
			lc.metrics.VCLTemplateChangesTotal.Inc()
			lc.metrics.DebounceEventsTotal.WithLabelValues("backend").Inc()
			slog.Info("VCL template changed on disk, scheduling reload")
			emitEvent(lc, v1.EventTypeNormal, "VCLTemplateChanged", "VCL template file change detected on disk")
			pendingReload = true
			templateChanged = true
			pendingReasons = appendUnique(pendingReasons, "template changed")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case frontends := <-lc.frontendCh:
			lc.debug("event", "kind", "frontend", "count", len(frontends))
			lc.latestFrontends = frontends
			lc.metrics.EndpointUpdatesTotal.WithLabelValues("frontend", lc.serviceName).Inc()
			lc.metrics.Endpoints.WithLabelValues("frontend", lc.serviceName).Set(float64(len(lc.latestFrontends)))
			lc.metrics.DebounceEventsTotal.WithLabelValues("frontend").Inc()
			if lc.bcast != nil {
				lc.bcast.SetFrontends(lc.latestFrontends)
			}
			pendingReload = true
			pendingReasons = appendUnique(pendingReasons, "frontend endpoints changed")
			resetDebounce(&frontend, lc.frontendDebounce, lc.frontendDebounceMax)

		case bc := <-backendChan(lc.backendCh):
			lc.debug("event", "kind", "backend", "name", bc.name, "gen", bc.gen,
				"removed", bc.removed, "endpoints", len(bc.endpoints))
			// Drop any update — ADD or REMOVAL — from a discovery incarnation
			// older than the one that currently owns this backend name. After a
			// name is handed between two --backend-selector watchers the new
			// owner's gen-2 ADD can be processed before the old owner's gen-1
			// REMOVAL (they arrive via independent fan-in goroutines), so a stale
			// removal must not delete, nor a stale add overwrite, the live
			// gen-2 backend. gen 0 is an explicit --backend watcher and is never
			// stale.
			if bc.gen != 0 && bc.gen < liveGen[bc.name] {
				lc.debug("decision", "action", "drop", "kind", "backend", "reason", "stale-incarnation",
					"name", bc.name, "gen", bc.gen, "liveGen", liveGen[bc.name])

				continue
			}
			// Drop a stale ADD emitted by a cancelled forwarding goroutine for a
			// discovery incarnation that was already removed (its gen is <= the
			// gen recorded at removal). Once the backend was deleted its liveGen
			// entry is gone, so this tombstone — not the check above — catches the
			// late ADD that would otherwise resurrect it.
			if !bc.removed && bc.gen != 0 && bc.gen <= removedGen[bc.name].gen {
				lc.debug("decision", "action", "drop", "kind", "backend", "reason", "stale-removed-tombstone",
					"name", bc.name, "gen", bc.gen, "tombstoneGen", removedGen[bc.name].gen)

				continue
			}
			epsChanged := !watcher.EndpointsEqual(lc.latestBackends[bc.name].Endpoints, bc.endpoints)
			if bc.removed {
				ts := removedGen[bc.name]
				if bc.gen > ts.gen {
					ts.gen = bc.gen
				}
				ts.at = time.Now()
				removedGen[bc.name] = ts
				delete(lc.latestBackends, bc.name)
				delete(liveGen, bc.name)
				// Delete both per-name series (gauge and counter) on removal so
				// a churning set of discovered backend names (via
				// --backend-selector) cannot grow Prometheus cardinality without
				// bound over the process lifetime.
				lc.metrics.Endpoints.DeleteLabelValues("backend", bc.name)
				lc.metrics.EndpointUpdatesTotal.DeleteLabelValues("backend", bc.name)
				lc.debug("decision", "action", "remove", "kind", "backend",
					"name", bc.name, "gen", bc.gen, "tombstoneGen", ts.gen)
			} else {
				lc.latestBackends[bc.name] = renderer.BackendGroup{
					Endpoints:   bc.endpoints,
					Labels:      bc.labels,
					Annotations: bc.annotations,
				}
				if bc.gen != 0 {
					liveGen[bc.name] = bc.gen
				}
				lc.metrics.Endpoints.WithLabelValues("backend", bc.name).Set(float64(len(bc.endpoints)))
				lc.metrics.EndpointUpdatesTotal.WithLabelValues("backend", bc.name).Inc()
				lc.debug("decision", "action", "apply", "kind", "backend", "name", bc.name,
					"gen", bc.gen, "endpoints", len(bc.endpoints), "epsChanged", epsChanged)
			}
			lc.metrics.DebounceEventsTotal.WithLabelValues("backend").Inc()
			pendingReload = true
			if epsChanged {
				pendingReasons = appendUnique(pendingReasons, "backend endpoints changed")
			} else {
				pendingReasons = appendUnique(pendingReasons, "backend metadata changed")
			}
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case vc := <-valuesChan(lc.valuesCh):
			lc.debug("event", "kind", "values", "name", vc.name, "keys", len(vc.data))
			lc.latestValues[vc.name] = vc.data
			lc.metrics.ValuesUpdatesTotal.WithLabelValues(vc.name).Inc()
			lc.metrics.DebounceEventsTotal.WithLabelValues("backend").Inc()
			slog.Info("values ConfigMap updated", "name", vc.name)
			pendingReload = true
			pendingReasons = appendUnique(pendingReasons, "values updated")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case sc := <-secretsChan(lc.secretsCh):
			lc.debug("event", "kind", "secrets", "name", sc.name, "keys", len(sc.data))
			lc.latestSecrets[sc.name] = sc.data
			if lc.redactor != nil {
				lc.redactor.Update(lc.latestSecrets)
			}
			lc.metrics.SecretsUpdatesTotal.WithLabelValues(sc.name).Inc()
			lc.metrics.DebounceEventsTotal.WithLabelValues("backend").Inc()
			slog.Info("values Secret updated", "name", sc.name)
			pendingReload = true
			pendingReasons = appendUnique(pendingReasons, "secrets updated")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case tc := <-tlsCertChan(lc.tlsCertCh):
			lc.debug("event", "kind", "tls-cert", "name", tc.name)
			// TLS certificate rotations are applied on their own debounce
			// group and never trigger a VCL reload.
			latestTLS[tc.name] = tc.data
			pendingCertNames[tc.name] = struct{}{}
			lc.metrics.TLSCertUpdatesTotal.WithLabelValues(tc.name).Inc()
			lc.metrics.DebounceEventsTotal.WithLabelValues("tls").Inc()
			slog.Info("TLS certificate Secret updated", "name", tc.name)
			resetDebounce(&tlsCert, lc.tlsCertDebounce, lc.tlsCertDebounceMax)

		case <-timerChan(frontend.timer):
			lc.debug("event", "kind", "debounce-fire", "group", "frontend", "capped", frontend.capped)
			lc.metrics.DebounceFiresTotal.WithLabelValues("frontend").Inc()
			if frontend.capped {
				lc.metrics.DebounceMaxEnforcementsTotal.WithLabelValues("frontend").Inc()
			}
			if !frontend.firstEvent.IsZero() {
				lc.metrics.DebounceLatencySeconds.WithLabelValues("frontend").Observe(time.Since(frontend.firstEvent).Seconds())
			}
			if !backend.firstEvent.IsZero() {
				lc.metrics.DebounceLatencySeconds.WithLabelValues("backend").Observe(time.Since(backend.firstEvent).Seconds())
			}
			handleReload()

		case <-timerChan(backend.timer):
			lc.debug("event", "kind", "debounce-fire", "group", "backend", "capped", backend.capped)
			lc.metrics.DebounceFiresTotal.WithLabelValues("backend").Inc()
			if backend.capped {
				lc.metrics.DebounceMaxEnforcementsTotal.WithLabelValues("backend").Inc()
			}
			if !backend.firstEvent.IsZero() {
				lc.metrics.DebounceLatencySeconds.WithLabelValues("backend").Observe(time.Since(backend.firstEvent).Seconds())
			}
			if !frontend.firstEvent.IsZero() {
				lc.metrics.DebounceLatencySeconds.WithLabelValues("frontend").Observe(time.Since(frontend.firstEvent).Seconds())
			}
			handleReload()

		case <-tombstoneSweep:
			evicted := sweepExpiredTombstones(removedGen, time.Now(), lc.backendTombstoneTTL)
			lc.debug("event", "kind", "tombstone-sweep", "evicted", evicted, "remaining", len(removedGen))

		case <-vclRecovery.channel():
			// fired clears the timer reference before retrying. On failure
			// handleReload's markRetry arms a fresh one; on success
			// vclRecovery.clear leaves it nil. Clearing here avoids
			// re-selecting the drained channel on the next iteration.
			prevDelay := vclRecovery.fired()
			slog.Info("reload recovery timer fired, retrying reload", "previous_delay", prevDelay)
			handleReload()
			// If handleReload did nothing (no pending reload), reset the
			// backoff so a future failure starts again at the initial
			// delay. Success already clears it via vclRecovery.clear.
			if vclRecovery.timer == nil {
				vclRecovery.delay = 0
			}

		case <-timerChan(tlsCert.timer):
			lc.debug("event", "kind", "debounce-fire", "group", "tls", "capped", tlsCert.capped)
			lc.metrics.DebounceFiresTotal.WithLabelValues("tls").Inc()
			if tlsCert.capped {
				lc.metrics.DebounceMaxEnforcementsTotal.WithLabelValues("tls").Inc()
			}
			if !tlsCert.firstEvent.IsZero() {
				lc.metrics.DebounceLatencySeconds.WithLabelValues("tls").Observe(time.Since(tlsCert.firstEvent).Seconds())
			}
			handleCertReload()

		case <-tlsRecovery.channel():
			prevDelay := tlsRecovery.fired()
			slog.Info("TLS certificate recovery timer fired, retrying", "previous_delay", prevDelay)
			handleCertReload()
			if tlsRecovery.timer == nil {
				tlsRecovery.delay = 0
			}

		case ev := <-lc.ncsaEvents:
			lc.debug("event", "kind", "ncsa", "reason", ev.Reason)
			emitEvent(lc, ev.Type, ev.Reason, ev.Message)

		case <-lc.ncsaCrashed:
			slog.Error("varnishncsa crash loop detected, shutting down")
			cancel()
			lc.mgr.ForwardSignal(syscall.SIGTERM)
			select {
			case <-lc.mgr.Done():
			case <-time.After(lc.shutdownTimeout):
				slog.Warn("varnishd did not exit in time, forcing")
				lc.mgr.ForwardSignal(syscall.SIGKILL)
			}

			return 1

		case serveErr := <-lc.serverErrCh:
			slog.Error("HTTP server failed, shutting down", "error", serveErr)
			cancel()
			lc.mgr.ForwardSignal(syscall.SIGTERM)
			select {
			case <-lc.mgr.Done():
			case <-time.After(lc.shutdownTimeout):
				slog.Warn("varnishd did not exit in time, forcing")
				lc.mgr.ForwardSignal(syscall.SIGKILL)
			}

			return 1

		case sig := <-lc.sigCh:
			slog.Info("received signal, shutting down", "signal", sig)

			if lc.drainBackend != "" {
				handleDrain(lc, sig)
			}

			if lc.bcast != nil {
				_ = lc.bcast.Drain(lc.broadcastDrainTimeout)
			}

			if lc.stopNCSA != nil {
				lc.stopNCSA()
			}

			cancel()
			lc.mgr.ForwardSignal(sig)

			select {
			case <-lc.mgr.Done():
			case <-time.After(lc.shutdownTimeout):
				slog.Warn("varnishd did not exit in time, forcing")
				lc.mgr.ForwardSignal(syscall.SIGKILL)
				// SIGKILL cannot be ignored. Wait for the exit so the Err()
				// read below is ordered after the manager's Wait goroutine
				// writes it (reading earlier is a data race).
				<-lc.mgr.Done()
			}

			err := lc.mgr.Err()
			if err != nil {
				slog.Error("varnishd exited with error", "error", err)

				return 1
			}

			return 0

		case <-lc.mgr.Done():
			lc.metrics.VarnishdUp.Set(0)
			if lc.status != nil {
				lc.status.setVarnishdUp(false)
			}
			slog.Error("varnishd exited unexpectedly", "error", lc.mgr.Err())
			emitEvent(lc, v1.EventTypeWarning, "VarnishdExited", fmt.Sprintf("varnishd exited unexpectedly: %v", lc.mgr.Err()))
			cancel()

			return 1
		}
	}
}

// emitEvent records a Kubernetes Event for the pod, if the recorder is configured.
func emitEvent(lc *loopConfig, eventType, reason, message string) {
	if lc.recorder != nil && lc.podRef != nil {
		lc.recorder.Event(lc.podRef, eventType, reason, message)
	}
}

// warnOnceEventSink wraps a record.EventSink and logs a single [slog.Warn]
// on the first Forbidden (RBAC) error, then stays quiet for subsequent errors.
type warnOnceEventSink struct {
	inner  record.EventSink
	warned sync.Once
}

func (s *warnOnceEventSink) Create(event *v1.Event) (*v1.Event, error) {
	ev, err := s.inner.Create(event)
	s.checkErr(err)

	return ev, err //nolint:wrapcheck // interface impl delegates to inner sink
}

func (s *warnOnceEventSink) Update(event *v1.Event) (*v1.Event, error) {
	ev, err := s.inner.Update(event)
	s.checkErr(err)

	return ev, err //nolint:wrapcheck // interface impl delegates to inner sink
}

func (s *warnOnceEventSink) Patch(oldEvent *v1.Event, data []byte) (*v1.Event, error) {
	ev, err := s.inner.Patch(oldEvent, data)
	s.checkErr(err)

	return ev, err //nolint:wrapcheck // interface impl delegates to inner sink
}

func (s *warnOnceEventSink) checkErr(err error) {
	if err != nil && apierrors.IsForbidden(err) {
		s.warned.Do(func() {
			slog.Warn("cannot create Kubernetes Events (RBAC permission denied); "+
				"grant 'create' and 'patch' on 'events' to silence this warning",
				"error", err)
		})
	}
}

// kubeContext returns a context bounded by timeout (with its cancel func) for a
// one-shot Kubernetes API call, or a non-expiring background context when
// timeout <= 0 (the limit is disabled). Passing a zero duration to
// [context.WithTimeout] would yield an already-expired context, so the zero case
// is handled explicitly.
func kubeContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.Background(), func() {}
	}

	return context.WithTimeout(context.Background(), timeout)
}

// detectLocalZone looks up the node's topology.kubernetes.io/zone label and
// returns the zone string. Returns "" if nodeName is empty, the node cannot
// be fetched, or the label is absent.
func detectLocalZone(ctx context.Context, logger *slog.Logger, clientset kubernetes.Interface, nodeName string) string {
	if nodeName == "" {
		logger.Info("NODE_NAME not set, topology zone detection disabled (.LocalZone will be empty, .LocalEndpoints/.RemoteEndpoints will be empty in templates)")

		return ""
	}

	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		logger.Warn("could not look up node for zone detection; .LocalZone will be empty, .LocalEndpoints/.RemoteEndpoints will be empty in templates",
			"node", nodeName, "error", err)

		return ""
	}

	zone := node.Labels["topology.kubernetes.io/zone"]
	if zone == "" {
		logger.Warn("node has no topology.kubernetes.io/zone label; .LocalZone will be empty, .LocalEndpoints/.RemoteEndpoints will be empty in templates",
			"node", nodeName)

		return ""
	}

	logger.Info("detected local topology zone", "zone", zone)

	return zone
}

func buildClientset() (kubernetes.Interface, error) { //nolint:ireturn // returns interface to allow test injection
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig.
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, _ := os.UserHomeDir()
			kubeconfig = home + "/.kube/config"
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("building kubeconfig: %w", err)
		}
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	return cs, nil
}

type backendChange struct {
	name        string
	endpoints   []watcher.Endpoint
	labels      map[string]string
	annotations map[string]string
	removed     bool   // true when a discovered service disappears
	gen         uint64 // discovery incarnation generation (0 for explicit --backend watchers)
}

// backendTombstone records the highest removed generation for a discovered
// backend name together with the wall-clock time the tombstone was last set, so
// a periodic sweep can evict it once it is older than the TTL.
type backendTombstone struct {
	gen uint64
	at  time.Time
}

// sweepExpiredTombstones deletes backend tombstones older than ttl, keeping the
// removedGen map bounded under backend churn. A tombstone only needs to outlive
// the brief window (sub-second) in which a cancelled forwarding goroutine may
// emit one stale late update for the removed incarnation, so any reasonable TTL
// is safe: by the time an entry is evicted, no further update for that
// incarnation can arrive. A ttl of 0 is treated as "never expire". It returns
// the number of tombstones evicted (for trace logging).
func sweepExpiredTombstones(tombstones map[string]backendTombstone, now time.Time, ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	evicted := 0
	for name, ts := range tombstones {
		if now.Sub(ts.at) >= ttl {
			delete(tombstones, name)
			evicted++
		}
	}

	return evicted
}

type valuesChange struct {
	name string
	data map[string]any
}

type secretsChange struct {
	name string
	data map[string]any
}

type tlsCertChange struct {
	name string
	data watcher.TLSCertData
}

// backendChan returns ch, or nil if ch is nil (so select skips it).
func backendChan(ch chan backendChange) <-chan backendChange {
	if ch == nil {
		return nil
	}

	return ch
}

// valuesChan returns ch, or nil if ch is nil (so select skips it).
func valuesChan(ch chan valuesChange) <-chan valuesChange {
	if ch == nil {
		return nil
	}

	return ch
}

// startTemplateWatch returns a channel signalling VCL template file changes, or
// nil (and starts no goroutine) when --file-watch is disabled.
func startTemplateWatch(ctx context.Context, cfg *config.Config) <-chan struct{} {
	if !cfg.FileWatch {
		slog.Info("VCL template file watching disabled (--file-watch=false); --values-dir is controlled independently")

		return nil
	}

	return watchFile(ctx, cfg.VCLTemplate, cfg.VCLTemplateWatchInterval)
}

// startValuesDirWatchers creates a FileValuesWatcher per --values-dir. The polling
// goroutine is started only when --values-dir-watch is enabled; otherwise a single
// ScanOnce delivers the initial values with no ongoing poller.
func startValuesDirWatchers(ctx context.Context, cfg *config.Config) ([]*watcher.FileValuesWatcher, []string) {
	names := make([]string, 0, len(cfg.ValuesDirs))
	watchers := make([]*watcher.FileValuesWatcher, 0, len(cfg.ValuesDirs))
	for _, vd := range cfg.ValuesDirs {
		fvw := watcher.NewFileValuesWatcher(vd.Path, cfg.ValuesDirPollInterval)
		name := vd.Name
		if cfg.ValuesDirWatch {
			go func() {
				runErr := fvw.Run(ctx)
				if runErr != nil {
					slog.Error("values-dir watcher error", "values_dir", name, "error", runErr)
				}
			}()
		} else {
			fvw.ScanOnce()
		}
		names = append(names, name)
		watchers = append(watchers, fvw)
	}

	return watchers, names
}

// wireValuesFanIn fans ConfigMap (--values) and FileValuesWatcher (--values-dir)
// updates into one channel. The --values-dir goroutines are wired only when
// --values-dir-watch is enabled. Returns nil (and starts no goroutine) when there
// is nothing to fan in.
func wireValuesFanIn(cfg *config.Config, vwWatchers []*watcher.ConfigMapWatcher, vwNames []string, fvwWatchers []*watcher.FileValuesWatcher, fvwNames []string) chan valuesChange {
	fvwCount := len(fvwWatchers)
	if !cfg.ValuesDirWatch {
		fvwCount = 0
	}
	if len(vwWatchers) == 0 && fvwCount == 0 {
		return nil
	}

	valuesCh := make(chan valuesChange, len(vwWatchers)+fvwCount)
	for i, vw := range vwWatchers {
		name := vwNames[i]
		go func() {
			for data := range vw.Changes() {
				valuesCh <- valuesChange{name: name, data: data}
			}
		}()
	}
	if cfg.ValuesDirWatch {
		for i, fvw := range fvwWatchers {
			name := fvwNames[i]
			go func() {
				for data := range fvw.Changes() {
					valuesCh <- valuesChange{name: name, data: data}
				}
			}()
		}
	}

	return valuesCh
}

// secretsChan returns ch, or nil if ch is nil (so select skips it).
func secretsChan(ch chan secretsChange) <-chan secretsChange {
	if ch == nil {
		return nil
	}

	return ch
}

// tlsCertChan returns ch, or nil if ch is nil (so select skips it).
func tlsCertChan(ch chan tlsCertChange) <-chan tlsCertChange {
	if ch == nil {
		return nil
	}

	return ch
}

// drainBackendForLoop returns the internal drain backend name when drain is
// enabled, or "" to disable draining in the event loop.
func drainBackendForLoop(drain bool) string {
	if drain {
		return drainBackendName
	}

	return ""
}

// timerChan returns the timer's channel, or nil if the timer is nil.
func timerChan(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}

	return t.C
}

// appendUnique appends s to the slice only if it is not already present.
func appendUnique(slice []string, s string) []string {
	if slices.Contains(slice, s) {
		return slice
	}

	return append(slice, s)
}

// buildNCSAArgs converts varnishncsa config flags into command-line arguments.
func buildNCSAArgs(cfg *config.Config) []string {
	var args []string
	if cfg.VarnishncsaBackend {
		args = append(args, "-b")
	}
	if cfg.VarnishncsaFormat != "" {
		args = append(args, "-F", cfg.VarnishncsaFormat)
	}
	if cfg.VarnishncsaQuery != "" {
		args = append(args, "-q", cfg.VarnishncsaQuery)
	}
	if cfg.VarnishncsaOutput != "" {
		args = append(args, "-w", cfg.VarnishncsaOutput)
	}

	return args
}

// initialEndpoints is the first snapshot collected from every watcher before
// varnishd starts, so it launches with a complete configuration.
type initialEndpoints struct {
	frontends []watcher.Frontend
	backends  map[string]renderer.BackendGroup
	values    map[string]map[string]any
	secrets   map[string]map[string]any
	tls       map[string]watcher.TLSCertData
}

// awaitInitial runs collect in a goroutine and returns its result, or ctx.Err()
// if ctx is cancelled first (e.g. the startup timeout elapses because the
// Kubernetes API is unreachable and the informers never sync, or a watcher's
// Run returned before its guaranteed initial send). On cancellation the collect
// goroutine is abandoned; the caller is expected to exit the process.
func awaitInitial[T any](ctx context.Context, collect func() T) (T, error) { //nolint:ireturn // returns the caller's generic type T, not an abstract interface
	done := make(chan T, 1)
	go func() { done <- collect() }()
	select {
	case v := <-done:
		return v, nil
	case <-ctx.Done():
		var zero T

		return zero, ctx.Err() //nolint:wrapcheck // context cancellation surfaced as-is
	}
}

// writeInitialVCLFile writes the rendered VCL to a new temp file and returns
// its path along with a cleanup function that removes it. The cleanup is safe
// to call more than once and is wired into run via defer so the file is removed
// on every exit path. On any error nothing is left behind and cleanup is nil.
func writeInitialVCLFile(contents string) (string, func(), error) {
	f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
	if err != nil {
		return "", nil, fmt.Errorf("creating initial VCL temp file: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }

	_, err = f.WriteString(contents)
	if err != nil {
		_ = f.Close()
		cleanup()

		return "", nil, fmt.Errorf("writing initial VCL temp file: %w", err)
	}

	err = f.Close()
	if err != nil {
		cleanup()

		return "", nil, fmt.Errorf("closing initial VCL temp file: %w", err)
	}

	return path, cleanup, nil
}

// watchFile polls a file for content changes and sends on the returned channel
// when a change is detected. The goroutine exits when ctx is cancelled.
func watchFile(ctx context.Context, path string, interval time.Duration) <-chan struct{} {
	path = filepath.Clean(path)
	ch := make(chan struct{}, 1)

	go func() {
		// path is the operator-configured --vcl-template, validated at startup, not
		// request-derived input.
		last, _ := os.ReadFile(path) //nolint:gosec // trusted operator-configured path
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := os.ReadFile(path) //nolint:gosec // trusted operator-configured path
				if err != nil {
					continue
				}
				if !bytes.Equal(last, current) {
					last = current
					// Non-blocking send.
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	return ch
}
