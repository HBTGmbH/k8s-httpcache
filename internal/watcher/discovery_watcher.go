package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

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
	// Gen is the generation of the managedBackend incarnation that produced
	// this update. It increases monotonically each time a Service name is
	// (re)discovered, letting the consumer drop a late update emitted by a
	// cancelled forwarding goroutine after the incarnation's removal (which
	// would otherwise resurrect a removed backend).
	Gen uint64
}

// managedBackend tracks a child BackendWatcher spawned for a discovered Service.
type managedBackend struct {
	watcher *BackendWatcher
	//nolint:containedctx // the child context is retained so the initial
	// collection loop can observe this backend being cancelled (its Service
	// was removed mid-startup) and skip it instead of blocking forever.
	ctx       context.Context
	cancel    context.CancelFunc
	fwdCancel context.CancelFunc // stops the forwarding goroutine (nil before forwarding starts)
	namespace string
	name      string
	gen       uint64 // generation tag, assigned at creation from genCounter
}

// discoveryWatcherSeq assigns each BackendDiscoveryWatcher a process-unique id
// so a shared NameRegistry can tell two watchers apart even when they manage the
// same namespace/serviceName.
var discoveryWatcherSeq atomic.Uint64

// genSeq is a process-wide monotonic generation counter shared by every
// BackendDiscoveryWatcher. Generations MUST be globally unique and increasing
// across watchers, not just within one: the event-loop consumer keys its
// removedGen tombstone by bare backend name (shared across all watchers), so a
// name handed off between two watchers with overlapping selectors needs the new
// owner's gen to exceed the gen the previous owner removed it with. A per-watcher
// counter would restart at 1 for the new owner and be dropped as stale.
// Generations start at 1, leaving 0 as the "never removed" sentinel on the
// consumer side (explicit --backend watchers carry gen 0).
var genSeq atomic.Uint64

// NameRegistry coordinates bare backend-name ownership across all discovery
// watchers. The event-loop consumer keys backends by bare name, so a given name
// must be forwarded by exactly one Service. The first watcher to discover a name
// owns it; others skip that name (logging a warning) until the owner releases it
// (on removal). Two label selectors can only collide on a name at runtime, so
// this is enforced as a graceful first-come-first-served claim rather than a
// startup error.
type NameRegistry struct {
	mu          sync.Mutex
	owners      map[string]string // bare name -> owner token
	subscribers map[uint64]func() // invoked (outside mu) after every successful release
	subSeq      uint64            // next subscriber id
}

// NewNameRegistry creates an empty shared name registry.
func NewNameRegistry() *NameRegistry {
	return &NameRegistry{owners: make(map[string]string)}
}

// claim records owner as the holder of name. It returns true if the claim
// succeeded (name was free, or already held by this exact owner) and false if a
// different owner already holds it.
func (r *NameRegistry) claim(name, owner string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, held := r.owners[name]; held {
		return cur == owner
	}
	r.owners[name] = owner

	return true
}

// release relinquishes name iff it is currently held by owner. Releasing a name
// held by someone else (or not held at all) is a no-op. After a successful
// release, all subscribers are notified: a watcher suppressed on this name gets
// no informer event for the release (the departing Service does not match its
// selector, and there is no informer resync), so this notification is its only
// wakeup to re-attempt the claim.
func (r *NameRegistry) release(name, owner string) {
	r.mu.Lock()
	released := false
	if r.owners[name] == owner {
		delete(r.owners, name)
		released = true
	}
	var subs []func()
	if released && len(r.subscribers) > 0 {
		subs = make([]func(), 0, len(r.subscribers))
		for _, f := range r.subscribers {
			subs = append(subs, f)
		}
	}
	r.mu.Unlock()

	// Called outside mu: subscribers are cheap non-blocking kicks, but they
	// must never run under the registry lock (a kicked watcher's reconcile
	// claims/releases names itself).
	for _, f := range subs {
		f()
	}
}

// subscribe registers f to be invoked after every successful release and
// returns a function that removes the subscription. f must be non-blocking
// (subscribers typically do a coalescing channel send). Unsubscribing on
// watcher shutdown keeps a long-lived registry from accumulating dead
// closures that fire on every future release.
func (r *NameRegistry) subscribe(f func()) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subscribers == nil {
		r.subscribers = make(map[uint64]func())
	}
	id := r.subSeq
	r.subSeq++
	r.subscribers[id] = f

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.subscribers, id)
	}
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
	id                uint64        // process-unique watcher id, for NameRegistry owner tokens
	registry          *NameRegistry // shared cross-watcher name claim; nil = no cross-watcher coordination

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
		id:                 discoveryWatcherSeq.Add(1),
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

