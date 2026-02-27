// Package watcher watches Kubernetes EndpointSlices and Services for changes.
package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
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

// Endpoint represents a single pod endpoint (IP + port + pod name) with
// optional topology metadata.
type Endpoint struct {
	IP       string
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
	mu           sync.Mutex // protects previous and synced
	previous     []Endpoint
	synced       bool // true after the first send, so the initial state is always delivered
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
	epSlices, err := lister.EndpointSlices(w.namespace).List(labels.Everything())
	if err != nil {
		w.log.Error("failed to list EndpointSlices", "error", err)

		return
	}

	var endpoints []Endpoint
	for _, slice := range epSlices {
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
			for _, addr := range ep.Addresses {
				endpoints = append(endpoints, Endpoint{
					IP:       addr,
					Port:     port,
					Name:     name,
					Zone:     zone,
					NodeName: nodeName,
					ForZones: forZones,
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

	if w.synced && EndpointsEqual(endpoints, w.previous) {
		return
	}

	if w.synced {
		added, removed := diffEndpoints(w.previous, endpoints)
		for _, ep := range added {
			w.log.Debug("endpoint added", "namespace", w.namespace, "service", w.serviceName,
				"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port), "zone", ep.Zone)
		}
		for _, ep := range removed {
			w.log.Debug("endpoint removed", "namespace", w.namespace, "service", w.serviceName,
				"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port), "zone", ep.Zone)
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
	return fmt.Sprintf("%s|%d|%s|%s|%s|%s", e.IP, e.Port, e.Name, e.Zone, e.NodeName, strings.Join(e.ForZones, ","))
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
	return a.IP == b.IP && a.Port == b.Port && a.Name == b.Name &&
		a.Zone == b.Zone && a.NodeName == b.NodeName &&
		slices.Equal(a.ForZones, b.ForZones)
}

// EndpointsEqual reports whether two endpoint slices are identical.
func EndpointsEqual(a, b []Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !endpointEqual(&a[i], &b[i]) {
			return false
		}
	}

	return true
}
