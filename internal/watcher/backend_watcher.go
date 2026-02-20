package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// BackendWatcher watches a Service object and emits endpoints. For ExternalName
// services it emits the hostname directly. For all other types it delegates to
// an internal EndpointSlice Watcher.
type BackendWatcher struct {
	clientset    kubernetes.Interface
	namespace    string
	serviceName  string
	portOverride string
	ch           chan []Endpoint

	mu       sync.Mutex // protects fields below
	previous []Endpoint
	synced   bool

	// EndpointSlice child watcher state (protected by mu).
	childWatcher *Watcher
	childCancel  context.CancelFunc
}

// NewBackendWatcher creates a new BackendWatcher.
func NewBackendWatcher(clientset kubernetes.Interface, namespace, serviceName, portOverride string) *BackendWatcher {
	return &BackendWatcher{
		clientset:    clientset,
		namespace:    namespace,
		serviceName:  serviceName,
		portOverride: portOverride,
		ch:           make(chan []Endpoint, 1),
	}
}

// Changes returns the channel on which endpoint updates are delivered.
func (bw *BackendWatcher) Changes() <-chan []Endpoint {
	return bw.ch
}

// Run starts watching the Service object and blocks until ctx is cancelled.
func (bw *BackendWatcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		bw.clientset,
		0,
		informers.WithNamespace(bw.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", bw.serviceName).String()
		}),
	)

	informer := factory.Core().V1().Services().Informer()
	lister := factory.Core().V1().Services().Lister()

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ interface{}) {
			bw.syncService(ctx, lister)
		},
		UpdateFunc: func(_, _ interface{}) {
			bw.syncService(ctx, lister)
		},
		DeleteFunc: func(_ interface{}) {
			bw.syncService(ctx, lister)
		},
	}

	if _, err := informer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("adding event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Deliver initial state even if no Service exists.
	bw.syncService(ctx, lister)

	slog.Info("watching Service for backend", "namespace", bw.namespace, "service", bw.serviceName)
	<-ctx.Done()

	bw.mu.Lock()
	bw.stopEndpointSliceWatcherLocked()
	bw.mu.Unlock()

	return nil
}

func (bw *BackendWatcher) syncService(ctx context.Context, lister corelisters.ServiceLister) {
	svc, err := lister.Services(bw.namespace).Get(bw.serviceName)
	if err != nil {
		// Service not found or error — emit empty endpoints.
		slog.Debug("service not found, emitting empty endpoints", "namespace", bw.namespace, "service", bw.serviceName, "error", err)
		bw.mu.Lock()
		bw.stopEndpointSliceWatcherLocked()
		bw.mu.Unlock()
		bw.send(nil)
		return
	}

	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		bw.mu.Lock()
		bw.stopEndpointSliceWatcherLocked()
		bw.mu.Unlock()

		if svc.Spec.ExternalName == "" {
			slog.Warn("ExternalName service has empty externalName, emitting empty endpoints",
				"namespace", bw.namespace, "service", bw.serviceName)
			bw.send(nil)
			return
		}

		port := bw.resolveExternalPort()
		endpoints := []Endpoint{{
			IP:   svc.Spec.ExternalName,
			Port: port,
			Name: "external",
		}}
		bw.send(endpoints)
		return
	}

	// Non-ExternalName service: delegate to EndpointSlice watcher.
	bw.mu.Lock()
	if bw.childWatcher == nil {
		bw.startEndpointSliceWatcherLocked(ctx)
	}
	bw.mu.Unlock()
}

func (bw *BackendWatcher) resolveExternalPort() int32 {
	if bw.portOverride == "" {
		slog.Warn("no port specified for ExternalName service, defaulting to 80",
			"namespace", bw.namespace, "service", bw.serviceName)
		return 80
	}
	if p, err := strconv.ParseInt(bw.portOverride, 10, 32); err == nil {
		return int32(p)
	}
	slog.Warn("named port not supported for ExternalName service, using port 0",
		"namespace", bw.namespace, "service", bw.serviceName, "port", bw.portOverride)
	return 0
}

func (bw *BackendWatcher) startEndpointSliceWatcherLocked(ctx context.Context) {
	childCtx, cancel := context.WithCancel(ctx)
	child := New(bw.clientset, bw.namespace, bw.serviceName, bw.portOverride)
	bw.childWatcher = child
	bw.childCancel = cancel

	go func() {
		if err := child.Run(childCtx); err != nil {
			slog.Error("EndpointSlice watcher error", "namespace", bw.namespace,
				"service", bw.serviceName, "error", err)
		}
	}()

	// Forward child watcher changes to our channel.
	go func() {
		for {
			select {
			case <-childCtx.Done():
				return
			case eps, ok := <-child.Changes():
				if !ok {
					return
				}
				bw.send(eps)
			}
		}
	}()
}

func (bw *BackendWatcher) stopEndpointSliceWatcherLocked() {
	if bw.childCancel != nil {
		bw.childCancel()
		bw.childCancel = nil
	}
	bw.childWatcher = nil
}

func (bw *BackendWatcher) send(endpoints []Endpoint) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.synced && endpointsEqual(endpoints, bw.previous) {
		return
	}

	if bw.synced {
		added, removed := diffEndpoints(bw.previous, endpoints)
		for _, ep := range added {
			slog.Debug("backend endpoint added", "namespace", bw.namespace, "service", bw.serviceName,
				"name", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
		}
		for _, ep := range removed {
			slog.Debug("backend endpoint removed", "namespace", bw.namespace, "service", bw.serviceName,
				"name", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
		}
	}

	bw.synced = true
	bw.previous = endpoints

	// Non-blocking send: drain then send.
	select {
	case <-bw.ch:
	default:
	}
	bw.ch <- endpoints
}