// SetNameRegistry installs a shared NameRegistry so this watcher coordinates
// bare backend-name ownership with the other discovery watchers using the same
// registry. Must be called before Run. When unset, the watcher only dedups names
// within itself (via nameClaimedLocked).
func (dw *BackendDiscoveryWatcher) SetNameRegistry(r *NameRegistry) {
	dw.registry = r
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

	// Reconcile when any watcher releases a bare name: this watcher may hold a
	// suppressed same-named Service that can now be adopted, and no informer
	// event fires here for the release (the departing Service does not match
	// this selector; resync is disabled). The cap-1 kick channel coalesces
	// bursts; syncServices is already safe to run from any goroutine (dw.mu).
	if dw.registry != nil {
		kick := make(chan struct{}, 1)
		unsubscribe := dw.registry.subscribe(func() {
			select {
			case kick <- struct{}{}:
			default:
			}
		})
		defer unsubscribe()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-kick:
					dw.syncServices(ctx, lister)
				}
			}
		}()
	}

	// Release child watchers and registry claims on every return path,
	// including the early return from collectInitialState below (a ctx cancel
	// during initial sync) - not just the normal post-<-ctx.Done() exit.
	defer dw.shutdown()

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Initial reconciliation - backends are created without forwarding
	// goroutines so we can consume the first value from each Changes()
	// channel without a race.
	dw.syncServices(ctx, lister)

	// Collect initial endpoints from all discovered backends.
	dw.mu.Lock()
	pendingInit := maps.Clone(dw.backends)
	dw.mu.Unlock()

	err = dw.collectInitialState(ctx, pendingInit)
	if err != nil {
		return err
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

	return nil
}

// shutdown stops all child watchers and releases their registry claims so
// another watcher sharing the registry can take over the names if it outlives
// us. It is registered with defer in Run, so it runs on every return path.
// Safe to call regardless of how far startup progressed: it ranges an empty
// backends map before any backend is discovered and is a no-op once backends
// has been cleared (the syncServices nil-guard covers any racing callback).
func (dw *BackendDiscoveryWatcher) shutdown() {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	for key, mb := range dw.backends {
		if mb.fwdCancel != nil {
			mb.fwdCancel()
		}
		mb.cancel()
		if dw.registry != nil {
			dw.registry.release(mb.name, dw.ownerToken(key))
		}
	}
	dw.backends = nil
}

// collectInitialState blocks until each backend in pending has delivered its
// first endpoint set, recording it in the initial* maps. It runs before the
// forwarding goroutines are started, so it is the sole consumer of each
// backend's Changes() channel during this phase.
func (dw *BackendDiscoveryWatcher) collectInitialState(ctx context.Context, pending map[string]*managedBackend) error {
	for _, mb := range pending {
		select {
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // context cancellation
		case <-mb.ctx.Done():
			// mb.ctx descends from ctx (see startup in syncServices), so a
			// parent cancellation fires this case too; when both are ready,
			// select picks at random. Disambiguate: a cancelled parent is a
			// shutdown. Propagate it rather than letting the random pick
			// swallow it as a per-backend skip.
			err := ctx.Err()
			if err != nil {
				return err //nolint:wrapcheck // context cancellation
			}
			// This backend's Service was removed during the initial sync
			// window: its child watcher is cancelled and will never deliver
			// its first endpoint set. Skip it rather than blocking forever
			// (which would leave Initial() unclosed and stall startup). The
			// removal is reconciled later via the normal update path.
			continue
		case st := <-mb.watcher.Changes():
			// The snapshot carries endpoints and metadata from one consistent
			// observation (see BackendState), so they can never be torn apart.
			dw.mu.Lock()
			dw.initialState[mb.name] = st.Endpoints
			dw.initialLabels[mb.name] = st.Labels
			dw.initialAnnotations[mb.name] = st.Annotations
			dw.mu.Unlock()
		}
	}

	return nil
}

