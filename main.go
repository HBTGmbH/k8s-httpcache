// k8s-httpcache is a Kubernetes-native HTTP caching proxy built on Varnish.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	"k8s-httpcache/internal/broadcast"
	"k8s-httpcache/internal/config"
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
	RenderToFile([]watcher.Frontend, map[string][]watcher.Endpoint, map[string]map[string]any) (string, error)
	Rollback()
}

// varnishProcess abstracts varnishd lifecycle (satisfied by *varnish.Manager).
type varnishProcess interface {
	Reload(vclPath string) error
	Done() <-chan struct{}
	Err() error
	ForwardSignal(sig os.Signal)
	MarkBackendSick(name string) error
	ActiveSessions() (int64, error)
}

// broadcaster abstracts broadcast server operations (satisfied by *broadcast.Server).
type broadcaster interface {
	SetFrontends([]watcher.Frontend)
	Drain(time.Duration) error
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
	rend   vclRenderer
	mgr    varnishProcess
	bcast  broadcaster  // nil when broadcast is disabled
	status *statusStore // nil when status endpoint is disabled

	frontendCh <-chan []watcher.Frontend
	backendCh  chan backendChange
	valuesCh   chan valuesChange
	templateCh <-chan struct{}
	sigCh      <-chan os.Signal

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

	latestFrontends []watcher.Frontend
	latestBackends  map[string][]watcher.Endpoint
	latestValues    map[string]map[string]any
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

	// Register configurable histogram before any metrics are observed.
	telemetry.RegisterDebounceLatency(cfg.DebounceLatencyBuckets)

	// Set build info metric.
	telemetry.BuildInfo.WithLabelValues(version, runtime.Version()).Set(1)

	// Create status store for the /status endpoint.
	status := &statusStore{
		version:   version,
		goVersion: runtime.Version(),
		startedAt: time.Now(),
	}

	// Start Prometheus metrics server if configured.
	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/status", statusHandler(status))
		metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: cfg.MetricsReadHeaderTimeout}
		go func() {
			slog.Info("starting metrics server", "addr", cfg.MetricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		if pod, err := clientset.CoreV1().Pods(cfg.ServiceNamespace).Get(context.Background(), podName, metav1.GetOptions{}); err != nil {
			slog.Warn("could not look up pod UID for event recording; events may not appear in kubectl describe pod", "error", err)
		} else {
			podRef.UID = pod.UID
		}
		slog.Info("kubernetes event recorder enabled", "pod", podName, "namespace", cfg.ServiceNamespace)
	} else {
		slog.Info("POD_NAME not set, kubernetes event recording disabled")
	}

	// Create varnish manager and detect version before rendering VCL,
	// because the renderer needs the version to generate compatible VCL.
	listenAddrs := make([]string, len(cfg.ListenAddrs))
	for i, la := range cfg.ListenAddrs {
		listenAddrs[i] = la.Raw
	}
	mgr := varnish.New(cfg.VarnishdPath, cfg.VarnishadmPath, listenAddrs, cfg.ExtraVarnishd, cfg.VarnishstatPath)
	mgr.AdminTimeout = cfg.AdminTimeout
	mgr.ReloadRetries = cfg.VCLReloadRetries
	mgr.ReloadRetryInterval = cfg.VCLReloadRetryInterval
	mgr.VCLKept = cfg.VCLKept

	if err := mgr.DetectVersion(); err != nil {
		log.Fatalf("varnish version: %v", err)
	}
	status.varnishMajorVersion = mgr.MajorVersion()

	// Parse VCL template.
	rend, err := renderer.New(cfg.VCLTemplate, cfg.TemplateDelimLeft, cfg.TemplateDelimRight)
	if err != nil {
		log.Fatalf("renderer: %v", err)
	}
	if cfg.Drain {
		rend.SetDrainBackend(drainBackendName)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start frontend watcher.
	w := watcher.New(clientset, cfg.ServiceNamespace, cfg.ServiceName, "")
	go func() {
		if err := w.Run(ctx); err != nil {
			slog.Error("watcher error", "error", err)
		}
	}()

	// Start backend watchers.
	var (
		bwNames    []string
		bwWatchers []*watcher.BackendWatcher
	)
	for _, b := range cfg.Backends {
		bw := watcher.NewBackendWatcher(clientset, b.Namespace, b.ServiceName, b.Port)
		name := b.Name
		go func() {
			if err := bw.Run(ctx); err != nil {
				slog.Error("backend watcher error", "backend", name, "error", err)
			}
		}()
		bwNames = append(bwNames, name)
		bwWatchers = append(bwWatchers, bw)
	}

	// Start ConfigMap watchers for --values.
	var (
		vwNames    []string
		vwWatchers []*watcher.ConfigMapWatcher
	)
	for _, v := range cfg.Values {
		vw := watcher.NewConfigMapWatcher(clientset, v.Namespace, v.ConfigMapName)
		name := v.Name
		go func() {
			if err := vw.Run(ctx); err != nil {
				slog.Error("values watcher error", "values", name, "error", err)
			}
		}()
		vwNames = append(vwNames, name)
		vwWatchers = append(vwWatchers, vw)
	}

	// Start directory watchers for --values-dir.
	var (
		fvwNames    []string
		fvwWatchers []*watcher.FileValuesWatcher
	)
	for _, vd := range cfg.ValuesDirs {
		fvw := watcher.NewFileValuesWatcher(vd.Path, cfg.ValuesDirPollInterval)
		name := vd.Name
		go func() {
			if err := fvw.Run(ctx); err != nil {
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
		slog.Warn("frontend Service has no ready endpoints at startup",
			"namespace", cfg.ServiceNamespace, "service", cfg.ServiceName)
	}
	latestBackends := make(map[string][]watcher.Endpoint)
	for i, bw := range bwWatchers {
		latestBackends[bwNames[i]] = <-bw.Changes()
	}
	latestValues := make(map[string]map[string]any)
	for i, vw := range vwWatchers {
		latestValues[vwNames[i]] = <-vw.Changes()
	}
	for i, fvw := range fvwWatchers {
		latestValues[fvwNames[i]] = <-fvw.Changes()
	}
	slog.Info("received initial endpoints", "frontends", len(latestFrontends), "backend_groups", len(latestBackends), "values", len(latestValues))

	// Set initial status counts.
	status.setEndpointCounts(len(latestFrontends), backendCountsMap(latestBackends))
	status.valuesCount = len(latestValues)

	// Set initial endpoint gauges.
	telemetry.Endpoints.WithLabelValues("frontend", cfg.ServiceName).Set(float64(len(latestFrontends)))
	for name, eps := range latestBackends {
		telemetry.Endpoints.WithLabelValues("backend", name).Set(float64(len(eps)))
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
		})
		bcast.SetFrontends(latestFrontends)
		go func() {
			if err := bcast.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("broadcast server: %v", err)
			}
		}()
	}

	// Render VCL with real endpoint data and start varnishd.
	renderStart := time.Now()
	initialVCL, err := rend.RenderToFile(latestFrontends, latestBackends, latestValues)
	telemetry.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
	if err != nil {
		log.Fatalf("initial render: %v", err) //nolint:gocritic // fatal before Start is intentional; deferred cancel is harmless to skip
	}
	defer func() { _ = os.Remove(initialVCL) }()

	if err := mgr.Start(initialVCL); err != nil {
		log.Fatalf("varnish start: %v", err)
	}
	telemetry.VarnishdUp.Set(1)
	status.setVarnishdUp(true)
	if eventRecorder != nil && podRef != nil {
		eventRecorder.Event(podRef, v1.EventTypeNormal, "VCLReloaded", "Initial VCL loaded and varnishd started")
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
	if len(bwWatchers) > 0 {
		backendCh = make(chan backendChange, len(bwWatchers))
		for i, bw := range bwWatchers {
			name := bwNames[i]
			go func() {
				for eps := range bw.Changes() {
					backendCh <- backendChange{name: name, endpoints: eps}
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

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Main event loop with debounce.
	os.Exit(runLoop(ctx, cancel, loopConfig{
		rend:   rend,
		mgr:    mgr,
		bcast:  bcast,
		status: status,

		frontendCh: w.Changes(),
		backendCh:  backendCh,
		valuesCh:   valuesCh,
		templateCh: templateCh,
		sigCh:      sigCh,

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

		latestFrontends: latestFrontends,
		latestBackends:  latestBackends,
		latestValues:    latestValues,
	}))
}

// runLoop runs the main event loop, returning 0 for clean shutdown and 1 for error.
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
			if err := lc.rend.Reload(); err != nil {
				telemetry.VCLTemplateParseErrorsTotal.Inc()
				slog.Error("template parse error, keeping old template", "error", err)
				emitEvent(&lc, v1.EventTypeWarning, "VCLTemplateParseFailed", fmt.Sprintf("Template parse error: %v", err))
			} else {
				reloadedTemplate = true
			}
		}

		templateChanged = false

		renderStart := time.Now()
		vclPath, err := lc.rend.RenderToFile(lc.latestFrontends, lc.latestBackends, lc.latestValues)
		telemetry.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
		if err != nil {
			telemetry.VCLRenderErrorsTotal.Inc()
			slog.Error("render error", "error", err)
			emitEvent(&lc, v1.EventTypeWarning, "VCLRenderFailed", fmt.Sprintf("VCL render error: %v", err))
			if reloadedTemplate {
				telemetry.VCLRollbacksTotal.Inc()
				lc.rend.Rollback()
				emitEvent(&lc, v1.EventTypeWarning, "VCLRolledBack", "Template rollback after render error")
			}
			return
		}

		if err := lc.mgr.Reload(vclPath); err != nil {
			telemetry.VCLReloadsTotal.WithLabelValues("error").Inc()
			slog.Error("reload error", "error", err)
			emitEvent(&lc, v1.EventTypeWarning, "VCLReloadFailed", fmt.Sprintf("VCL reload failed: %v", err))
			_ = os.Remove(vclPath)

			if !reloadedTemplate {
				return
			}

			// New template produced VCL that Varnish rejected; revert and
			// retry so that any concurrent frontend/backend changes still
			// take effect with the old (known-good) template.
			telemetry.VCLRollbacksTotal.Inc()
			lc.rend.Rollback()
			slog.Warn("rolled back to previous template")
			emitEvent(&lc, v1.EventTypeWarning, "VCLRolledBack", "Rolled back to previous template after reload failure")

			renderStart = time.Now()
			vclPath, err = lc.rend.RenderToFile(lc.latestFrontends, lc.latestBackends, lc.latestValues)
			telemetry.VCLRenderDurationSeconds.Observe(time.Since(renderStart).Seconds())
			if err != nil {
				telemetry.VCLRenderErrorsTotal.Inc()
				slog.Error("render error after rollback", "error", err)
				emitEvent(&lc, v1.EventTypeWarning, "VCLRenderFailed", fmt.Sprintf("VCL render error after rollback: %v", err))
				return
			}
			if err := lc.mgr.Reload(vclPath); err != nil {
				telemetry.VCLReloadsTotal.WithLabelValues("error").Inc()
				slog.Error("reload error after rollback", "error", err)
				emitEvent(&lc, v1.EventTypeWarning, "VCLReloadFailed", fmt.Sprintf("VCL reload failed after rollback: %v", err))
			} else {
				telemetry.VCLReloadsTotal.WithLabelValues("success").Inc()
				emitEvent(&lc, v1.EventTypeNormal, "VCLReloaded", fmt.Sprintf("VCL reloaded successfully after rollback (%s)", reasons))
				if lc.status != nil {
					lc.status.recordReload()
					lc.status.setEndpointCounts(len(lc.latestFrontends), backendCountsMap(lc.latestBackends))
				}
			}
			_ = os.Remove(vclPath)
			return
		}

		telemetry.VCLReloadsTotal.WithLabelValues("success").Inc()
		emitEvent(&lc, v1.EventTypeNormal, "VCLReloaded", fmt.Sprintf("VCL reloaded successfully (%s)", reasons))
		if lc.status != nil {
			lc.status.recordReload()
			lc.status.setEndpointCounts(len(lc.latestFrontends), backendCountsMap(lc.latestBackends))
		}
		_ = os.Remove(vclPath)
	}

	for {
		select {
		case <-lc.templateCh:
			telemetry.VCLTemplateChangesTotal.Inc()
			telemetry.DebounceEventsTotal.WithLabelValues("backend").Inc()
			slog.Info("VCL template changed on disk, scheduling reload")
			emitEvent(&lc, v1.EventTypeNormal, "VCLTemplateChanged", "VCL template file change detected on disk")
			pendingReload = true
			templateChanged = true
			pendingReasons = appendUnique(pendingReasons, "template changed")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case frontends := <-lc.frontendCh:
			lc.latestFrontends = frontends
			telemetry.EndpointUpdatesTotal.WithLabelValues("frontend", lc.serviceName).Inc()
			telemetry.Endpoints.WithLabelValues("frontend", lc.serviceName).Set(float64(len(lc.latestFrontends)))
			telemetry.DebounceEventsTotal.WithLabelValues("frontend").Inc()
			if lc.bcast != nil {
				lc.bcast.SetFrontends(lc.latestFrontends)
			}
			pendingReload = true
			pendingReasons = appendUnique(pendingReasons, "frontend endpoints changed")
			resetDebounce(&frontend, lc.frontendDebounce, lc.frontendDebounceMax)

		case bc := <-backendChan(lc.backendCh):
			lc.latestBackends[bc.name] = bc.endpoints
			telemetry.EndpointUpdatesTotal.WithLabelValues("backend", bc.name).Inc()
			telemetry.Endpoints.WithLabelValues("backend", bc.name).Set(float64(len(bc.endpoints)))
			telemetry.DebounceEventsTotal.WithLabelValues("backend").Inc()
			pendingReload = true
			pendingReasons = appendUnique(pendingReasons, "backend endpoints changed")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case vc := <-valuesChan(lc.valuesCh):
			lc.latestValues[vc.name] = vc.data
			telemetry.ValuesUpdatesTotal.WithLabelValues(vc.name).Inc()
			telemetry.DebounceEventsTotal.WithLabelValues("backend").Inc()
			slog.Info("values ConfigMap updated", "name", vc.name)
			pendingReload = true
			pendingReasons = appendUnique(pendingReasons, "values updated")
			resetDebounce(&backend, lc.backendDebounce, lc.backendDebounceMax)

		case <-timerChan(frontend.timer):
			telemetry.DebounceFiresTotal.WithLabelValues("frontend").Inc()
			if frontend.capped {
				telemetry.DebounceMaxEnforcementsTotal.WithLabelValues("frontend").Inc()
			}
			if !frontend.firstEvent.IsZero() {
				telemetry.DebounceLatencySeconds.WithLabelValues("frontend").Observe(time.Since(frontend.firstEvent).Seconds())
			}
			if !backend.firstEvent.IsZero() {
				telemetry.DebounceLatencySeconds.WithLabelValues("backend").Observe(time.Since(backend.firstEvent).Seconds())
			}
			handleReload()

		case <-timerChan(backend.timer):
			telemetry.DebounceFiresTotal.WithLabelValues("backend").Inc()
			if backend.capped {
				telemetry.DebounceMaxEnforcementsTotal.WithLabelValues("backend").Inc()
			}
			if !backend.firstEvent.IsZero() {
				telemetry.DebounceLatencySeconds.WithLabelValues("backend").Observe(time.Since(backend.firstEvent).Seconds())
			}
			if !frontend.firstEvent.IsZero() {
				telemetry.DebounceLatencySeconds.WithLabelValues("frontend").Observe(time.Since(frontend.firstEvent).Seconds())
			}
			handleReload()

		case sig := <-lc.sigCh:
			slog.Info("received signal, shutting down", "signal", sig)

			if lc.drainBackend != "" {
				emitEvent(&lc, v1.EventTypeNormal, "DrainStarted", fmt.Sprintf("Received %v, starting graceful drain", sig))
				// Mark the VCL backend sick so Varnish starts responding
				// with Connection: close, causing clients to disconnect.
				if err := lc.mgr.MarkBackendSick(lc.drainBackend); err != nil {
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
				if !interrupted && lc.drainTimeout > 0 {
					deadline := time.After(lc.drainTimeout)
					ticker := time.NewTicker(lc.drainPollInterval)
				drainLoop:
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
								emitEvent(&lc, v1.EventTypeNormal, "DrainCompleted", "All connections drained")
								break drainLoop
							}
						case <-deadline:
							slog.Warn("drain timeout reached, proceeding with shutdown")
							emitEvent(&lc, v1.EventTypeWarning, "DrainTimeout", "Drain timeout reached, proceeding with forced shutdown")
							break drainLoop
						case <-lc.sigCh:
							slog.Info("second signal received, aborting drain")
							break drainLoop
						}
					}
					ticker.Stop()
				}
			}

			if lc.bcast != nil {
				_ = lc.bcast.Drain(lc.broadcastDrainTimeout)
			}

			cancel()
			lc.mgr.ForwardSignal(sig)

			select {
			case <-lc.mgr.Done():
			case <-time.After(lc.shutdownTimeout):
				slog.Warn("varnishd did not exit in time, forcing")
				lc.mgr.ForwardSignal(syscall.SIGKILL)
			}

			if err := lc.mgr.Err(); err != nil {
				slog.Error("varnishd exited with error", "error", err)
				return 1
			}
			return 0

		case <-lc.mgr.Done():
			telemetry.VarnishdUp.Set(0)
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

func (s *warnOnceEventSink) checkErr(err error) {
	if err != nil && apierrors.IsForbidden(err) {
		s.warned.Do(func() {
			slog.Warn("cannot create Kubernetes Events (RBAC permission denied); "+
				"grant 'create' and 'patch' on 'events' to silence this warning",
				"error", err)
		})
	}
}

func (s *warnOnceEventSink) Create(event *v1.Event) (*v1.Event, error) {
	ev, err := s.inner.Create(event)
	s.checkErr(err)
	return ev, err
}

func (s *warnOnceEventSink) Update(event *v1.Event) (*v1.Event, error) {
	ev, err := s.inner.Update(event)
	s.checkErr(err)
	return ev, err
}

func (s *warnOnceEventSink) Patch(oldEvent *v1.Event, data []byte) (*v1.Event, error) {
	ev, err := s.inner.Patch(oldEvent, data)
	s.checkErr(err)
	return ev, err
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
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}

type backendChange struct {
	name      string
	endpoints []watcher.Endpoint
}

type valuesChange struct {
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
