package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// BackendUpdate carries an endpoint change for a discovered backend service.
// Endpoints is nil when the service was removed.
type BackendUpdate struct {
	Name        string
	Endpoints   []Endpoint        // nil = removed
	Labels      map[string]string // Service labels at the time of the update
	Annotations map[string]string // Service annotations at the time of the update
}

// managedBackend tracks a child BackendWatcher spawned for a discovered Service.
type managedBackend struct {
	watcher   *BackendWatcher
	cancel    context.CancelFunc
	fwdCancel context.CancelFunc // stops the forwarding goroutine (nil before forwarding starts)
	namespace string
	name      string
}

// BackendDiscoveryWatcher watches Services matching a label selector and
// manages child BackendWatchers for each discovered Service. Updates are
// emitted on a single channel as BackendUpdate values.
type BackendDiscoveryWatcher struct {
	clientset         kubernetes.Interface
	namespace         string // empty when allNamespaces=true
	allNamespaces     bool
	selector          labels.Selector
	portOverride      string
	explicitNames     map[string]bool // names reserved by --backend flags
	excludeAnnotation func(string) bool
	log               *slog.Logger

	mu          sync.Mutex
	backends    map[string]*managedBackend // keyed by "namespace/serviceName"
	initialized bool                       // true after initial sync; enables forwarding in syncServices

	updateCh           chan BackendUpdate
	initialState       map[string][]Endpoint
	initialLabels      map[string]map[string]string
	initialAnnotations map[string]map[string]string
	initialCh          chan struct{} // closed after initial sync
}

// NewBackendDiscoveryWatcher creates a new discovery watcher.
func NewBackendDiscoveryWatcher(
	clientset kubernetes.Interface,
	namespace string,
	allNamespaces bool,
	selector labels.Selector,
	portOverride string,
	explicitNames map[string]bool,
) *BackendDiscoveryWatcher {
	return &BackendDiscoveryWatcher{
		clientset:          clientset,
		namespace:          namespace,
		allNamespaces:      allNamespaces,
		selector:           selector,
		portOverride:       portOverride,
		explicitNames:      explicitNames,
		log:                slog.Default(),
		backends:           make(map[string]*managedBackend),
		updateCh:           make(chan BackendUpdate, 16),
		initialState:       make(map[string][]Endpoint),
		initialLabels:      make(map[string]map[string]string),
		initialAnnotations: make(map[string]map[string]string),
		initialCh:          make(chan struct{}),
	}
}

// SetExcludeAnnotations sets the annotation exclusion filter that will be
// propagated to all child BackendWatchers. Must be called before Run.
func (dw *BackendDiscoveryWatcher) SetExcludeAnnotations(exclude func(string) bool) {
	dw.excludeAnnotation = exclude
}

// Initial returns a channel that is closed after the initial sync completes.
func (dw *BackendDiscoveryWatcher) Initial() <-chan struct{} {
	return dw.initialCh
}

// InitialState returns a copy of the initial endpoints discovered during the
// first reconciliation. Safe to call after Initial() is closed.
func (dw *BackendDiscoveryWatcher) InitialState() map[string][]Endpoint {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	return maps.Clone(dw.initialState)
}

// InitialLabels returns a copy of the Service labels collected during the
// first reconciliation, keyed by service name. Safe to call after Initial()
// is closed.
func (dw *BackendDiscoveryWatcher) InitialLabels() map[string]map[string]string {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	out := make(map[string]map[string]string, len(dw.initialLabels))
	for k, v := range dw.initialLabels {
		out[k] = maps.Clone(v)
	}

	return out
}

// InitialAnnotations returns a copy of the Service annotations collected during
// the first reconciliation, keyed by service name. Safe to call after Initial()
// is closed.
func (dw *BackendDiscoveryWatcher) InitialAnnotations() map[string]map[string]string {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	out := make(map[string]map[string]string, len(dw.initialAnnotations))
	for k, v := range dw.initialAnnotations {
		out[k] = maps.Clone(v)
	}

	return out
}

// Changes returns the channel on which backend updates are delivered.
func (dw *BackendDiscoveryWatcher) Changes() <-chan BackendUpdate {
	return dw.updateCh
}

