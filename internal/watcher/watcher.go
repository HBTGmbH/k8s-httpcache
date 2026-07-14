// Package watcher watches Kubernetes EndpointSlices and Services for changes.
package watcher

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
)

// Endpoint represents a single pod endpoint (host + port + pod name) with
// optional topology metadata.
type Endpoint struct {
	Host     string
	Port     int32
	Name     string
	Zone     string   // topology.kubernetes.io/zone from EndpointSlice
	NodeName string   // node hosting this endpoint
	ForZones []string // zone names from endpoint.hints.forZones (Topology Aware Routing)
}

// Frontend is an alias for Endpoint, used for Varnish shard peers.
type Frontend = Endpoint

// Watcher watches EndpointSlices for a given Service and sends updated
// endpoint lists on a channel whenever the set of ready endpoints changes.
type Watcher struct {
	clientset    kubernetes.Interface
	namespace    string
	serviceName  string
	portOverride string // numeric string, port name, or "" = first slice port
	log          *slog.Logger
	ch           chan []Endpoint
	mu           sync.Mutex // protects previous, synced, fqdnWarned and portWarned
	previous     []Endpoint
	synced       bool // true after the first send, so the initial state is always delivered
	fqdnWarned   bool // the FQDN-only warning has been logged (once per watcher)
	portWarned   bool // the unmatched-port-override warning has been logged (once per watcher)
}

// New creates a new EndpointSlice watcher. portOverride selects which port to
// use: a numeric string uses that port number directly, a non-numeric string
// matches the port by name in the EndpointSlice, and "" uses the first port.
func New(clientset kubernetes.Interface, namespace, serviceName, portOverride string) *Watcher {
	return &Watcher{
		clientset:    clientset,
		namespace:    namespace,
		serviceName:  serviceName,
		portOverride: portOverride,
		log:          slog.Default(),
		ch:           make(chan []Endpoint, 1),
	}
}

// Changes returns the channel on which endpoint updates are delivered.
func (w *Watcher) Changes() <-chan []Endpoint {
	return w.ch
}

