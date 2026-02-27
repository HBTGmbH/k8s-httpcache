// k8s-httpcache is a Kubernetes-native HTTP caching proxy built on Varnish.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	"k8s-httpcache/internal/broadcast"
	"k8s-httpcache/internal/config"
	"k8s-httpcache/internal/redact"
	"k8s-httpcache/internal/renderer"
	"k8s-httpcache/internal/telemetry"
	"k8s-httpcache/internal/varnish"
	"k8s-httpcache/internal/watcher"
)

// version is set at build time via -ldflags.
var version = "dev"

// drainBackendName is the VCL backend name used internally for graceful
// connection draining. Users do not configure this — it is auto-injected
// into the rendered VCL when --drain is enabled.
const drainBackendName = "drain_flag"

// vclRenderer abstracts VCL template operations (satisfied by *renderer.Renderer).
type vclRenderer interface {
	Reload() error
	Render(frontends []watcher.Frontend, backends map[string][]watcher.Endpoint, values, secrets map[string]map[string]any) (string, error)
	RenderToFile(frontends []watcher.Frontend, backends map[string][]watcher.Endpoint, values, secrets map[string]map[string]any) (string, error)
	Rollback()
	SetBackendLabels(labels map[string]map[string]string)
}

// varnishProcess abstracts varnishd lifecycle (satisfied by *varnish.Manager).
type varnishProcess interface {
	Reload(vclPath string) error
	Done() <-chan struct{}
	Err() error
	ForwardSignal(sig os.Signal)
	MarkBackendSick(name string) error
	ActiveSessions() (uint64, error)
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

// statusStore holds runtime state for the /status endpoint.
// The event loop writes via update methods; the HTTP handler reads via snapshot().
type statusStore struct {
	mu sync.RWMutex

	// Static (set once during init).
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
func backendCountsMap(backends map[string][]watcher.Endpoint) map[string]int {
	m := make(map[string]int, len(backends))
	for name, eps := range backends {
		m[name] = len(eps)
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

	frontendCh  <-chan []watcher.Frontend
	backendCh   chan backendChange
	valuesCh    chan valuesChange
	secretsCh   chan secretsChange
	templateCh  <-chan struct{}
	sigCh       <-chan os.Signal
	ncsaEvents  <-chan varnish.NCSAEvent // nil when disabled
	ncsaCrashed <-chan struct{}          // closed when ncsa crash-loops (nil when disabled)
	stopNCSA    func()                   // called during shutdown (nil when disabled)

	serviceName           string
	frontendDebounce      time.Duration
	frontendDebounceMax   time.Duration
	backendDebounce       time.Duration
	backendDebounceMax    time.Duration
	shutdownTimeout       time.Duration
	broadcastDrainTimeout time.Duration

	recorder record.EventRecorder
	podRef   *v1.ObjectReference

	drainBackend      string
	drainDelay        time.Duration
	drainPollInterval time.Duration
	drainTimeout      time.Duration

	latestFrontends     []watcher.Frontend
	latestBackends      map[string][]watcher.Endpoint
	latestBackendLabels map[string]map[string]string
	latestValues        map[string]map[string]any
	latestSecrets       map[string]map[string]any

	lastVCLHash [sha256.Size]byte
}

func main() {
	cfg, err := config.Parse(version, os.Args)
	if errors.Is(err, config.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		os.Exit(2)
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

	// Start Prometheus metrics server if configured.
	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(&telemetry.ZeroCounterFilter{Inner: prometheus.DefaultGatherer}, promhttp.HandlerOpts{}))
		mux.HandleFunc("/status", statusHandler(status))
		mux.HandleFunc("/healthz", healthzHandler(status))
		readyzAddr := net.JoinHostPort("localhost", strconv.FormatInt(int64(cfg.ListenAddrs[0].Port), 10))
		mux.HandleFunc("/readyz", readyzHandler(status, readyzAddr))
		metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: cfg.MetricsReadHeaderTimeout}
		go func() {
			slog.Info("starting metrics server", "addr", cfg.MetricsAddr)

			err := metricsSrv.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("metrics server: %v", err)
			}
		}()
	}

	// Build Kubernetes client.
	clientset, err := buildClientset()
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
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
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       podName,
			Namespace:  cfg.ServiceNamespace,
		}
		pod, err := clientset.CoreV1().Pods(cfg.ServiceNamespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			slog.Warn("could not look up pod UID for event recording; events may not appear in kubectl describe pod", "error", err)
		} else {
			podRef.UID = pod.UID
		}
		slog.Info("kubernetes event recorder enabled", "pod", podName, "namespace", cfg.ServiceNamespace) //nolint:gosec // G706: values from pod spec env/flags, not runtime user input
	} else {
		slog.Info("POD_NAME not set, kubernetes event recording disabled")
	}

	// Determine topology zone: use explicit --zone flag if set, otherwise auto-detect.
	var localZone string
	if cfg.Zone != "" {
		localZone = cfg.Zone
		slog.Info("using explicit topology zone from --zone", "zone", localZone) //nolint:gosec // G706: zone from CLI flag, not runtime user input
	} else {
		localZone = detectLocalZone(slog.Default(), clientset, os.Getenv("NODE_NAME"))
	}

	// Create varnish manager and detect version before rendering VCL,
	// because the renderer needs the version to generate compatible VCL.
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
		log.Fatalf("varnish version: %v", err)
	}
	status.varnishMajorVersion = mgr.MajorVersion()

	// Register varnishstat Prometheus exporter if enabled.
	if cfg.VarnishstatExport {
		vsc := telemetry.NewVarnishstatCollector(mgr.VarnishstatFunc(), cfg.VarnishstatExportFilter)
		prometheus.DefaultRegisterer.MustRegister(vsc)
		slog.Info("varnishstat prometheus exporter enabled", "filter", cfg.VarnishstatExportFilter) //nolint:gosec // G706: filter from CLI flag, not runtime user input
	}

	// Parse VCL template.
	rend, err := renderer.New(cfg.VCLTemplate, cfg.TemplateDelimLeft, cfg.TemplateDelimRight)
	if err != nil {
		log.Fatalf("renderer: %v", err)
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
		err := w.Run(ctx)
		if err != nil {
			slog.Error("watcher error", "error", err)
		}
	}()

	// Start backend watchers.
	var (
		bwNames    = make([]string, 0, len(cfg.Backends))
		bwWatchers = make([]*watcher.BackendWatcher, 0, len(cfg.Backends))
	)
	for _, b := range cfg.Backends {
		bw := watcher.NewBackendWatcher(clientset, b.Namespace, b.ServiceName, b.Port)
		name := b.Name
		go func() {
			err := bw.Run(ctx)
			if err != nil {
				slog.Error("backend watcher error", "backend", name, "error", err)
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
	discoveryWatchers := make([]*watcher.BackendDiscoveryWatcher, 0, len(cfg.BackendSelectors))
	for _, bs := range cfg.BackendSelectors {
		sel, _ := labels.Parse(bs.Selector) // already validated in config
		dw := watcher.NewBackendDiscoveryWatcher(clientset, bs.Namespace, bs.AllNamespaces, sel, bs.Port, explicitNames)
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
			err := vw.Run(ctx)
			if err != nil {
				slog.Error("values watcher error", "values", name, "error", err)
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
			err := sw.Run(ctx)
			if err != nil {
				slog.Error("secrets watcher error", "secrets", name, "error", err)
			}
		}()
		swNames = append(swNames, name)
		swWatchers = append(swWatchers, sw)
	}

	// Start directory watchers for --values-dir.
	var (
		fvwNames    = make([]string, 0, len(cfg.ValuesDirs))
		fvwWatchers = make([]*watcher.FileValuesWatcher, 0, len(cfg.ValuesDirs))
	)
	for _, vd := range cfg.ValuesDirs {
		fvw := watcher.NewFileValuesWatcher(vd.Path, cfg.ValuesDirPollInterval)
		name := vd.Name
		go func() {
			err := fvw.Run(ctx)
			if err != nil {
				slog.Error("values-dir watcher error", "values-dir", name, "error", err)
			}
		}()
		fvwNames = append(fvwNames, name)
		fvwWatchers = append(fvwWatchers, fvw)
	}

	// Populate remaining static status fields now that config is known.
	status.serviceName = cfg.ServiceName
	status.serviceNamespace = cfg.ServiceNamespace
	status.drainEnabled = cfg.Drain
	status.broadcastEnabled = cfg.BroadcastAddr != ""

	// Collect the initial endpoint snapshot from every watcher before
	// starting varnishd, so it launches with a complete configuration.
	// The watcher guarantees at least one send after cache sync (even if
	// the endpoint list is empty), so this will not deadlock.
	slog.Info("waiting for initial endpoint data")
	latestFrontends := <-w.Changes()
	if len(latestFrontends) == 0 {
		slog.Warn("frontend Service has no ready endpoints at startup", //nolint:gosec // G706: values from CLI flags, not runtime user input
			"namespace", cfg.ServiceNamespace, "service", cfg.ServiceName)
	}
	latestBackends := make(map[string][]watcher.Endpoint)
	latestBackendLabels := make(map[string]map[string]string)
	for i, bw := range bwWatchers {
		latestBackends[bwNames[i]] = <-bw.Changes()
		latestBackendLabels[bwNames[i]] = bw.Labels()
	}
	for _, dw := range discoveryWatchers {
		<-dw.Initial()
		maps.Copy(latestBackends, dw.InitialState())
		maps.Copy(latestBackendLabels, dw.InitialLabels())
	}
	latestValues := make(map[string]map[string]any)
	for i, vw := range vwWatchers {
		latestValues[vwNames[i]] = <-vw.Changes()
	}
	for i, fvw := range fvwWatchers {
		latestValues[fvwNames[i]] = <-fvw.Changes()
	}
	latestSecrets := make(map[string]map[string]any)
	for i, sw := range swWatchers {
		latestSecrets[swNames[i]] = <-sw.Changes()
	}
	secretRedactor.Update(latestSecrets)
	slog.Info("received initial endpoints", "frontends", len(latestFrontends), "backend_groups", len(latestBackends), "values", len(latestValues), "secrets", len(latestSecrets)) //nolint:gosec // G706: values are integer counts, not user strings

	// Set initial status counts.
	status.setEndpointCounts(len(latestFrontends), backendCountsMap(latestBackends))
	status.valuesCount = len(latestValues)
	status.secretsCount = len(latestSecrets)

	// Set initial endpoint gauges.
	metrics.Endpoints.WithLabelValues("frontend", cfg.ServiceName).Set(float64(len(latestFrontends)))
	for name, eps := range latestBackends {
		metrics.Endpoints.WithLabelValues("backend", name).Set(float64(len(eps)))
	}

	// Start broadcast server if configured.
	var bcast *broadcast.Server
	if cfg.BroadcastAddr != "" {
		bcast = broadcast.New(broadcast.Options{
			Addr:              cfg.BroadcastAddr,
			TargetPort:        cfg.BroadcastTargetPort,
			ServerIdleTimeout: cfg.BroadcastServerIdleTimeout,
			ReadHeaderTimeout: cfg.BroadcastReadHeaderTimeout,
			ClientTimeout:     cfg.BroadcastClientTimeout,
			ClientIdleTimeout: cfg.BroadcastClientIdleTimeout,
			ShutdownTimeout:   cfg.BroadcastShutdownTimeout,
			Metrics:           metrics,
		})
		bcast.SetFrontends(latestFrontends)
		go func() {
			err := bcast.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("broadcast server: %v", err)
			}
		}()
	}

	// Render VCL with real endpoint data and start varnishd.
	rend.SetBackendLabels(latestBackendLabels)
	renderStart := time.Now()
	initialVCLStr, err := rend.Render(latestFrontends, latestBackends, latestValues, latestSecrets)
	metrics.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
	if err != nil {
		log.Fatalf("initial render: %v", err) //nolint:gocritic // fatal before Start is intentional; deferred cancel is harmless to skip
	}
	initialVCLHash := sha256.Sum256([]byte(initialVCLStr))
	initialVCLFile, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
	if err != nil {
		log.Fatalf("creating initial VCL temp file: %v", err)
	}
	initialVCL := initialVCLFile.Name()
	_, err = initialVCLFile.WriteString(initialVCLStr)
	if err != nil {
		_ = initialVCLFile.Close()
		log.Fatalf("writing initial VCL temp file: %v", err)
	}
	err = initialVCLFile.Close()
	if err != nil {
		log.Fatalf("closing initial VCL temp file: %v", err)
	}
	defer func() { _ = os.Remove(initialVCL) }()

	err = mgr.Start(initialVCL)
	if err != nil {
		log.Fatalf("varnish start: %v", err)
	}
	metrics.VarnishdUp.Set(1)
	status.setVarnishdUp(true)
	if eventRecorder != nil && podRef != nil {
		eventRecorder.Event(podRef, v1.EventTypeNormal, "VCLReloaded", "Initial VCL loaded and varnishd started")
	}

	// Start varnishncsa access logging subprocess (opt-in).
	if cfg.VarnishncsaEnabled {
		mgr.StartNCSA(cfg.VarnishncsaPath, buildNCSAArgs(cfg), cfg.VarnishncsaPrefix)
		slog.Info("varnishncsa access logging enabled", "path", cfg.VarnishncsaPath) //nolint:gosec // G706: path from CLI flag, not runtime user input
	}

	// Watch VCL template file for changes.
	var templateCh <-chan struct{}
	if cfg.FileWatch {
		templateCh = watchFile(ctx, cfg.VCLTemplate, cfg.VCLTemplateWatchInterval)
	} else {
		slog.Info("file watching disabled, skipping VCL template and --values-dir watchers")
	}

	// Fan-in backend watcher updates to a single channel.
	var backendCh chan backendChange
	if len(bwWatchers) > 0 || len(discoveryWatchers) > 0 {
		backendCh = make(chan backendChange, len(bwWatchers)+len(discoveryWatchers)*8)
		for i, bw := range bwWatchers {
			name := bwNames[i]
			go func() {
				for eps := range bw.Changes() {
					backendCh <- backendChange{name: name, endpoints: eps, labels: bw.Labels()}
				}
			}()
		}
		for _, dw := range discoveryWatchers {
			go func() {
				for update := range dw.Changes() {
					backendCh <- backendChange{
						name:      update.Name,
						endpoints: update.Endpoints,
						labels:    update.Labels,
						removed:   update.Endpoints == nil,
					}
				}
			}()
		}
	}

	// Fan-in ConfigMapWatcher and FileValuesWatcher updates to a single channel.
	// When file watching is disabled, FileValuesWatcher goroutines are not
	// wired into valuesCh so disk changes won't trigger reloads. (The
	// watchers were still started above to provide initial directory state.)
	var valuesCh chan valuesChange
	fvwCount := len(fvwWatchers)
	if !cfg.FileWatch {
		fvwCount = 0
	}
	if len(vwWatchers) > 0 || fvwCount > 0 {
		valuesCh = make(chan valuesChange, len(vwWatchers)+fvwCount)
		for i, vw := range vwWatchers {
			name := vwNames[i]
			go func() {
				for data := range vw.Changes() {
					valuesCh <- valuesChange{name: name, data: data}
				}
			}()
		}
		if cfg.FileWatch {
			for i, fvw := range fvwWatchers {
				name := fvwNames[i]
				go func() {
					for data := range fvw.Changes() {
						valuesCh <- valuesChange{name: name, data: data}
					}
				}()
			}
		}
	}

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

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Main event loop with debounce.
	os.Exit(runLoop(ctx, cancel, loopConfig{
		rend:     rend,
		mgr:      mgr,
		bcast:    bcast,
		status:   status,
		metrics:  metrics,
		redactor: secretRedactor,

		frontendCh:  w.Changes(),
		backendCh:   backendCh,
		valuesCh:    valuesCh,
		secretsCh:   secretsCh,
		templateCh:  templateCh,
		sigCh:       sigCh,
		ncsaEvents:  mgr.NCSAEvents(),
		ncsaCrashed: mgr.NCSACrashed(),
		stopNCSA:    func() { mgr.StopNCSA() },

		serviceName:           cfg.ServiceName,
		frontendDebounce:      cfg.FrontendDebounce,
		frontendDebounceMax:   cfg.FrontendDebounceMax,
		backendDebounce:       cfg.BackendDebounce,
		backendDebounceMax:    cfg.BackendDebounceMax,
		shutdownTimeout:       cfg.ShutdownTimeout,
		broadcastDrainTimeout: cfg.BroadcastDrainTimeout,

		recorder: eventRecorder,
		podRef:   podRef,

		drainBackend:      drainBackendForLoop(cfg.Drain),
		drainDelay:        cfg.DrainDelay,
		drainPollInterval: cfg.DrainPollInterval,
		drainTimeout:      cfg.DrainTimeout,

		latestFrontends:     latestFrontends,
		latestBackends:      latestBackends,
		latestBackendLabels: latestBackendLabels,
		latestValues:        latestValues,
		latestSecrets:       latestSecrets,

		lastVCLHash: initialVCLHash,
	}))
}

// runLoop runs the main event loop, returning 0 for clean shutdown and 1 for error.
// rollbackReload re-renders VCL with the rolled-back template and reloads
// Varnish. Called after the primary reload fails on a new template.
func rollbackReload(lc *loopConfig, reasons string) {
	lc.rend.SetBackendLabels(lc.latestBackendLabels)
	renderStart := time.Now()
	vclStr, err := lc.rend.Render(lc.latestFrontends, lc.latestBackends, lc.latestValues, lc.latestSecrets)
	lc.metrics.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
	if err != nil {
		lc.metrics.VCLRenderErrorsTotal.Inc()
		slog.Error("render error after rollback", "error", err)
		emitEvent(lc, v1.EventTypeWarning, "VCLRenderFailed", fmt.Sprintf("VCL render error after rollback: %v", err))

		return
	}

	f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
	if err != nil {
		slog.Error("creating temp VCL file after rollback", "error", err)

		return
	}
	vclPath := f.Name()
	_, err = f.WriteString(vclStr)
	_ = f.Close()
	if err != nil {
		slog.Error("writing temp VCL file after rollback", "error", err)
		_ = os.Remove(vclPath) //nolint:gosec // G703: path from os.CreateTemp, not user input

		return
	}
	defer func() { _ = os.Remove(vclPath) }()

	err = lc.mgr.Reload(vclPath)
	if err != nil {
		lc.metrics.VCLReloadsTotal.WithLabelValues("error").Inc()
		slog.Error("reload error after rollback", "error", err)
		emitEvent(lc, v1.EventTypeWarning, "VCLReloadFailed", fmt.Sprintf("VCL reload failed after rollback: %v", err))

		return
	}
	lc.lastVCLHash = sha256.Sum256([]byte(vclStr))
	lc.metrics.VCLReloadsTotal.WithLabelValues("success").Inc()
	emitEvent(lc, v1.EventTypeNormal, "VCLReloaded", fmt.Sprintf("VCL reloaded successfully after rollback (%s)", reasons))
	if lc.status != nil {
		lc.status.recordReload()
		lc.status.setEndpointCounts(len(lc.latestFrontends), backendCountsMap(lc.latestBackends))
	}
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

//nolint:gocritic // hugeParam: called once per process; helpers already take &lc
func runLoop(_ context.Context, cancel context.CancelFunc, lc loopConfig) int {
	var (
		frontend        debounceState
		backend         debounceState
		pendingReload   bool
		templateChanged bool
		pendingReasons  []string
	)

	// handleReload executes the VCL reload logic, clearing both groups'
	// timers and deadlines. Returns true if a reload was attempted.
	handleReload := func() {
		// Clear both groups.
		if frontend.timer != nil {
			frontend.timer.Stop()
		}
		frontend.timer = nil
		frontend.deadline = time.Time{}
		frontend.firstEvent = time.Time{}
		frontend.capped = false
		if backend.timer != nil {
			backend.timer.Stop()
		}
		backend.timer = nil
		backend.deadline = time.Time{}
		backend.firstEvent = time.Time{}
		backend.capped = false

		if !pendingReload {
			return
		}
		pendingReload = false
		reasons := strings.Join(pendingReasons, ", ")
		pendingReasons = pendingReasons[:0]

		// If the template file changed, try to parse the new version.
		reloadedTemplate := false
		if templateChanged {
			err := lc.rend.Reload()
			if err != nil {
				lc.metrics.VCLTemplateParseErrorsTotal.Inc()
				slog.Error("template parse error, keeping old template", "error", err)
				emitEvent(&lc, v1.EventTypeWarning, "VCLTemplateParseFailed", fmt.Sprintf("Template parse error: %v", err))
			} else {
				reloadedTemplate = true
			}
		}

		templateChanged = false

		lc.rend.SetBackendLabels(lc.latestBackendLabels)
		renderStart := time.Now()
		vclStr, err := lc.rend.Render(lc.latestFrontends, lc.latestBackends, lc.latestValues, lc.latestSecrets)
		lc.metrics.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
		if err != nil {
			lc.metrics.VCLRenderErrorsTotal.Inc()
			slog.Error("render error", "error", err)
			emitEvent(&lc, v1.EventTypeWarning, "VCLRenderFailed", fmt.Sprintf("VCL render error: %v", err))
			if reloadedTemplate {
				lc.metrics.VCLRollbacksTotal.Inc()
				lc.rend.Rollback()
				emitEvent(&lc, v1.EventTypeWarning, "VCLRolledBack", "Template rollback after render error")
			}

			return
		}

		vclHash := sha256.Sum256([]byte(vclStr))
		if vclHash == lc.lastVCLHash {
			slog.Info("VCL unchanged, skipping reload", "reasons", reasons)
			lc.metrics.VCLReloadsTotal.WithLabelValues("skipped").Inc()

			return
		}

		f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
		if err != nil {
			slog.Error("creating temp VCL file", "error", err)

			return
		}
		vclPath := f.Name()
		_, err = f.WriteString(vclStr)
		_ = f.Close()
		if err != nil {
			slog.Error("writing temp VCL file", "error", err)
			_ = os.Remove(vclPath) //nolint:gosec // G703: path from os.CreateTemp, not user input

			return
		}

		err = lc.mgr.Reload(vclPath)
		if err != nil {
			lc.metrics.VCLReloadsTotal.WithLabelValues("error").Inc()
			slog.Error("reload error", "error", err)
			emitEvent(&lc, v1.EventTypeWarning, "VCLReloadFailed", fmt.Sprintf("VCL reload failed: %v", err))
			_ = os.Remove(vclPath) //nolint:gosec // G703: path from os.CreateTemp, not user input

			if !reloadedTemplate {
				return
			}

			// New template produced VCL that Varnish rejected; revert and
			// retry so that any concurrent frontend/backend changes still
			// take effect with the old (known-good) template.
			lc.metrics.VCLRollbacksTotal.Inc()
			lc.rend.Rollback()
			slog.Warn("rolled back to previous template")
			emitEvent(&lc, v1.EventTypeWarning, "VCLRolledBack", "Rolled back to previous template after reload failure")

			rollbackReload(&lc, reasons)

			return
		}

		lc.lastVCLHash = vclHash
		lc.metrics.VCLReloadsTotal.WithLabelValues("success").Inc()
		emitEvent(&lc, v1.EventTypeNormal, "VCLReloaded", fmt.Sprintf("VCL reloaded successfully (%s)", reasons))
		if lc.status != nil {
			lc.status.recordReload()
			lc.status.setEndpointCounts(len(lc.latestFrontends), backendCountsMap(lc.latestBackends))
		}
		_ = os.Remove(vclPath) //nolint:gosec // G703: path from os.CreateTemp, not user input
	}

	for {
		select {
		case <-lc.templateCh:
			lc.metrics.VCLTemplateChangesTotal.Inc()
			lc.metrics.DebounceEventsTotal.WithLabelValues("backend").Inc()
			slog.Info("VCL template changed on disk, scheduling reload")
			emitEvent(&lc, v1.EventTypeNormal, "VCLTemplateChanged", "VCL template file change detected on disk")
			pendingReload = true
			templateChanged = true
			pendingReasons = appendUnique(pendingReasons, "template changed")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case frontends := <-lc.frontendCh:
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
			epsChanged := !watcher.EndpointsEqual(lc.latestBackends[bc.name], bc.endpoints)
			if bc.removed {
				delete(lc.latestBackends, bc.name)
				delete(lc.latestBackendLabels, bc.name)
				lc.metrics.Endpoints.DeleteLabelValues("backend", bc.name)
			} else {
				lc.latestBackends[bc.name] = bc.endpoints
				lc.latestBackendLabels[bc.name] = bc.labels
				lc.metrics.Endpoints.WithLabelValues("backend", bc.name).Set(float64(len(bc.endpoints)))
			}
			lc.metrics.EndpointUpdatesTotal.WithLabelValues("backend", bc.name).Inc()
			lc.metrics.DebounceEventsTotal.WithLabelValues("backend").Inc()
			pendingReload = true
			if epsChanged {
				pendingReasons = appendUnique(pendingReasons, "backend endpoints changed")
			} else {
				pendingReasons = appendUnique(pendingReasons, "backend labels changed")
			}
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case vc := <-valuesChan(lc.valuesCh):
			lc.latestValues[vc.name] = vc.data
			lc.metrics.ValuesUpdatesTotal.WithLabelValues(vc.name).Inc()
			lc.metrics.DebounceEventsTotal.WithLabelValues("backend").Inc()
			slog.Info("values ConfigMap updated", "name", vc.name)
			pendingReload = true
			pendingReasons = appendUnique(pendingReasons, "values updated")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case sc := <-secretsChan(lc.secretsCh):
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

		case <-timerChan(frontend.timer):
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

		case ev := <-lc.ncsaEvents:
			emitEvent(&lc, ev.Type, ev.Reason, ev.Message)

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

		case sig := <-lc.sigCh:
			slog.Info("received signal, shutting down", "signal", sig)

			if lc.drainBackend != "" {
				handleDrain(&lc, sig)
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
			emitEvent(&lc, v1.EventTypeWarning, "VarnishdExited", fmt.Sprintf("varnishd exited unexpectedly: %v", lc.mgr.Err()))
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

// warnOnceEventSink wraps a record.EventSink and logs a single slog.Warn
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

// detectLocalZone looks up the node's topology.kubernetes.io/zone label and
// returns the zone string. Returns "" if nodeName is empty, the node cannot
// be fetched, or the label is absent.
func detectLocalZone(logger *slog.Logger, clientset kubernetes.Interface, nodeName string) string {
	if nodeName == "" {
		logger.Info("NODE_NAME not set, topology zone detection disabled (.LocalZone will be empty, .LocalBackends/.RemoteBackends will be empty in templates)")

		return ""
	}

	node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		logger.Warn("could not look up node for zone detection; .LocalZone will be empty, .LocalBackends/.RemoteBackends will be empty in templates",
			"node", nodeName, "error", err)

		return ""
	}

	zone := node.Labels["topology.kubernetes.io/zone"]
	if zone == "" {
		logger.Warn("node has no topology.kubernetes.io/zone label; .LocalZone will be empty, .LocalBackends/.RemoteBackends will be empty in templates",
			"node", nodeName)

		return ""
	}

	logger.Info("detected local topology zone", "zone", zone)

	return zone
}

func buildClientset() (kubernetes.Interface, error) {
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
	name      string
	endpoints []watcher.Endpoint
	labels    map[string]string
	removed   bool // true when a discovered service disappears
}

type valuesChange struct {
	name string
	data map[string]any
}

type secretsChange struct {
	name string
	data map[string]any
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

// secretsChan returns ch, or nil if ch is nil (so select skips it).
func secretsChan(ch chan secretsChange) <-chan secretsChange {
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

// watchFile polls a file for content changes and sends on the returned channel
// when a change is detected. The goroutine exits when ctx is cancelled.
func watchFile(ctx context.Context, path string, interval time.Duration) <-chan struct{} {
	ch := make(chan struct{}, 1)

	go func() {
		last, _ := os.ReadFile(path)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := os.ReadFile(path)
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
