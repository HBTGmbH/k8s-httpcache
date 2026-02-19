package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
)

// Endpoint represents a single pod endpoint (IP + port + pod name).
type Endpoint struct {
	IP   string
	Port int32
	Name string
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
	ch           chan []Endpoint
	mu           sync.Mutex // protects previous and synced
	previous     []Endpoint
	synced       bool // true after the first send, so the initial state is always delivered
}

// New creates a new EndpointSlice watcher. portOverride selects which port to
// use: a numeric string uses that port number directly, a non-numeric string
// matches the port by name in the EndpointSlice, and "" uses the first port.
func New(clientset kubernetes.Interface, namespace, serviceName string, portOverride string) *Watcher {
	return &Watcher{
		clientset:    clientset,
		namespace:    namespace,
		serviceName:  serviceName,
		portOverride: portOverride,
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
		AddFunc: func(_ interface{}) {
			w.sync(lister)
		},
		UpdateFunc: func(_, _ interface{}) {
			w.sync(lister)
		},
		DeleteFunc: func(_ interface{}) {
			w.sync(lister)
		},
	}

	if _, err := informer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("adding event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Ensure the initial state is always delivered, even when there are no
	// EndpointSlices (so no Add events fired during cache sync).
	w.sync(lister)

	slog.Info("watching EndpointSlices", "namespace", w.namespace, "service", w.serviceName)
	<-ctx.Done()
	return nil
}

func (w *Watcher) sync(lister discoverylisters.EndpointSliceLister) {
	slices, err := lister.EndpointSlices(w.namespace).List(labels.Everything())
	if err != nil {
		slog.Error("failed to list EndpointSlices", "error", err)
		return
	}

	var endpoints []Endpoint
	for _, slice := range slices {
		if slice.AddressType != discoveryv1.AddressTypeIPv4 &&
			slice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}

		port := resolvePort(slice.Ports, w.portOverride)

		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready == nil || !*ep.Conditions.Ready {
				continue
			}
			name := ""
			if ep.TargetRef != nil {
				name = ep.TargetRef.Name
			}
			for _, addr := range ep.Addresses {
				endpoints = append(endpoints, Endpoint{
					IP:   addr,
					Port: port,
					Name: name,
				})
			}
		}
	}

	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].IP != endpoints[j].IP {
			return endpoints[i].IP < endpoints[j].IP
		}
		return endpoints[i].Port < endpoints[j].Port
	})

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.synced && endpointsEqual(endpoints, w.previous) {
		return
	}

	if w.synced {
		added, removed := diffEndpoints(w.previous, endpoints)
		for _, ep := range added {
			slog.Debug("endpoint added", "namespace", w.namespace, "service", w.serviceName,
				"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
		}
		for _, ep := range removed {
			slog.Debug("endpoint removed", "namespace", w.namespace, "service", w.serviceName,
				"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
		}
	}

	w.synced = true
	w.previous = endpoints

	// Non-blocking send: drain then send.
	select {
	case <-w.ch:
	default:
	}
	w.ch <- endpoints
}

// resolvePort picks the port number from the EndpointSlice ports list.
// If override is a numeric string, it is used directly. If it is a non-empty
// non-numeric string, the port with that name is looked up. If override is
// empty, the first port in the list is used.
func resolvePort(ports []discoveryv1.EndpointPort, override string) int32 {
	if override != "" {
		if p, err := strconv.ParseInt(override, 10, 32); err == nil {
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

func diffEndpoints(old, new []Endpoint) (added, removed []Endpoint) {
	oldSet := make(map[Endpoint]struct{}, len(old))
	for _, e := range old {
		oldSet[e] = struct{}{}
	}
	newSet := make(map[Endpoint]struct{}, len(new))
	for _, e := range new {
		newSet[e] = struct{}{}
	}
	for _, e := range new {
		if _, ok := oldSet[e]; !ok {
			added = append(added, e)
		}
	}
	for _, e := range old {
		if _, ok := newSet[e]; !ok {
			removed = append(removed, e)
		}
	}
	return
}

func endpointsEqual(a, b []Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