// startForwardingLocked starts a goroutine that forwards changes from the
// BackendWatcher to the discovery watcher's updateCh. Must be called with
// dw.mu held.
func (dw *BackendDiscoveryWatcher) startForwardingLocked(ctx context.Context, mb *managedBackend) {
	fwdCtx, fwdCancel := context.WithCancel(ctx)
	mb.fwdCancel = fwdCancel
	svcName := mb.name
	gen := mb.gen
	bw := mb.watcher
	go func() {
		for {
			select {
			case <-fwdCtx.Done():
				return
			case st, ok := <-bw.Changes():
				if !ok {
					return
				}
				eps := st.Endpoints
				if eps == nil {
					// The child watcher emits nil when the Service has no
					// ready endpoints. Nil Endpoints on a BackendUpdate
					// means "Service removed", so normalize to an empty
					// slice to keep the two cases distinguishable.
					eps = []Endpoint{}
				}
				// st carries endpoints and metadata from one consistent
				// observation (see BackendState).
				select {
				case dw.updateCh <- BackendUpdate{Name: svcName, Endpoints: eps, Labels: st.Labels, Annotations: st.Annotations, Gen: gen}:
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
	// Hold dw.mu across the lister read AND the reconcile so the two form one
	// atomic critical section, mirroring every sibling watcher's sync: this
	// runs concurrently from the informer handler, the NameRegistry kick
	// goroutine, and Run's explicit startup sync. Without the lock around the
	// read, a reconcile that completed its List but not yet taken the lock
	// could apply a STALE service list after a fresher reconcile - re-adding a
	// deleted Service's backend with a new generation (which defeats the
	// consumer's removedGen tombstone) or removing a just-added live one. The
	// lister read is an in-memory lookup, so the critical section stays cheap.
	dw.mu.Lock()

	// backends is set to nil during shutdown; bail out to avoid nil-map panic.
	if dw.backends == nil {
		dw.mu.Unlock()

		return
	}

	var services []*corev1.Service

	if dw.allNamespaces {
		all, listErr := lister.List(dw.selector)
		if listErr != nil {
			dw.mu.Unlock()
			dw.log.Error("failed to list Services for discovery", "error", listErr)

			return
		}
		services = all
	} else {
		ns, listErr := lister.Services(dw.namespace).List(dw.selector)
		if listErr != nil {
			dw.mu.Unlock()
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

	// Reconcile the listed Services in a deterministic order. Two Services with
	// the same bare name in different namespaces can both match this selector,
	// and only the first one visited claims the name (nameClaimedLocked and the
	// registry claim suppress the rest). Ranging `current` directly handed that
	// choice to Go's per-range randomized map iteration, so replicas of the same
	// controller - given byte-identical cluster state and flags - bound one VCL
	// backend name to Services in DIFFERENT namespaces, and every restart
	// re-rolled the choice, with nothing in the VCL, metrics or status surfacing
	// the divergence. Sorting the keys makes the winner a pure function of the
	// cluster state (lowest "namespace/serviceName" wins), the same reasoning
	// that fixed the endpoint dedup survivor in canonicalizeEndpoints.
	keys := slices.Sorted(maps.Keys(current))

	// Services the add pass skipped, recorded rather than logged on the spot:
	// the second add pass below can adopt the very Service the first one
	// skipped (a removal in this same reconcile frees the bare name), and
	// warning eagerly announced "skipping discovered Service" for a Service
	// adopted a few statements later - a line byte-identical to the genuine
	// permanently-suppressed one, so every healthy same-name handoff looked
	// like a misconfiguration. Each pass resets the list, so what survives the
	// final pass is exactly the set still suppressed when the reconcile ends.
	type suppressedService struct {
		namespace string
		name      string
		// byRegistry distinguishes a cross-watcher claim held by another
		// --backend-selector from a same-named Service managed by this watcher.
		byRegistry bool
	}
	var suppressed []suppressedService

	// addPass adopts every currently-listed Service that is not already managed
	// and whose bare name is free. It is run again after the remove pass below:
	// a removal can free a bare name (or registry claim) that a still-listed
	// same-named Service in another namespace was suppressed on during this
	// pass (which runs before the removal, while the departing winner still
	// holds the name). With no informer resync (period 0) the deletion is the
	// only event that fires, so the survivor must be adopted within this same
	// reconcile or the backend is orphaned until some unrelated future event.
	addPass := func() {
		suppressed = nil
		for _, key := range keys {
			svc := current[key]
			if _, exists := dw.backends[key]; exists {
				continue
			}
			if dw.explicitNames[svc.Name] {
				dw.log.Debug("skipping discovered Service (matches explicit --backend name)",
					"namespace", svc.Namespace, "service", svc.Name)

				continue
			}
			if dw.nameClaimedLocked(svc.Name, svc.Namespace) {
				suppressed = append(suppressed, suppressedService{namespace: svc.Namespace, name: svc.Name})

				continue
			}
			// Cross-watcher claim: another --backend-selector may already manage a
			// Service with this bare name. The consumer keys backends by bare name,
			// so only the first claimant forwards updates for it.
			if dw.registry != nil && !dw.registry.claim(svc.Name, dw.ownerToken(key)) {
				suppressed = append(suppressed, suppressedService{namespace: svc.Namespace, name: svc.Name, byRegistry: true})

				continue
			}

			bw := NewBackendWatcher(dw.clientset, svc.Namespace, svc.Name, dw.portOverride)
			bw.SetExcludeAnnotations(dw.excludeAnnotation)
			childCtx, childCancel := context.WithCancel(ctx)

			mb := &managedBackend{
				watcher:   bw,
				ctx:       childCtx,
				cancel:    childCancel,
				namespace: svc.Namespace,
				name:      svc.Name,
				gen:       nextGen(),
			}

			go func() {
				runErr := bw.Run(childCtx)
				if runErr != nil {
					dw.log.Error("discovered backend watcher error",
						"namespace", svc.Namespace, "service", svc.Name, "error", runErr)
				}
			}()

			// Only start forwarding after the initial sync phase. During
			// initial sync, Run() reads the first value directly.
			if dw.initialized {
				dw.startForwardingLocked(ctx, mb)
			}

			dw.backends[key] = mb
			dw.log.Info("discovered backend Service",
				"namespace", svc.Namespace, "service", svc.Name)
		}
	}
	addPass()

	// Remove backends that no longer match. Record each removed incarnation's
	// gen alongside its name: the removal is stamped with that gen so the
	// consumer can tombstone exactly this incarnation and drop any late update
	// a cancelled forwarding goroutine may still emit for it.
	type removal struct {
		name string
		gen  uint64
	}
	var removed []removal
	for key, mb := range dw.backends {
		if _, exists := current[key]; exists {
			continue
		}
		if mb.fwdCancel != nil {
			mb.fwdCancel()
		}
		mb.cancel()
		delete(dw.backends, key)
		if dw.registry != nil {
			dw.registry.release(mb.name, dw.ownerToken(key))
		}
		dw.log.Info("removed discovered backend Service",
			"namespace", mb.namespace, "service", mb.name)
		removed = append(removed, removal{name: mb.name, gen: mb.gen})
	}

	// A removal may have freed a bare name (or registry claim) that a
	// still-listed same-named Service was suppressed on during the add pass
	// above. Re-run the add pass now that the departing owner is gone so the
	// survivor is adopted in this same reconcile rather than orphaned.
	if len(removed) > 0 {
		addPass()
	}

	// Report only the Services still suppressed now that the reconcile has
	// settled - never one the second add pass just adopted.
	for _, s := range suppressed {
		if s.byRegistry {
			dw.log.Warn("skipping discovered Service: backend name already claimed by another --backend-selector",
				"namespace", s.namespace, "service", s.name)

			continue
		}
		dw.log.Warn("skipping discovered Service: name already claimed by a same-named Service in another namespace",
			"namespace", s.namespace, "service", s.name)
	}
	dw.mu.Unlock()

	// Send removal notifications without holding the lock. A removed Service
	// produces no further events that could correct a dropped notification,
	// so block until the consumer drains the channel instead of dropping on
	// overflow.
	for _, r := range removed {
		select {
		case dw.updateCh <- BackendUpdate{Name: r.name, Endpoints: nil, Gen: r.gen}:
		case <-ctx.Done():
			return
		}
	}
}

// nameClaimedLocked reports whether a backend with the given Service name
// already exists in a different namespace. Updates are keyed by bare Service
// name, so a second same-named Service would fight over the same backend group
// downstream; the incumbent keeps the name, and among Services first listed
// together the lowest "namespace/serviceName" wins (syncServices reconciles in
// sorted key order, so the choice cannot vary between restarts or replicas).
// Must be called with dw.mu held.
//
// Because syncServices runs its add pass before its remove pass, a departing
// winner still suppresses a same-named survivor during that first pass - the
// watcher never manages two same-named Services at once. The promotion is
// nonetheless EAGER, not lazy: syncServices re-runs the add pass after any
// removal (see the addPass comment), so the survivor is adopted in the same
// reconcile that drops the winner. It has to be - the informer resync period is
// 0, so the winner's deletion is the only event that ever fires and a deferred
// promotion would orphan the backend forever
// (TestDiscoveryWatcher_SurvivingSameNameAdoptedAfterWinnerDeleted).
func (dw *BackendDiscoveryWatcher) nameClaimedLocked(name, namespace string) bool {
	for _, mb := range dw.backends {
		if mb.name == name && mb.namespace != namespace {
			return true
		}
	}

	return false
}

// nextGen returns the next process-wide monotonically increasing generation tag
// from the shared genSeq counter. Each (re)discovery of a Service - by any
// watcher - gets a strictly higher gen than every previous incarnation, so the
// consumer can distinguish a genuine re-add from a stale update emitted by a
// cancelled forwarding goroutine, including when a name is handed off between
// watchers with overlapping selectors. Generations start at 1, leaving 0 as a
// "never removed" sentinel on the consumer side.
func nextGen() uint64 {
	return genSeq.Add(1)
}

// ownerToken returns this watcher's registry owner token for a backend keyed by
// "namespace/serviceName". The watcher id makes the token unique even when two
// watchers manage the same namespace/serviceName.
func (dw *BackendDiscoveryWatcher) ownerToken(key string) string {
	return fmt.Sprintf("%d/%s", dw.id, key)
}
