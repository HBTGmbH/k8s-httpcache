// k8s-httpcache is a Kubernetes-native HTTP caching proxy built on Varnish.
package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"k8s-httpcache/internal/broadcast"
	"k8s-httpcache/internal/config"
	"k8s-httpcache/internal/renderer"
	"k8s-httpcache/internal/telemetry"
	"k8s-httpcache/internal/varnish"
	"k8s-httpcache/internal/watcher"
)

// version is set at build time via -ldflags.
var version = "dev"

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
}

// broadcaster abstracts broadcast server operations (satisfied by *broadcast.Server).
type broadcaster interface {
	SetFrontends([]watcher.Frontend)
	Drain(time.Duration) error
}

// loopConfig holds all inputs for the main event loop.
type loopConfig struct {
	rend  vclRenderer
	mgr   varnishProcess
	bcast broadcaster // nil when broadcast is disabled

	frontendCh <-chan []watcher.Frontend
	backendCh  chan backendChange
	valuesCh   chan valuesChange
	templateCh <-chan struct{}
	sigCh      <-chan os.Signal

	serviceName           string
	debounce              time.Duration
	shutdownTimeout       time.Duration
	broadcastDrainTimeout time.Duration

	latestFrontends []watcher.Frontend
	latestBackends  map[string][]watcher.Endpoint
	latestValues    map[string]map[string]any
}

