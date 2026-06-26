package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

var errNamedPortExternalName = errors.New("named port not supported for ExternalName service")

// externalEndpointName is the synthetic endpoint name emitted for
// ExternalName services, which have no underlying EndpointSlice.
const externalEndpointName = "external"

// BackendWatcher watches a Service object and emits endpoints. For ExternalName
// services it emits the hostname directly. For all other types it delegates to
// an internal EndpointSlice Watcher.
type BackendWatcher struct {
	clientset    kubernetes.Interface
	namespace    string
	serviceName  string
	portOverride string
	log          *slog.Logger
	ch           chan []Endpoint

	excludeAnnotation func(string) bool // nil = no filtering

	mu              sync.Mutex // protects fields below
	previous        []Endpoint
	labels          map[string]string
	annotations     map[string]string
	synced          bool
	serviceNotFound bool

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
		log:          slog.Default(),
		ch:           make(chan []Endpoint, 1),
	}
}

// Changes returns the channel on which endpoint updates are delivered.
func (bw *BackendWatcher) Changes() <-chan []Endpoint {
	return bw.ch
}

// Labels returns a copy of the most recently observed Service labels.
// Returns an empty (non-nil) map when the Service has no labels or has not
// been observed yet, so callers never need to nil-check.
func (bw *BackendWatcher) Labels() map[string]string {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.labels == nil {
		return make(map[string]string)
	}

	return maps.Clone(bw.labels)
}

// Annotations returns a copy of the most recently observed Service annotations.
// Returns an empty (non-nil) map when the Service has no annotations or has not
// been observed yet, so callers never need to nil-check.
func (bw *BackendWatcher) Annotations() map[string]string {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.annotations == nil {
		return make(map[string]string)
	}

	return maps.Clone(bw.annotations)
}

// Metadata returns copies of the most recently observed Service labels and
// annotations, read together under a single lock so the two always come from
// the same observation. Calling Labels() and Annotations() separately can tear:
// a metadata update landing between the two calls would pair labels from one
// generation with annotations from the next. Both maps are empty (non-nil) when
// unset, so callers never need to nil-check.
func (bw *BackendWatcher) Metadata() (map[string]string, map[string]string) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	labels := make(map[string]string)
	if bw.labels != nil {
		labels = maps.Clone(bw.labels)
	}
	annotations := make(map[string]string)
	if bw.annotations != nil {
		annotations = maps.Clone(bw.annotations)
	}

	return labels, annotations
}

// SetExcludeAnnotations sets the annotation exclusion filter. Must be called
// before Run.
func (bw *BackendWatcher) SetExcludeAnnotations(exclude func(string) bool) {
	bw.excludeAnnotation = exclude
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
		AddFunc: func(_ any) {
			bw.syncService(ctx, lister)
		},
		UpdateFunc: func(_, _ any) {
			bw.syncService(ctx, lister)
		},
		DeleteFunc: func(_ any) {
			bw.syncService(ctx, lister)
		},
	}

	_, err := informer.AddEventHandler(handler)
	if err != nil {
		return fmt.Errorf("adding event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Deliver initial state even if no Service exists.
	bw.syncService(ctx, lister)

	bw.log.Info("watching Service for backend", "namespace", bw.namespace, "service", bw.serviceName)
	<-ctx.Done()

	bw.mu.Lock()
	bw.stopEndpointSliceWatcherLocked()
	bw.mu.Unlock()

	return nil
}

func (bw *BackendWatcher) syncService(ctx context.Context, lister corelisters.ServiceLister) {
	// Hold bw.mu across the lister read and every send so the read+send is one
	// atomic critical section: the explicit syncService in Run can run
	// concurrently with an informer-handler syncService, and serialising them
	// keeps a goroutine that read an older Service state from sending last and
	// leaving a stale value buffered on the cap-1 channel. (The child-delegation
	// path is additionally guarded by sendFromChild's identity check.) Every
	// operation here is in-memory and non-blocking, so the section stays cheap.
	// See Watcher.sync.
	bw.mu.Lock()
	defer bw.mu.Unlock()

	svc, err := lister.Services(bw.namespace).Get(bw.serviceName)
	if err != nil {
		// Service not found or error — emit empty endpoints.
		if !bw.serviceNotFound {
			bw.serviceNotFound = true
			bw.log.Warn("backend Service not found, emitting empty endpoints",
				"namespace", bw.namespace, "service", bw.serviceName, "error", err)
		}
		bw.labels = nil
		bw.annotations = nil
		bw.stopEndpointSliceWatcherLocked()
		bw.sendLocked(nil)

		return
	}

	// Service exists — reset the warning flag so we warn again if it disappears.
	bw.serviceNotFound = false

	// Track Service labels and annotations; trigger a resend when metadata
	// changed so downstream consumers (e.g. VCL templates using
	// BackendGroup.Labels / .Annotations) pick up new metadata even if
	// endpoints remain identical.
	newLabels := maps.Clone(svc.Labels)
	labelsChanged := !maps.Equal(bw.labels, newLabels)
	bw.labels = newLabels

	newAnnotations := filterAnnotations(maps.Clone(svc.Annotations), bw.excludeAnnotation)
	annotationsChanged := !maps.Equal(bw.annotations, newAnnotations)
	bw.annotations = newAnnotations

	if (labelsChanged || annotationsChanged) && bw.synced {
		bw.resendLocked()
	}

	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		bw.stopEndpointSliceWatcherLocked()

		if svc.Spec.ExternalName == "" {
			bw.log.Warn("ExternalName service has empty externalName, emitting empty endpoints",
				"namespace", bw.namespace, "service", bw.serviceName)
			bw.sendLocked(nil)

			return
		}

		port, err := bw.resolveExternalPort()
		if err != nil {
			bw.log.Error("cannot resolve port for ExternalName service, emitting empty endpoints",
				"namespace", bw.namespace, "service", bw.serviceName, "error", err)
			bw.sendLocked(nil)

			return
		}
		endpoints := []Endpoint{{
			Host: svc.Spec.ExternalName,
			Port: port,
			Name: externalEndpointName,
		}}
		bw.sendLocked(endpoints)

		return
	}

	// Non-ExternalName service: delegate to EndpointSlice watcher.
	if bw.childWatcher == nil {
		bw.startEndpointSliceWatcherLocked(ctx)
	}
}