// Run starts the Service informer and blocks until ctx is cancelled.
func (dw *BackendDiscoveryWatcher) Run(ctx context.Context) error {
	opts := []informers.SharedInformerOption{
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = dw.selector.String()
		}),
	}
	if !dw.allNamespaces {
		opts = append(opts, informers.WithNamespace(dw.namespace))
	}

	factory := informers.NewSharedInformerFactoryWithOptions(dw.clientset, 0, opts...)
	informer := factory.Core().V1().Services().Informer()
	lister := factory.Core().V1().Services().Lister()

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { dw.syncServices(ctx, lister) },
		UpdateFunc: func(_, _ any) { dw.syncServices(ctx, lister) },
		DeleteFunc: func(_ any) { dw.syncServices(ctx, lister) },
	}
	_, err := informer.AddEventHandler(handler)
	if err != nil {
		return fmt.Errorf("adding event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Initial reconciliation — backends are created without forwarding
	// goroutines so we can consume the first value from each Changes()
	// channel without a race.
	dw.syncServices(ctx, lister)

	// Collect initial endpoints from all discovered backends.
	dw.mu.Lock()
	pendingInit := maps.Clone(dw.backends)
	dw.mu.Unlock()

	for _, mb := range pendingInit {
		select {
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // context cancellation
		case eps := <-mb.watcher.Changes():
			dw.mu.Lock()
			dw.initialState[mb.name] = eps
			dw.initialLabels[mb.name] = mb.watcher.Labels()
			dw.initialAnnotations[mb.name] = mb.watcher.Annotations()
			dw.mu.Unlock()
		}
	}

	// Now start forwarding goroutines for all initial backends.
	dw.mu.Lock()
	for _, mb := range dw.backends {
		dw.startForwardingLocked(ctx, mb)
	}
	dw.initialized = true
	dw.mu.Unlock()

	close(dw.initialCh)

	ns := dw.namespace
	if dw.allNamespaces {
		ns = "*"
	}
	dw.log.Info("watching Services for backend discovery", "namespace", ns, "selector", dw.selector.String())

	<-ctx.Done()

	// Stop all child watchers.
	dw.mu.Lock()
	for _, mb := range dw.backends {
		if mb.fwdCancel != nil {
			mb.fwdCancel()
		}
		mb.cancel()
	}
	dw.backends = nil
	dw.mu.Unlock()

	return nil
}

// startForwardingLocked starts a goroutine that forwards changes from the
// BackendWatcher to the discovery watcher's updateCh. Must be called with
// dw.mu held.
func (dw *BackendDiscoveryWatcher) startForwardingLocked(ctx context.Context, mb *managedBackend) {
	fwdCtx, fwdCancel := context.WithCancel(ctx)
	mb.fwdCancel = fwdCancel
	svcName := mb.name
	bw := mb.watcher
	go func() {
		for {
			select {
			case <-fwdCtx.Done():
				return
			case eps, ok := <-bw.Changes():
				if !ok {
					return
				}
				select {
				case dw.updateCh <- BackendUpdate{Name: svcName, Endpoints: eps, Labels: bw.Labels(), Annotations: bw.Annotations()}:
				case <-fwdCtx.Done():
					return
				}
			}
		}
	}()
}

// syncServices reconciles the set of active backends against the current list
// of matching Services.
func (dw *BackendDiscoveryWatcher) syncServices(ctx context.Context, lister corelisters.ServiceLister) {
	var services []*corev1.Service

	if dw.allNamespaces {
		all, listErr := lister.List(dw.selector)
		if listErr != nil {
			dw.log.Error("failed to list Services for discovery", "error", listErr)

			return
		}
		services = all
	} else {
		ns, listErr := lister.Services(dw.namespace).List(dw.selector)
		if listErr != nil {
			dw.log.Error("failed to list Services for discovery", "namespace", dw.namespace, "error", listErr)

			return
		}
		services = ns
	}

	current := make(map[string]*corev1.Service, len(services))
	for _, svc := range services {
		key := svc.Namespace + "/" + svc.Name
		current[key] = svc
	}

	dw.mu.Lock()
	defer dw.mu.Unlock()

	// backends is set to nil during shutdown; bail out to avoid nil-map panic.
	if dw.backends == nil {
		return
	}

	// Add new backends.
	for key, svc := range current {
		if _, exists := dw.backends[key]; exists {
			continue
		}
		if dw.explicitNames[svc.Name] {
			dw.log.Debug("skipping discovered Service (matches explicit --backend name)",
				"namespace", svc.Namespace, "service", svc.Name)

			continue
		}

		bw := NewBackendWatcher(dw.clientset, svc.Namespace, svc.Name, dw.portOverride)
		bw.SetExcludeAnnotations(dw.excludeAnnotation)
		childCtx, childCancel := context.WithCancel(ctx)
		go func() {
			runErr := bw.Run(childCtx)
			if runErr != nil {
				dw.log.Error("discovered backend watcher error",
					"namespace", svc.Namespace, "service", svc.Name, "error", runErr)
			}
		}()

		mb := &managedBackend{
			watcher:   bw,
			cancel:    childCancel,
			namespace: svc.Namespace,
			name:      svc.Name,
		}

		// Only start forwarding after the initial sync phase. During
		// initial sync, Run() reads the first value directly.
		if dw.initialized {
			dw.startForwardingLocked(ctx, mb)
		}

		dw.backends[key] = mb
		dw.log.Info("discovered backend Service",
			"namespace", svc.Namespace, "service", svc.Name)
	}

	// Remove backends that no longer match.
	for key, mb := range dw.backends {
		if _, exists := current[key]; exists {
			continue
		}
		if mb.fwdCancel != nil {
			mb.fwdCancel()
		}
		mb.cancel()
		delete(dw.backends, key)
		dw.log.Info("removed discovered backend Service",
			"namespace", mb.namespace, "service", mb.name)

		// Send removal notification.
		select {
		case dw.updateCh <- BackendUpdate{Name: mb.name, Endpoints: nil}:
		default:
			// Best effort — channel full.
		}
	}
}