func main() {
	cfg, err := config.Parse()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	logOpts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var logHandler slog.Handler
	if cfg.LogFormat == "json" {
		logHandler = slog.NewJSONHandler(os.Stderr, logOpts)
	} else {
		logHandler = slog.NewTextHandler(os.Stderr, logOpts)
	}
	slog.SetDefault(slog.New(logHandler))

	// Set build info metric.
	telemetry.BuildInfo.WithLabelValues(version, runtime.Version()).Set(1)

	// Start Prometheus metrics server if configured.
	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
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

	// Parse VCL template.
	rend, err := renderer.New(cfg.VCLTemplate)
	if err != nil {
		log.Fatalf("renderer: %v", err)
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
	initialVCL, err := rend.RenderToFile(latestFrontends, latestBackends, latestValues)
	if err != nil {
		log.Fatalf("initial render: %v", err) //nolint:gocritic // startup fatal; process exits, no cleanup needed
	}
	defer func() { _ = os.Remove(initialVCL) }()

	listenAddrs := make([]string, len(cfg.ListenAddrs))
	for i, la := range cfg.ListenAddrs {
		listenAddrs[i] = la.Raw
	}
	mgr := varnish.New(cfg.VarnishdPath, cfg.VarnishadmPath, cfg.AdminAddr, listenAddrs, cfg.SecretPath, cfg.ExtraVarnishd)
	defer mgr.Cleanup()

	if err := mgr.Start(initialVCL); err != nil {
		log.Fatalf("varnish start: %v", err)
	}
	telemetry.VarnishdUp.Set(1)

	// Watch VCL template file for changes.
	templateCh := watchFile(ctx, cfg.VCLTemplate, 5*time.Second)

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
	var valuesCh chan valuesChange
	if len(vwWatchers) > 0 || len(fvwWatchers) > 0 {
		valuesCh = make(chan valuesChange, len(vwWatchers)+len(fvwWatchers))
		for i, vw := range vwWatchers {
			name := vwNames[i]
			go func() {
				for data := range vw.Changes() {
					valuesCh <- valuesChange{name: name, data: data}
				}
			}()
		}
		for i, fvw := range fvwWatchers {
			name := fvwNames[i]
			go func() {
				for data := range fvw.Changes() {
					valuesCh <- valuesChange{name: name, data: data}
				}
			}()
		}
	}

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Main event loop with debounce.
	os.Exit(runLoop(ctx, cancel, loopConfig{
		rend:  rend,
		mgr:   mgr,
		bcast: bcast,

		frontendCh: w.Changes(),
		backendCh:  backendCh,
		valuesCh:   valuesCh,
		templateCh: templateCh,
		sigCh:      sigCh,

		serviceName:           cfg.ServiceName,
		debounce:              cfg.Debounce,
		shutdownTimeout:       cfg.ShutdownTimeout,
		broadcastDrainTimeout: cfg.BroadcastDrainTimeout,

		latestFrontends: latestFrontends,
		latestBackends:  latestBackends,
		latestValues:    latestValues,
	}))
}

// runLoop runs the main event loop, returning 0 for clean shutdown and 1 for error.
func runLoop(_ context.Context, cancel context.CancelFunc, lc loopConfig) int {
	var (
		debounceTimer   *time.Timer
		pendingReload   bool
		templateChanged bool
	)

	for {
		select {
		case <-lc.templateCh:
			telemetry.VCLTemplateChangesTotal.Inc()
			slog.Info("VCL template changed on disk, scheduling reload")
			pendingReload = true
			templateChanged = true

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(lc.debounce)

		case frontends := <-lc.frontendCh:
			lc.latestFrontends = frontends
			telemetry.EndpointUpdatesTotal.WithLabelValues("frontend", lc.serviceName).Inc()
			telemetry.Endpoints.WithLabelValues("frontend", lc.serviceName).Set(float64(len(lc.latestFrontends)))
			if lc.bcast != nil {
				lc.bcast.SetFrontends(lc.latestFrontends)
			}
			pendingReload = true

			// Reset debounce timer.
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(lc.debounce)

		case bc := <-backendChan(lc.backendCh):
			lc.latestBackends[bc.name] = bc.endpoints
			telemetry.EndpointUpdatesTotal.WithLabelValues("backend", bc.name).Inc()
			telemetry.Endpoints.WithLabelValues("backend", bc.name).Set(float64(len(bc.endpoints)))
			pendingReload = true

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(lc.debounce)

		case vc := <-valuesChan(lc.valuesCh):
			lc.latestValues[vc.name] = vc.data
			telemetry.ValuesUpdatesTotal.WithLabelValues(vc.name).Inc()
			slog.Info("values ConfigMap updated", "name", vc.name)
			pendingReload = true

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(lc.debounce)

		case <-timerChan(debounceTimer):
			debounceTimer = nil
			if !pendingReload {
				continue
			}
			pendingReload = false

			// If the template file changed, try to parse the new version.
			reloadedTemplate := false
			if templateChanged {
				if err := lc.rend.Reload(); err != nil {
					telemetry.VCLTemplateParseErrorsTotal.Inc()
					slog.Error("template parse error, keeping old template", "error", err)
				} else {
					reloadedTemplate = true
				}
			}

			templateChanged = false

			vclPath, err := lc.rend.RenderToFile(lc.latestFrontends, lc.latestBackends, lc.latestValues)
			if err != nil {
				telemetry.VCLRenderErrorsTotal.Inc()
				slog.Error("render error", "error", err)
				if reloadedTemplate {
					telemetry.VCLRollbacksTotal.Inc()
					lc.rend.Rollback()
				}
				continue
			}

			if err := lc.mgr.Reload(vclPath); err != nil {
				telemetry.VCLReloadsTotal.WithLabelValues("error").Inc()
				slog.Error("reload error", "error", err)
				_ = os.Remove(vclPath)

				if !reloadedTemplate {
					continue
				}

				// New template produced VCL that Varnish rejected; revert and
				// retry so that any concurrent frontend/backend changes still
				// take effect with the old (known-good) template.
				telemetry.VCLRollbacksTotal.Inc()
				lc.rend.Rollback()
				slog.Warn("rolled back to previous template")

				vclPath, err = lc.rend.RenderToFile(lc.latestFrontends, lc.latestBackends, lc.latestValues)
				if err != nil {
					telemetry.VCLRenderErrorsTotal.Inc()
					slog.Error("render error after rollback", "error", err)
					continue
				}
				if err := lc.mgr.Reload(vclPath); err != nil {
					telemetry.VCLReloadsTotal.WithLabelValues("error").Inc()
					slog.Error("reload error after rollback", "error", err)
				} else {
					telemetry.VCLReloadsTotal.WithLabelValues("success").Inc()
				}
				_ = os.Remove(vclPath)
				continue
			}

			telemetry.VCLReloadsTotal.WithLabelValues("success").Inc()
			_ = os.Remove(vclPath)

		case sig := <-lc.sigCh:
			slog.Info("received signal, shutting down", "signal", sig)
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
			slog.Error("varnishd exited unexpectedly", "error", lc.mgr.Err())
			cancel()
			return 1
		}
	}
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

// timerChan returns the timer's channel, or nil if the timer is nil.
func timerChan(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
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
