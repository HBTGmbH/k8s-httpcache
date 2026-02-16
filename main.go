package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"k8s-httpcache/internal/config"
	"k8s-httpcache/internal/renderer"
	"k8s-httpcache/internal/varnish"
	"k8s-httpcache/internal/watcher"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("k8s-httpcache: ")

	cfg, err := config.Parse()
	if err != nil {
		log.Fatalf("config: %v", err)
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
			log.Printf("watcher error: %v", err)
		}
	}()

	// Start backend watchers.
	var (
		bwNames    []string
		bwWatchers []*watcher.Watcher
	)
	for _, b := range cfg.Backends {
		bw := watcher.New(clientset, b.Namespace, b.ServiceName, b.Port)
		name := b.Name
		go func() {
			if err := bw.Run(ctx); err != nil {
				log.Printf("backend watcher %s error: %v", name, err)
			}
		}()
		bwNames = append(bwNames, name)
		bwWatchers = append(bwWatchers, bw)
	}

	// Collect the initial endpoint snapshot from every watcher before
	// starting varnishd, so it launches with a complete configuration.
	// The watcher guarantees at least one send after cache sync (even if
	// the endpoint list is empty), so this will not deadlock.
	log.Printf("waiting for initial endpoint data")
	latestFrontends := <-w.Changes()
	latestBackends := make(map[string][]watcher.Endpoint)
	for i, bw := range bwWatchers {
		latestBackends[bwNames[i]] = <-bw.Changes()
	}
	log.Printf("received initial endpoints: %d frontends, %d backend groups", len(latestFrontends), len(latestBackends))

	// Render VCL with real endpoint data and start varnishd.
	initialVCL, err := rend.RenderToFile(latestFrontends, latestBackends)
	if err != nil {
		log.Fatalf("initial render: %v", err)
	}
	defer os.Remove(initialVCL)

	mgr := varnish.New(cfg.VarnishdPath, cfg.VarnishadmPath, cfg.AdminAddr, cfg.ListenAddr, cfg.SecretPath, cfg.ExtraVarnishd)
	defer mgr.Cleanup()

	if err := mgr.Start(initialVCL); err != nil {
		log.Fatalf("varnish start: %v", err)
	}

	// Watch VCL template file for changes.
	templateCh := watchFile(ctx, cfg.VCLTemplate, 5*time.Second)

	// Fan-in backend watcher updates to a single channel.
	var backendCh chan backendChange
	if len(bwWatchers) > 0 {
		backendCh = make(chan backendChange, len(bwWatchers))
		for i, bw := range bwWatchers {
			name := bwNames[i]
			bw := bw
			go func() {
				for eps := range bw.Changes() {
					backendCh <- backendChange{name: name, endpoints: eps}
				}
			}()
		}
	}

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Main event loop with debounce.
	var (
		debounceTimer   *time.Timer
		pendingReload   bool
		templateChanged bool
	)

	for {
		select {
		case <-templateCh:
			log.Printf("VCL template changed on disk, scheduling reload")
			pendingReload = true
			templateChanged = true

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(cfg.Debounce)

		case frontends := <-w.Changes():
			latestFrontends = frontends
			pendingReload = true

			// Reset debounce timer.
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(cfg.Debounce)

		case bc := <-backendChan(backendCh):
			latestBackends[bc.name] = bc.endpoints
			pendingReload = true

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(cfg.Debounce)

		case <-timerChan(debounceTimer):
			debounceTimer = nil
			if !pendingReload {
				continue
			}
			pendingReload = false

			// If the template file changed, try to parse the new version.
			reloadedTemplate := false
			if templateChanged {
				if err := rend.Reload(); err != nil {
					log.Printf("template parse error (keeping old template): %v", err)
					templateChanged = false
				} else {
					reloadedTemplate = true
				}
			}

			if len(latestFrontends) == 0 && !templateChanged {
				templateChanged = false
				log.Printf("skipping reload: no ready endpoints")
				continue
			}
			templateChanged = false

			vclPath, err := rend.RenderToFile(latestFrontends, latestBackends)
			if err != nil {
				log.Printf("render error: %v", err)
				if reloadedTemplate {
					rend.Rollback()
				}
				continue
			}

			if err := mgr.Reload(vclPath); err != nil {
				log.Printf("reload error: %v", err)
				os.Remove(vclPath)

				if !reloadedTemplate {
					continue
				}

				// New template produced VCL that Varnish rejected; revert and
				// retry so that any concurrent frontend/backend changes still
				// take effect with the old (known-good) template.
				rend.Rollback()
				log.Printf("rolled back to previous template")

				vclPath, err = rend.RenderToFile(latestFrontends, latestBackends)
				if err != nil {
					log.Printf("render error after rollback: %v", err)
					continue
				}
				if err := mgr.Reload(vclPath); err != nil {
					log.Printf("reload error after rollback: %v", err)
				}
				os.Remove(vclPath)
				continue
			}

			os.Remove(vclPath)

		case sig := <-sigCh:
			log.Printf("received signal %v, shutting down", sig)
			cancel()
			mgr.ForwardSignal(sig)

			select {
			case <-mgr.Done():
			case <-time.After(cfg.ShutdownTimeout):
				log.Printf("varnishd did not exit in time, forcing")
				mgr.ForwardSignal(syscall.SIGKILL)
			}

			if err := mgr.Err(); err != nil {
				log.Printf("varnishd exited with error: %v", err)
				os.Exit(1)
			}
			os.Exit(0)

		case <-mgr.Done():
			log.Printf("varnishd exited unexpectedly: %v", mgr.Err())
			cancel()
			os.Exit(1)
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

// backendChan returns ch, or nil if ch is nil (so select skips it).
func backendChan(ch chan backendChange) <-chan backendChange {
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