// resolveExternalPort determines the port for an ExternalName backend from the
// configured override. With no override it defaults to 80 (logging a warning)
// for backward compatibility. A future opt-in flag (e.g. --external-name-require-port,
// plumbed as a requireExplicitPort bool on BackendWatcher/BackendDiscoveryWatcher)
// could instead return an error here when set, without changing this default.
func (bw *BackendWatcher) resolveExternalPort() (int32, error) {
	if bw.portOverride == "" {
		bw.log.Warn("no port specified for ExternalName service, defaulting to 80",
			"namespace", bw.namespace, "service", bw.serviceName)

		return 80, nil
	}
	p, parseErr := strconv.ParseInt(bw.portOverride, 10, 32)
	if parseErr == nil {
		return int32(p), nil
	}

	return 0, fmt.Errorf("port %q for ExternalName service %s/%s: %w",
		bw.portOverride, bw.namespace, bw.serviceName, errNamedPortExternalName)
}

func (bw *BackendWatcher) startEndpointSliceWatcherLocked(ctx context.Context) {
	childCtx, cancel := context.WithCancel(ctx)
	child := New(bw.clientset, bw.namespace, bw.serviceName, bw.portOverride)
	child.log = bw.log
	bw.childWatcher = child
	bw.childCancel = cancel

	go func() {
		err := child.Run(childCtx)
		if err != nil {
			bw.log.Error("EndpointSlice watcher error", "namespace", bw.namespace,
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
				bw.sendFromChild(child, eps)
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

// resend re-sends the last known endpoints to the channel, bypassing
// the endpoint-equality dedup in send(). This is used when Service
// metadata (labels or annotations) changes without an endpoint change.
func (bw *BackendWatcher) resend() {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	bw.resendLocked()
}

// resendLocked re-sends the last known endpoints, bypassing the dedup in
// sendLocked. Callers must hold bw.mu.
//
// The send must stay under the mutex so it is atomic with sendLocked() —
// otherwise a resend racing a newer update can drain the fresh value and
// deliver the stale one (coalescingSend's drain+send pair only guarantees
// buffer space when all senders are serialised).
func (bw *BackendWatcher) resendLocked() {
	coalescingSend(bw.ch, bw.previous)
}

// sendFromChild forwards an endpoint update originating from the given child
// EndpointSlice watcher, but only if that child is still the active one. When a
// Service transitions away from a backing EndpointSlice (e.g. to ExternalName,
// or it is deleted), stopEndpointSliceWatcherLocked cancels the child and the
// parent emits the new endpoints itself — but the cancelled child's forwarding
// goroutine can still hold one in-flight update. Without this guard that stale
// update could race the parent's send() and overwrite the fresh endpoints,
// leaving the backend pointed at gone pod IPs until the next Service event. The
// child check and the send happen together under bw.mu, so they are atomic with
// respect to stopEndpointSliceWatcherLocked.
func (bw *BackendWatcher) sendFromChild(child *Watcher, endpoints []Endpoint) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.childWatcher != child {
		return // this child has been superseded; drop its stale update
	}
	bw.sendLocked(endpoints)
}

func (bw *BackendWatcher) send(endpoints []Endpoint) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	bw.sendLocked(endpoints)
}

// sendLocked performs the endpoint dedup, logging and drain-then-send. Callers
// must hold bw.mu.
func (bw *BackendWatcher) sendLocked(endpoints []Endpoint) {
	if bw.synced && EndpointsEqual(endpoints, bw.previous) {
		return
	}

	if bw.synced {
		added, removed := diffEndpoints(bw.previous, endpoints)
		for _, ep := range added {
			bw.log.Debug("backend endpoint added", "namespace", bw.namespace, "service", bw.serviceName,
				"name", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.Host, ep.Port), "zone", ep.Zone)
		}
		for _, ep := range removed {
			bw.log.Debug("backend endpoint removed", "namespace", bw.namespace, "service", bw.serviceName,
				"name", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.Host, ep.Port), "zone", ep.Zone)
		}
	}

	if len(endpoints) == 0 {
		bw.log.Warn("backend has no ready endpoints", "namespace", bw.namespace, "service", bw.serviceName)
	}

	bw.synced = true
	bw.previous = endpoints

	coalescingSend(bw.ch, endpoints)
}