// Run starts the informer and blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset,
		0,
		informers.WithNamespace(w.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labels.Set{
				discoveryv1.LabelServiceName: w.serviceName,
			}.String()
		}),
	)

	informer := factory.Discovery().V1().EndpointSlices().Informer()
	lister := factory.Discovery().V1().EndpointSlices().Lister()

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ any) {
			w.sync(lister)
		},
		UpdateFunc: func(_, _ any) {
			w.sync(lister)
		},
		DeleteFunc: func(_ any) {
			w.sync(lister)
		},
	}

	_, err := informer.AddEventHandler(handler)
	if err != nil {
		return fmt.Errorf("adding event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Ensure the initial state is always delivered, even when there are no
	// EndpointSlices (so no Add events fired during cache sync).
	w.sync(lister)

	w.log.Info("watching EndpointSlices", "namespace", w.namespace, "service", w.serviceName)
	<-ctx.Done()

	return nil
}

func (w *Watcher) sync(lister discoverylisters.EndpointSliceLister) {
	// Hold the mutex across the lister read and the send so the two are atomic:
	// the explicit sync in Run can run concurrently with an informer-handler
	// sync, and without this the goroutine that read the older store state could
	// send last, leaving a stale value buffered on the cap-1 channel. Serialising
	// read+send guarantees the last sender is the last reader, so the delivered
	// value converges to the latest store state. The read is an in-memory lister
	// lookup, so the critical section stays cheap.
	w.mu.Lock()
	defer w.mu.Unlock()

	epSlices, err := lister.EndpointSlices(w.namespace).List(labels.Everything())
	if err != nil {
		w.log.Error("failed to list EndpointSlices", "error", err)

		return
	}

	// Dual-stack Services publish one EndpointSlice per address family, so the
	// same pod would otherwise be listed once per family - with an identical
	// endpoint Name but different addresses. Duplicate names break templates
	// that key backends on .Name (duplicate VCL backend declarations fail to
	// compile), and the broadcast fan-out would hit every pod twice. Serve a
	// single family: IPv4 when any IPv4 slice exists, IPv6 otherwise.
	//
	// At informer startup the two family slices can be observed out of order
	// (an IPv6-only first sync, then the IPv4 slice's Add flips the family).
	// That one-time transition is absorbed by the frontend/backend debounce;
	// in steady state a dual-stack Service's slices appear and disappear
	// together, so the family never flaps.
	family := discoveryv1.AddressTypeIPv6
	sawIP := false
	sawFQDN := false
	for _, slice := range epSlices {
		switch slice.AddressType {
		case discoveryv1.AddressTypeIPv4:
			family = discoveryv1.AddressTypeIPv4
			sawIP = true
		case discoveryv1.AddressTypeIPv6:
			sawIP = true
		case discoveryv1.AddressTypeFQDN:
			sawFQDN = true
		}
	}
	// FQDN slices are deliberately not served (pinned intent: only IP
	// families are supported), but dropping them SILENTLY would leave an
	// FQDN-only Service inexplicably empty - warn once so the operator can
	// tell misconfiguration from an empty Service.
	if sawFQDN && !sawIP && !w.fqdnWarned {
		w.fqdnWarned = true
		w.log.Warn("Service publishes only FQDN-type EndpointSlices, which are not supported; emitting no endpoints",
			"namespace", w.namespace, "service", w.serviceName)
	}

	var endpoints, servingTerminating []Endpoint
	for _, slice := range epSlices {
		if slice.AddressType != family {
			continue
		}

		port := resolvePort(slice.Ports, w.portOverride)
		// A named override that matches nothing (e.g. the port was renamed on
		// the Service at runtime; startup validation only catches impossible
		// names) resolves every endpoint to port 0 - warn once instead of
		// silently rendering backends on port 0.
		if port == 0 && w.portOverride != "" && !w.portWarned {
			w.portWarned = true
			w.log.Warn("port override matches no port in the EndpointSlice; endpoints resolve to port 0",
				"namespace", w.namespace, "service", w.serviceName, "override", w.portOverride)
		}

		for _, ep := range slice.Endpoints {
			// Ready == nil means "unknown"; the EndpointSlice spec
			// instructs consumers to interpret unknown as ready.
			ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
			// Serving-and-terminating endpoints are kept in a separate set:
			// when NO endpoint is ready (a rolling restart's final phase),
			// traffic falls back to them - matching kube-proxy's
			// ProxyTerminatingEndpoints behavior - instead of rendering an
			// empty backend list.
			fallback := !ready &&
				ep.Conditions.Serving != nil && *ep.Conditions.Serving &&
				ep.Conditions.Terminating != nil && *ep.Conditions.Terminating
			if !ready && !fallback {
				continue
			}
			name := ""
			if ep.TargetRef != nil {
				name = ep.TargetRef.Name
			}
			zone := ""
			if ep.Zone != nil {
				zone = *ep.Zone
			}
			nodeName := ""
			if ep.NodeName != nil {
				nodeName = *ep.NodeName
			}
			var forZones []string
			if ep.Hints != nil {
				for _, h := range ep.Hints.ForZones {
					forZones = append(forZones, h.Name)
				}
			}
			// Only the first address is used: discovery/v1 defines no
			// semantics for additional addresses (kube-proxy ignores them),
			// and emitting one Endpoint per address would duplicate .Name -
			// the exact duplicate-backend condition the dual-stack handling
			// above documents as breaking vcl.load.
			if len(ep.Addresses) == 0 {
				continue
			}
			e := Endpoint{
				Host:     ep.Addresses[0],
				Port:     port,
				Name:     name,
				Zone:     zone,
				NodeName: nodeName,
				ForZones: forZones,
			}
			if ready {
				endpoints = append(endpoints, e)
			} else {
				servingTerminating = append(servingTerminating, e)
			}
		}
	}

	if len(endpoints) == 0 && len(servingTerminating) > 0 {
		w.log.Debug("no ready endpoints, falling back to serving-terminating endpoints",
			"namespace", w.namespace, "service", w.serviceName, "count", len(servingTerminating))
		endpoints = servingTerminating
	}

	endpoints = canonicalizeEndpoints(endpoints)

	if w.synced && EndpointsEqual(endpoints, w.previous) {
		return
	}

	if w.synced {
		added, removed := diffEndpoints(w.previous, endpoints)
		for _, ep := range added {
			w.log.Debug("endpoint added", "namespace", w.namespace, "service", w.serviceName,
				"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.Host, ep.Port), "zone", ep.Zone)
		}
		for _, ep := range removed {
			w.log.Debug("endpoint removed", "namespace", w.namespace, "service", w.serviceName,
				"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.Host, ep.Port), "zone", ep.Zone)
		}
	}

	w.synced = true
	w.previous = endpoints

	coalescingSend(w.ch, endpoints)
}

// canonicalizeEndpoints sorts endpoints into a total order and deduplicates
// same-(Host,Port) entries (the same endpoint can transiently appear in more
// than one slice during EndpointSlice rebalancing; the API docs require
// consumers to deduplicate). The sort covers EVERY field, not just (Host,
// Port): with a partial order the dedup survivor's metadata (Zone, Name,
// ForZones) would depend on lister iteration order, making EndpointsEqual
// flap across syncs of an unchanged store and causing spurious
// endpoint-change deliveries.
func canonicalizeEndpoints(endpoints []Endpoint) []Endpoint {
	slices.SortFunc(endpoints, func(a, b Endpoint) int {
		if c := cmp.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Port, b.Port); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Zone, b.Zone); c != 0 {
			return c
		}
		if c := cmp.Compare(a.NodeName, b.NodeName); c != 0 {
			return c
		}

		return slices.Compare(a.ForZones, b.ForZones)
	})

	return slices.CompactFunc(endpoints, func(a, b Endpoint) bool {
		return a.Host == b.Host && a.Port == b.Port
	})
}

// resolvePort picks the port number from the EndpointSlice ports list.
// If override is a numeric string, it is used directly. If it is a non-empty
// non-numeric string, the port with that name is looked up. If override is
// empty, the first port in the list is used.
func resolvePort(ports []discoveryv1.EndpointPort, override string) int32 {
	if override != "" {
		p, parseErr := strconv.ParseInt(override, 10, 32)
		if parseErr == nil {
			return int32(p)
		}
		// Match by port name.
		for _, p := range ports {
			if p.Name != nil && *p.Name == override && p.Port != nil {
				return *p.Port
			}
		}

		return 0
	}
	if len(ports) > 0 && ports[0].Port != nil {
		return *ports[0].Port
	}

	return 0
}

// endpointKey returns a comparable string key for use in maps, since Endpoint
// contains a []string field (ForZones) that makes the struct non-comparable.
func endpointKey(e *Endpoint) string {
	return fmt.Sprintf("%s|%d|%s|%s|%s|%s", e.Host, e.Port, e.Name, e.Zone, e.NodeName, strings.Join(e.ForZones, ","))
}

func diffEndpoints(old, cur []Endpoint) ([]Endpoint, []Endpoint) {
	oldSet := make(map[string]struct{}, len(old))
	for i := range old {
		oldSet[endpointKey(&old[i])] = struct{}{}
	}
	curSet := make(map[string]struct{}, len(cur))
	for i := range cur {
		curSet[endpointKey(&cur[i])] = struct{}{}
	}
	var added, removed []Endpoint
	for i := range cur {
		if _, ok := oldSet[endpointKey(&cur[i])]; !ok {
			added = append(added, cur[i])
		}
	}
	for i := range old {
		if _, ok := curSet[endpointKey(&old[i])]; !ok {
			removed = append(removed, old[i])
		}
	}

	return added, removed
}

// endpointEqual compares two Endpoints for equality, including the ForZones slice.
func endpointEqual(a, b *Endpoint) bool {
	return a.Host == b.Host && a.Port == b.Port && a.Name == b.Name &&
		a.Zone == b.Zone && a.NodeName == b.NodeName &&
		slices.Equal(a.ForZones, b.ForZones)
}

// EndpointsEqual reports whether two endpoint slices are identical.
func EndpointsEqual(a, b []Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	// The combined bound (i < len(a) && i < len(b)) is redundant given the
	// length check above, but it makes the b[i] index provably in-range for
	// static analysis (gosec G602). endpointEqual takes pointers to avoid
	// copying the (large) Endpoint struct on every comparison.
	for i := 0; i < len(a) && i < len(b); i++ {
		if !endpointEqual(&a[i], &b[i]) {
			return false
		}
	}

	return true
}

// coalescingSend drains any stale buffered value then enqueues v on a cap-1
// channel, so the consumer always observes the latest value without the
// producer ever blocking.
//
// INVARIANT: callers MUST hold the watcher mutex that serialises all sends on
// ch across both the drain and the send. The drain+send pair only guarantees
// buffer space (and thus a non-blocking send) when every sender is serialised;
// calling this without that mutex reintroduces the stale-value race documented
// on BackendWatcher.resend.
func coalescingSend[T any](ch chan T, v T) {
	select {
	case <-ch:
	default:
	}
	ch <- v
}
