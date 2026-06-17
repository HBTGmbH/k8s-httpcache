package watcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
)

func makeDiscoverableService(namespace, name string, lbls map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    lbls,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

func makeDiscoverableEndpointSlice(namespace, serviceName, sliceName, ip string, port int32) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sliceName,
			Namespace: namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: serviceName,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{ip},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: serviceName + "-pod-0"},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(port)},
		},
	}
}

func readDiscoveryUpdate(t *testing.T, dw *BackendDiscoveryWatcher) BackendUpdate {
	t.Helper()
	select {
	case u := <-dw.Changes():
		return u
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for discovery update")

		return BackendUpdate{}
	}
}

// readDiscoveryRemoval reads updates until a removal (nil Endpoints) for name
// arrives. The Service and EndpointSlice informers sync independently, so a
// redundant endpoints refresh (non-nil) can be delivered before the removal;
// such refreshes are skipped so the assertion stays race-free.
func readDiscoveryRemoval(t *testing.T, dw *BackendDiscoveryWatcher, name string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name != name {
				t.Fatalf("Name = %q, want %q", u.Name, name)
			}
			if u.Endpoints == nil {
				return // removal observed
			}
			// Redundant endpoints refresh before removal — keep waiting.
		case <-deadline:
			t.Fatalf("timeout waiting for removal of %q", name)
		}
	}
}

func TestDiscoveryWatcher_InitialDiscovery(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	state := dw.InitialState()
	eps, ok := state["web"]
	if !ok {
		t.Fatal("expected 'web' in initial state")
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Host != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", eps[0].Host)
	}
}

func TestDiscoveryWatcher_ServiceAdded(t *testing.T) {
	t.Parallel()
	// Start with no matching services.
	clientset := fake.NewClientset()

	sel, _ := labels.Parse("app=api")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	state := dw.InitialState()
	if len(state) != 0 {
		t.Fatalf("expected empty initial state, got %d entries", len(state))
	}

	// Now create a matching service + EndpointSlice.
	svc := makeDiscoverableService("default", "api", map[string]string{"app": "api"})
	slice := makeDiscoverableEndpointSlice("default", "api", "api-abc", "10.0.1.1", 9090)
	_, err := clientset.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Service: %v", err)
	}

	waitForWatch()

	_, err = clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	// The first update may have empty endpoints (before EndpointSlice is
	// created). Keep reading until we get endpoints or time out.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name != "api" {
				t.Errorf("Name = %q, want api", u.Name)
			}
			if len(u.Endpoints) > 0 {
				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for discovery update with endpoints")
		}
	}
}

func TestDiscoveryWatcher_ServiceRemoved(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	waitForWatch()

	// Delete the service.
	err := clientset.CoreV1().Services("default").Delete(ctx, "web", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Service: %v", err)
	}

	readDiscoveryRemoval(t, dw, "web")
}

func TestDiscoveryWatcher_ExplicitNameSkipped(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "origin", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "origin", "origin-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	explicit := map[string]bool{"origin": true}
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", explicit)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	state := dw.InitialState()
	if len(state) != 0 {
		t.Fatalf("expected empty state (explicit name should be skipped), got %v", state)
	}
}

func TestDiscoveryWatcher_AllNamespaces(t *testing.T) {
	t.Parallel()
	svc1 := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	slice1 := makeDiscoverableEndpointSlice("ns1", "web", "web-abc", "10.0.0.1", 8080)
	svc2 := makeDiscoverableService("ns2", "web2", map[string]string{"app": "web"})
	slice2 := makeDiscoverableEndpointSlice("ns2", "web2", "web2-abc", "10.0.0.2", 8080)
	clientset := fake.NewClientset(svc1, slice1, svc2, slice2)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "", true, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	state := dw.InitialState()
	if len(state) != 2 {
		t.Fatalf("expected 2 backends, got %d: %v", len(state), state)
	}
	if _, ok := state["web"]; !ok {
		t.Error("expected 'web' in state")
	}
	if _, ok := state["web2"]; !ok {
		t.Error("expected 'web2' in state")
	}
}

func TestDiscoveryWatcher_EmptyInitialState(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()

	sel, _ := labels.Parse("app=nonexistent")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	state := dw.InitialState()
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %d entries", len(state))
	}
}

func TestDiscoveryWatcher_Labels(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web", "version": "v1"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	// InitialLabels should contain labels for the discovered service.
	initialLabels := dw.InitialLabels()
	webLabels, ok := initialLabels["web"]
	if !ok {
		t.Fatal("expected 'web' in initial labels")
	}
	if webLabels["app"] != "web" {
		t.Errorf("expected app=web, got %q", webLabels["app"])
	}
	if webLabels["version"] != "v1" {
		t.Errorf("expected version=v1, got %q", webLabels["version"])
	}

	waitForWatch()

	// Update Service labels to trigger a resend with labels in the update.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Labels["version"] = "v2"
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Read updates until we see one with the new labels.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Labels != nil && u.Labels["version"] == "v2" {
				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for label update in BackendUpdate")
		}
	}
}

func TestDiscoveryWatcher_ContextCancellation(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = dw.Run(ctx)
		close(done)
	}()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Run to return after cancellation")
	}
}

// waitForInitial waits for the discovery watcher's initial sync to complete.
func waitForInitial(t *testing.T, dw *BackendDiscoveryWatcher) {
	t.Helper()
	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}
}

// --- Port override ---

func TestDiscoveryWatcher_PortOverride(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	// EndpointSlice with two ports: "http" on 8080 and "metrics" on 9090.
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "web"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
			TargetRef:  &corev1.ObjectReference{Name: "web-pod-0"},
		}},
		Ports: []discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
			{Name: new("metrics"), Port: new(int32(9090))},
		},
	}
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	// Override port to "9090" so the child BackendWatcher uses that port.
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "9090", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	eps, ok := state["web"]
	if !ok {
		t.Fatal("expected 'web' in initial state")
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 9090 {
		t.Errorf("Port = %d, want 9090 (port override)", eps[0].Port)
	}
}

// --- Multiple services matching same selector in same namespace ---

func TestDiscoveryWatcher_MultipleServicesMatchSelector(t *testing.T) {
	t.Parallel()
	svc1 := makeDiscoverableService("default", "web", map[string]string{"tier": "frontend"})
	slice1 := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	svc2 := makeDiscoverableService("default", "api", map[string]string{"tier": "frontend"})
	slice2 := makeDiscoverableEndpointSlice("default", "api", "api-abc", "10.0.0.2", 9090)
	clientset := fake.NewClientset(svc1, slice1, svc2, slice2)

	sel, _ := labels.Parse("tier=frontend")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if len(state) != 2 {
		t.Fatalf("expected 2 backends, got %d: %v", len(state), state)
	}
	if _, ok := state["web"]; !ok {
		t.Error("expected 'web' in state")
	}
	if _, ok := state["api"]; !ok {
		t.Error("expected 'api' in state")
	}
}

// --- Label change causes selector to stop matching (effective removal) ---

func TestDiscoveryWatcher_LabelChangeRemovesMatch(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if _, ok := state["web"]; !ok {
		t.Fatal("expected 'web' in initial state")
	}

	waitForWatch()

	// Update the Service labels so it no longer matches the selector.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Labels = map[string]string{"app": "other"}
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Should receive a removal update (Endpoints == nil).
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "web" && u.Endpoints == nil {
				return // success: service was removed
			}
		case <-deadline:
			t.Fatal("timeout waiting for removal after label change")
		}
	}
}

// --- Endpoint changes on discovered backends flow through Changes() ---

func TestDiscoveryWatcher_EndpointChangeFlowsThrough(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if eps := state["web"]; len(eps) != 1 || eps[0].Host != "10.0.0.1" {
		t.Fatalf("unexpected initial state: %v", eps)
	}

	waitForWatch()

	// Add a second endpoint to the existing EndpointSlice.
	slice2, err := clientset.DiscoveryV1().EndpointSlices("default").Get(ctx, "web-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting EndpointSlice: %v", err)
	}
	slice2.Endpoints = append(slice2.Endpoints, discoveryv1.Endpoint{
		Addresses:  []string{"10.0.0.2"},
		Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		TargetRef:  &corev1.ObjectReference{Name: "web-pod-1"},
	})
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, slice2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	// Read updates until we see one with 2 endpoints.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "web" && len(u.Endpoints) == 2 {
				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for endpoint change on discovered backend")
		}
	}
}

// --- Label update propagates to BackendUpdate.Labels in forwarding path ---

func TestDiscoveryWatcher_LabelUpdateInForwardingPath(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web", "version": "v1"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	waitForWatch()

	// Update labels on the Service without changing endpoints.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Labels["version"] = "v3"
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// The update should propagate as a BackendUpdate with updated labels.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Labels != nil && u.Labels["version"] == "v3" {
				// Also verify the update carries the service name.
				if u.Name != "web" {
					t.Errorf("Name = %q, want web", u.Name)
				}

				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for label update in forwarding path")
		}
	}
}

// --- Annotation tests ---

func TestDiscoveryWatcher_Annotations(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	svc.Annotations = map[string]string{"example.com/version": "v1", "example.com/tier": "backend"}
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	// InitialAnnotations should contain annotations for the discovered service.
	initialAnnotations := dw.InitialAnnotations()
	webAnnotations, ok := initialAnnotations["web"]
	if !ok {
		t.Fatal("expected 'web' in initial annotations")
	}
	if webAnnotations["example.com/version"] != "v1" {
		t.Errorf("expected example.com/version=v1, got %q", webAnnotations["example.com/version"])
	}
	if webAnnotations["example.com/tier"] != "backend" {
		t.Errorf("expected example.com/tier=backend, got %q", webAnnotations["example.com/tier"])
	}

	waitForWatch()

	// Update Service annotations to trigger a resend with annotations in the update.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Annotations["example.com/version"] = "v2"
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Read updates until we see one with the new annotations.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Annotations != nil && u.Annotations["example.com/version"] == "v2" {
				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for annotation update in BackendUpdate")
		}
	}
}

func TestDiscoveryWatcher_AnnotationUpdateInForwardingPath(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	svc.Annotations = map[string]string{"example.com/version": "v1"}
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	waitForWatch()

	// Update annotations on the Service without changing endpoints.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Annotations["example.com/version"] = "v3"
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// The update should propagate as a BackendUpdate with updated annotations.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Annotations != nil && u.Annotations["example.com/version"] == "v3" {
				if u.Name != "web" {
					t.Errorf("Name = %q, want web", u.Name)
				}

				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for annotation update in forwarding path")
		}
	}
}

// --- ExternalName service discovered as backend ---

func TestDiscoveryWatcher_ExternalNameService(t *testing.T) {
	t.Parallel()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ext",
			Namespace: "default",
			Labels:    map[string]string{"app": "ext"},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "example.com",
		},
	}
	clientset := fake.NewClientset(svc)

	sel, _ := labels.Parse("app=ext")
	// Provide port override since ExternalName services need it.
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "443", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	eps, ok := state["ext"]
	if !ok {
		t.Fatal("expected 'ext' in initial state")
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Host != "example.com" {
		t.Errorf("IP = %q, want example.com", eps[0].Host)
	}
	if eps[0].Port != 443 {
		t.Errorf("Port = %d, want 443", eps[0].Port)
	}
	if eps[0].Name != "external" {
		t.Errorf("Name = %q, want external", eps[0].Name)
	}
}

// --- ExternalName service with empty externalName ---

func TestDiscoveryWatcher_ExternalNameEmpty(t *testing.T) {
	t.Parallel()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ext",
			Namespace: "default",
			Labels:    map[string]string{"app": "ext"},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "",
		},
	}
	clientset := fake.NewClientset(svc)

	sel, _ := labels.Parse("app=ext")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "443", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	eps, ok := state["ext"]
	if !ok {
		t.Fatal("expected 'ext' in initial state")
	}
	// Empty externalName emits nil endpoints.
	if eps != nil {
		t.Errorf("Endpoints = %v, want nil for empty ExternalName", eps)
	}
}

// --- Duplicate service name across namespaces in all-namespace mode ---

func TestDiscoveryWatcher_DuplicateNameAcrossNamespaces(t *testing.T) {
	t.Parallel()
	// Two services named "web" in different namespaces.
	svc1 := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	slice1 := makeDiscoverableEndpointSlice("ns1", "web", "web-abc", "10.0.0.1", 8080)
	svc2 := makeDiscoverableService("ns2", "web", map[string]string{"app": "web"})
	slice2 := makeDiscoverableEndpointSlice("ns2", "web", "web-def", "10.0.0.2", 8080)
	clientset := fake.NewClientset(svc1, slice1, svc2, slice2)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "", true, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	// Both services have the same Name "web", and InitialState is keyed by
	// service name. The first discovered Service wins; the same-named
	// Service in the other namespace is skipped — only one entry.
	state := dw.InitialState()
	eps, ok := state["web"]
	if !ok {
		t.Fatal("expected 'web' in initial state")
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint for 'web', got %d", len(eps))
	}
	// We can't predict which one wins, but at least one IP should be present.
	if eps[0].Host != "10.0.0.1" && eps[0].Host != "10.0.0.2" {
		t.Errorf("unexpected IP %q", eps[0].Host)
	}
}

// --- Race between shutdown and informer callbacks (nil-map regression) ---

func TestDiscoveryWatcher_ShutdownRaceNoNilMapPanic(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")

	// Run many concurrent iterations to exercise the race.
	const iterations = 20
	var wg sync.WaitGroup
	wg.Add(iterations)
	for range iterations {
		go func() {
			defer wg.Done()
			dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				_ = dw.Run(ctx)
				close(done)
			}()
			select {
			case <-dw.Initial():
			case <-time.After(60 * time.Second):
				t.Error("timeout waiting for initial sync")
				cancel()

				return
			}
			// Cancel immediately to race with any pending informer events.
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("timeout waiting for Run to return")
			}
		}()
	}
	wg.Wait()
}

// --- Second service added dynamically after initial sync ---

func TestDiscoveryWatcher_SecondServiceAddedDynamically(t *testing.T) {
	t.Parallel()
	// Start with one matching service.
	svc1 := makeDiscoverableService("default", "web", map[string]string{"tier": "frontend"})
	slice1 := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc1, slice1)

	sel, _ := labels.Parse("tier=frontend")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if len(state) != 1 {
		t.Fatalf("expected 1 initial backend, got %d", len(state))
	}

	// Add a second matching service.
	svc2 := makeDiscoverableService("default", "api", map[string]string{"tier": "frontend"})
	slice2 := makeDiscoverableEndpointSlice("default", "api", "api-abc", "10.0.0.2", 9090)
	_, err := clientset.CoreV1().Services("default").Create(ctx, svc2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Service: %v", err)
	}

	waitForWatch()

	_, err = clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	// Read updates until we see endpoints from "api".
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "api" && len(u.Endpoints) > 0 {
				if u.Endpoints[0].Host != "10.0.0.2" {
					t.Errorf("IP = %q, want 10.0.0.2", u.Endpoints[0].Host)
				}

				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for second dynamically added backend")
		}
	}
}

// --- Service removed then re-added ---

func TestDiscoveryWatcher_ServiceRemovedThenReadded(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	waitForWatch()

	// Delete the service.
	err := clientset.CoreV1().Services("default").Delete(ctx, "web", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Service: %v", err)
	}

	// Wait for removal.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "web" && u.Endpoints == nil {
				goto removed
			}
		case <-deadline:
			t.Fatal("timeout waiting for removal")
		}
	}
removed:

	waitForWatch()

	// Re-create the service. The original EndpointSlice "web-abc" still
	// exists in the API, so the new child BackendWatcher will pick it up.
	svc2 := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	_, err = clientset.CoreV1().Services("default").Create(ctx, svc2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("re-creating Service: %v", err)
	}

	// Read updates until we see the re-added backend with endpoints.
	deadline = time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "web" && len(u.Endpoints) > 0 {
				return // success — service was re-discovered
			}
		case <-deadline:
			t.Fatal("timeout waiting for re-added backend")
		}
	}
}

// --- Removal update includes Labels ---

func TestDiscoveryWatcher_RemovalUpdateLabels(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	waitForWatch()

	err := clientset.CoreV1().Services("default").Delete(ctx, "web", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Service: %v", err)
	}

	// Removal events don't carry labels (the Service is gone).
	// This documents the current behavior.
	readDiscoveryRemoval(t, dw, "web")
}

func TestDiscoveryWatcher_SelectorRematchAfterLabelRestore(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if _, ok := state["web"]; !ok {
		t.Fatal("expected 'web' in initial state")
	}

	waitForWatch()

	// Change labels so the service no longer matches the selector.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Labels = map[string]string{"app": "other"}
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service labels to unmatch: %v", err)
	}

	// Wait for removal.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "web" && u.Endpoints == nil {
				goto removed
			}
		case <-deadline:
			t.Fatal("timeout waiting for removal after label unmatch")
		}
	}
removed:

	waitForWatch()

	// Update the EndpointSlice to a different IP while service is unmatched.
	slice2, err := clientset.DiscoveryV1().EndpointSlices("default").Get(ctx, "web-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting EndpointSlice: %v", err)
	}
	slice2.Endpoints[0].Addresses = []string{"10.0.0.99"}
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, slice2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	waitForWatch()

	// Restore labels so the service matches again.
	svc3, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc3.Labels = map[string]string{"app": "web"}
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc3, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service labels to rematch: %v", err)
	}

	// Should be re-discovered with the new endpoint.
	deadline = time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "web" && len(u.Endpoints) > 0 && u.Endpoints[0].Host == "10.0.0.99" {
				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for re-discovery with new endpoints")
		}
	}
}

func TestDiscoveryWatcher_ExternalNameToClusterIPTransition(t *testing.T) {
	t.Parallel()
	// Start with an ExternalName service.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api",
			Namespace: "default",
			Labels:    map[string]string{"app": "api"},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "api.example.com",
		},
	}
	clientset := fake.NewClientset(svc)

	sel, _ := labels.Parse("app=api")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "8080", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	eps, ok := state["api"]
	if !ok {
		t.Fatal("expected 'api' in initial state")
	}
	if len(eps) != 1 || eps[0].Host != "api.example.com" {
		t.Fatalf("expected ExternalName endpoint, got %v", eps)
	}

	waitForWatch()

	// Transition to ClusterIP.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Spec.Type = corev1.ServiceTypeClusterIP
	svc2.Spec.ExternalName = ""
	svc2.Spec.ClusterIP = "10.96.0.1"
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service to ClusterIP: %v", err)
	}

	// Create an EndpointSlice for the ClusterIP service.
	slice := makeDiscoverableEndpointSlice("default", "api", "api-abc", "10.0.0.5", 8080)
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	// Read updates until we see the ClusterIP endpoint.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name == "api" && len(u.Endpoints) > 0 && u.Endpoints[0].Host == "10.0.0.5" {
				return // success
			}
		case <-deadline:
			t.Fatal("timeout waiting for ClusterIP endpoint after type transition")
		}
	}
}

func TestDiscoveryWatcher_InitialSnapshotMixedTypes(t *testing.T) {
	t.Parallel()
	// ClusterIP service with EndpointSlice.
	clusterSvc := makeDiscoverableService("default", "web", map[string]string{"tier": "app"})
	clusterSlice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)

	// ExternalName service.
	extSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cdn",
			Namespace: "default",
			Labels:    map[string]string{"tier": "app"},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "cdn.example.com",
		},
	}
	clientset := fake.NewClientset(clusterSvc, clusterSlice, extSvc)

	sel, _ := labels.Parse("tier=app")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "8080", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if len(state) != 2 {
		t.Fatalf("expected 2 backends in initial state, got %d: %v", len(state), state)
	}

	webEps, ok := state["web"]
	if !ok {
		t.Fatal("expected 'web' in initial state")
	}
	if len(webEps) != 1 || webEps[0].Host != "10.0.0.1" {
		t.Errorf("web endpoints = %v, want [{Host:10.0.0.1}]", webEps)
	}

	cdnEps, ok := state["cdn"]
	if !ok {
		t.Fatal("expected 'cdn' in initial state")
	}
	if len(cdnEps) != 1 || cdnEps[0].Host != "cdn.example.com" {
		t.Errorf("cdn endpoints = %v, want [{Host:cdn.example.com}]", cdnEps)
	}
	if cdnEps[0].Port != 8080 {
		t.Errorf("cdn port = %d, want 8080", cdnEps[0].Port)
	}
}

func TestDiscoveryWatcher_RapidLabelFlap(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if _, ok := state["web"]; !ok {
		t.Fatal("expected 'web' in initial state")
	}

	waitForWatch()

	// Rapid flap: match → unmatch → match.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Labels = map[string]string{"app": "other"}
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("flap to unmatch: %v", err)
	}

	waitForWatch()

	svc3, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc3.Labels = map[string]string{"app": "web"}
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc3, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("flap back to match: %v", err)
	}

	// Drain all updates and verify the final state: "web" should be discovered
	// with endpoints (no stale removal). We may see intermediate removal and
	// re-addition updates.
	deadline := time.After(60 * time.Second)
	var lastUpdate *BackendUpdate

	for {
		select {
		case u := <-dw.Changes():
			if u.Name != "web" {
				continue
			}

			uCopy := u
			lastUpdate = &uCopy

			if len(u.Endpoints) == 0 {
				continue
			}

			// Service was re-discovered with endpoints. Keep draining
			// briefly to make sure no stale removal follows.
			timer := time.NewTimer(500 * time.Millisecond)
			for draining := true; draining; {
				select {
				case u2 := <-dw.Changes():
					if u2.Name == "web" {
						u2Copy := u2
						lastUpdate = &u2Copy
					}
				case <-timer.C:
					draining = false
				}
			}
			timer.Stop()

			if lastUpdate.Endpoints == nil {
				t.Fatal("final state after rapid flap is removal — expected endpoints")
			}

			return // success
		case <-deadline:
			t.Fatalf("timeout waiting for re-discovery after rapid flap; last update: %v", lastUpdate)
		}
	}
}

func TestDiscoveryWatcher_ServiceAddedDuringInitPhase(t *testing.T) {
	t.Parallel()
	// Two services exist at startup — both should appear in InitialState.
	svc1 := makeDiscoverableService("default", "web", map[string]string{"tier": "frontend"})
	slice1 := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	svc2 := makeDiscoverableService("default", "api", map[string]string{"tier": "frontend"})
	slice2 := makeDiscoverableEndpointSlice("default", "api", "api-abc", "10.0.0.2", 9090)
	clientset := fake.NewClientset(svc1, slice1, svc2, slice2)

	sel, _ := labels.Parse("tier=frontend")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)

	state := dw.InitialState()
	if len(state) != 2 {
		t.Fatalf("expected 2 backends in InitialState, got %d: %v", len(state), state)
	}

	webEps, ok := state["web"]
	if !ok {
		t.Fatal("expected 'web' in initial state")
	}
	if len(webEps) != 1 || webEps[0].Host != "10.0.0.1" {
		t.Errorf("web = %v, want [{Host:10.0.0.1 Port:8080}]", webEps)
	}
	if webEps[0].Port != 8080 {
		t.Errorf("web port = %d, want 8080", webEps[0].Port)
	}

	apiEps, ok := state["api"]
	if !ok {
		t.Fatal("expected 'api' in initial state")
	}
	if len(apiEps) != 1 || apiEps[0].Host != "10.0.0.2" {
		t.Errorf("api = %v, want [{Host:10.0.0.2 Port:9090}]", apiEps)
	}
	if apiEps[0].Port != 9090 {
		t.Errorf("api port = %d, want 9090", apiEps[0].Port)
	}
}

// TestDiscoveryWatcher_InitialCollectionSkipsRemovedBackend reproduces a
// startup-hang race: if a discovered Service is removed during the window
// between the initial reconciliation and the initial-state collection, the
// child watcher's context is cancelled and its Changes() channel never
// delivers. collectInitialState must skip such a backend instead of blocking
// forever (which would leave Initial() unclosed and varnishd unstarted).
func TestDiscoveryWatcher_InitialCollectionSkipsRemovedBackend(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Backend "a": healthy — its first endpoint set is already buffered, and
	// its context is live.
	bwA := NewBackendWatcher(clientset, "default", "a", "")
	bwA.ch <- []Endpoint{{Host: "10.0.0.1", Port: 8080, Name: "a-pod-0"}}
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	mbA := &managedBackend{watcher: bwA, ctx: ctxA, cancel: cancelA, namespace: "default", name: "a"}

	// Backend "b": its Service was removed mid-startup — the child watcher is
	// cancelled and will never deliver on Changes().
	bwB := NewBackendWatcher(clientset, "default", "b", "")
	ctxB, cancelB := context.WithCancel(ctx)
	cancelB()
	mbB := &managedBackend{watcher: bwB, ctx: ctxB, cancel: cancelB, namespace: "default", name: "b"}

	pending := map[string]*managedBackend{"default/a": mbA, "default/b": mbB}

	done := make(chan error, 1)
	go func() { done <- dw.collectInitialState(ctx, pending) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("collectInitialState returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collectInitialState hung: a backend removed mid-collection blocked the initial sync")
	}

	state := dw.InitialState()
	if _, ok := state["a"]; !ok {
		t.Errorf("expected healthy backend 'a' in initial state, got %v", state)
	}
	if _, ok := state["b"]; ok {
		t.Error("removed backend 'b' should be skipped, but it is present in initial state")
	}
}

// --- Stub lister for syncServices unit tests ---

type stubServiceLister struct {
	services []*corev1.Service
	err      error
}

func (l *stubServiceLister) List(_ labels.Selector) ([]*corev1.Service, error) {
	return l.services, l.err
}

func (l *stubServiceLister) Services(_ string) corelisters.ServiceNamespaceLister {
	return &stubServiceNamespaceLister{services: l.services, err: l.err}
}

type stubServiceNamespaceLister struct {
	services []*corev1.Service
	err      error
}

func (l *stubServiceNamespaceLister) List(_ labels.Selector) ([]*corev1.Service, error) {
	return l.services, l.err
}

func (l *stubServiceNamespaceLister) Get(_ string) (*corev1.Service, error) {
	return nil, l.err
}

func TestDiscoveryWatcher_SyncServicesListerError(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")

	for _, allNS := range []bool{false, true} {
		name := "namespaced"
		if allNS {
			name = "allNamespaces"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dw := NewBackendDiscoveryWatcher(clientset, "default", allNS, sel, "", nil)

			// Pre-populate a backend to verify it survives the lister error.
			bw := NewBackendWatcher(clientset, "default", "web", "")
			_, childCancel := context.WithCancel(t.Context())
			defer childCancel()
			dw.backends["default/web"] = &managedBackend{
				watcher:   bw,
				cancel:    childCancel,
				namespace: "default",
				name:      "web",
			}
			dw.initialized = true

			listerErr := errors.New("simulated API server error")
			dw.syncServices(t.Context(), &stubServiceLister{err: listerErr})

			// Backend should still be present — lister error causes early return
			// without reconciling (no spurious removals).
			dw.mu.Lock()
			_, exists := dw.backends["default/web"]
			dw.mu.Unlock()
			if !exists {
				t.Fatal("backend was removed despite lister error; syncServices should return early")
			}
		})
	}
}

func TestDiscoveryWatcher_RemovalDeliveredWhenChannelFull(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	// Pre-populate a backend.
	bw := NewBackendWatcher(clientset, "default", "web", "")
	_, childCancel := context.WithCancel(t.Context())
	defer childCancel()
	_, fwdCancel := context.WithCancel(t.Context())
	defer fwdCancel()
	dw.backends["default/web"] = &managedBackend{
		watcher:   bw,
		cancel:    childCancel,
		fwdCancel: fwdCancel,
		namespace: "default",
		name:      "web",
	}
	dw.initialized = true

	// Fill the update channel to capacity.
	for i := range cap(dw.updateCh) {
		dw.updateCh <- BackendUpdate{Name: fmt.Sprintf("filler-%d", i)}
	}

	// Call syncServices with an empty lister (no services match). A removed
	// Service produces no further events, so a dropped removal would leave
	// the deleted backend in the VCL forever — it must be delivered even
	// when the channel is momentarily full.
	done := make(chan struct{})
	go func() {
		dw.syncServices(t.Context(), &stubServiceLister{})
		close(done)
	}()

	// Give syncServices time to either finish (if it drops the removal) or
	// block on the full channel (correct behavior) before draining.
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}

	found := false
	timeout := time.After(60 * time.Second)
drain:
	for {
		select {
		case u := <-dw.updateCh:
			if u.Name == "web" && u.Endpoints == nil {
				found = true

				break drain
			}
		case <-timeout:
			break drain
		default:
			// Channel momentarily empty: if syncServices already returned,
			// nothing more is coming.
			select {
			case <-done:
				break drain
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	if !found {
		t.Fatal("removal update was dropped; it must be delivered even when the channel is full")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncServices did not return after removal was consumed")
	}

	// Verify the backend was removed from the internal map.
	dw.mu.Lock()
	_, exists := dw.backends["default/web"]
	dw.mu.Unlock()
	if exists {
		t.Fatal("backend was not removed from internal map")
	}
}

// TestDiscoveryWatcher_RemovalCarriesIncarnationGen pins the generation
// contract the event-loop consumer relies on to drop stale forward updates (the
// resurrection fix): a removal update carries the gen of the removed
// incarnation, and a subsequent re-add gets a strictly higher gen. If the
// watcher ever stopped stamping the gen, reused it, or decreased it, the
// consumer would either resurrect removed backends or drop legitimate re-adds.
func TestDiscoveryWatcher_RemovalCarriesIncarnationGen(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	present := &stubServiceLister{services: []*corev1.Service{svc}}
	absent := &stubServiceLister{}

	// Initial discovery assigns a gen. Forwarding stays off (initialized=false)
	// so updateCh receives only the removal triggered below, keeping the read
	// deterministic.
	dw.syncServices(ctx, present)

	dw.mu.Lock()
	mb1, ok := dw.backends["default/web"]
	dw.mu.Unlock()
	if !ok {
		t.Fatal("expected 'web' to be discovered")
	}
	g1 := mb1.gen
	if g1 == 0 {
		t.Fatal("discovered backend has gen 0; discovery incarnations must use a non-zero gen")
	}

	// Removal must carry the removed incarnation's gen.
	dw.syncServices(ctx, absent)
	select {
	case u := <-dw.updateCh:
		if u.Name != "web" || u.Endpoints != nil {
			t.Fatalf("expected removal of 'web' (nil endpoints), got %+v", u)
		}
		if u.Gen != g1 {
			t.Fatalf("removal gen = %d, want %d (the removed incarnation's gen)", u.Gen, g1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for removal update")
	}

	// Re-add must get a strictly higher gen so the consumer accepts it over the
	// tombstone left by the removal.
	dw.syncServices(ctx, present)
	dw.mu.Lock()
	mb2, ok := dw.backends["default/web"]
	dw.mu.Unlock()
	if !ok {
		t.Fatal("expected 'web' to be re-added")
	}
	if mb2.gen <= g1 {
		t.Fatalf("re-added gen = %d, want strictly greater than %d", mb2.gen, g1)
	}
}

// TestDiscoveryWatcher_NameRegistrySkipsCrossWatcherCollision verifies that two
// discovery watchers sharing a NameRegistry never both manage a backend with the
// same bare name (the consumer keys backends by bare name). The first to claim
// it wins; the second skips; and the name becomes claimable again once the owner
// releases it on removal.
func TestDiscoveryWatcher_NameRegistrySkipsCrossWatcherCollision(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	reg := NewNameRegistry()

	dwA := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dwA.SetNameRegistry(reg)
	dwB := NewBackendDiscoveryWatcher(clientset, "ns2", false, sel, "", nil)
	dwB.SetNameRegistry(reg)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	svcA := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	svcB := makeDiscoverableService("ns2", "web", map[string]string{"app": "web"})
	listerA := &stubServiceLister{services: []*corev1.Service{svcA}}
	listerB := &stubServiceLister{services: []*corev1.Service{svcB}}

	// A claims "web" first; B must skip it.
	dwA.syncServices(ctx, listerA)
	dwB.syncServices(ctx, listerB)

	if !backendExists(dwA, "ns1/web") {
		t.Fatal("watcher A should manage 'web'")
	}
	if backendExists(dwB, "ns2/web") {
		t.Fatal("watcher B must skip 'web' while A owns the name")
	}

	// A's Service goes away: A releases the claim, so B can take over "web".
	dwA.syncServices(ctx, &stubServiceLister{})
	dwB.syncServices(ctx, listerB)

	if backendExists(dwA, "ns1/web") {
		t.Fatal("watcher A should have removed 'web'")
	}
	if !backendExists(dwB, "ns2/web") {
		t.Fatal("watcher B should manage 'web' after A released it")
	}
}

// TestDiscoveryWatcher_CrossWatcherGenStrictlyIncreasing pins the cross-watcher
// generation contract the event-loop consumer relies on. The consumer keys its
// removedGen tombstone by bare backend name (shared across all discovery
// watchers) and drops any add whose gen is <= the tombstone. So when one watcher
// removes a name and another watcher (sharing the registry) later re-claims it,
// the re-claim MUST get a gen strictly greater than the gen the first watcher
// removed it with — otherwise the consumer silently drops the new owner's
// backend for the entire lifetime of its ownership.
//
// Per-watcher generation counters violate this: each watcher's sequence starts
// at 1, so the second watcher's first gen collides with (and is <=) the first
// watcher's removal gen.
func TestDiscoveryWatcher_CrossWatcherGenStrictlyIncreasing(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	reg := NewNameRegistry()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dwA := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dwA.SetNameRegistry(reg)
	dwB := NewBackendDiscoveryWatcher(clientset, "ns2", false, sel, "", nil)
	dwB.SetNameRegistry(reg)

	lbls := map[string]string{"app": "web"}
	filler1 := makeDiscoverableService("ns1", "a", lbls)
	filler2 := makeDiscoverableService("ns1", "b", lbls)
	webA := makeDiscoverableService("ns1", "web", lbls)

	// Push watcher A's generation counter up by discovering unrelated Services
	// first (one new Service per sync so the assigned gens are deterministic),
	// so the incarnation A eventually creates for "web" carries a high gen.
	dwA.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{filler1}})
	dwA.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{filler1, filler2}})
	dwA.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{filler1, filler2, webA}})

	dwA.mu.Lock()
	genARemoved := dwA.backends["ns1/web"].gen
	dwA.mu.Unlock()

	// A's "web" disappears: the registry claim is released and the removal
	// carries genARemoved (the consumer records removedGen["web"] = genARemoved).
	dwA.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{filler1, filler2}})
	if registryClaimed(reg, "web") {
		t.Fatal("removal must release the registry claim so B can take over")
	}

	// Watcher B now claims "web" (different namespace) and assigns its own gen.
	webB := makeDiscoverableService("ns2", "web", lbls)
	dwB.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{webB}})

	dwB.mu.Lock()
	mb, ok := dwB.backends["ns2/web"]
	dwB.mu.Unlock()
	if !ok {
		t.Fatal("watcher B should manage 'web' after A released it")
	}

	if mb.gen <= genARemoved {
		t.Fatalf("cross-watcher gen collision: B re-claim gen = %d, want strictly greater "+
			"than A removal gen = %d (else the consumer's removedGen tombstone drops it)",
			mb.gen, genARemoved)
	}
}

// TestDiscoveryWatcher_NameRegistrySameServiceTwoWatchers covers the case two
// watchers match the exact same namespace/serviceName (overlapping selectors).
// The per-watcher id in the owner token must keep them distinct so only one
// claims the name.
func TestDiscoveryWatcher_NameRegistrySameServiceTwoWatchers(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	reg := NewNameRegistry()

	dwA := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dwA.SetNameRegistry(reg)
	dwB := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dwB.SetNameRegistry(reg)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	svc := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	lister := &stubServiceLister{services: []*corev1.Service{svc}}

	dwA.syncServices(ctx, lister)
	dwB.syncServices(ctx, lister)

	if !backendExists(dwA, "ns1/web") {
		t.Fatal("watcher A should manage 'ns1/web'")
	}
	if backendExists(dwB, "ns1/web") {
		t.Fatal("watcher B must skip 'ns1/web' — same name already claimed by A")
	}
}

func backendExists(dw *BackendDiscoveryWatcher, key string) bool {
	dw.mu.Lock()
	defer dw.mu.Unlock()
	_, ok := dw.backends[key]

	return ok
}

//nolint:unparam // generic helper; current callers all happen to query "web".
func registryClaimed(r *NameRegistry, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.owners[name]

	return ok
}

// TestDiscoveryWatcher_NameRegistryThreeWatchersSingleOwner verifies that when
// three watchers sharing a registry all match the same Service, exactly one
// ends up managing it.
func TestDiscoveryWatcher_NameRegistryThreeWatchersSingleOwner(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	reg := NewNameRegistry()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	svc := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	lister := &stubServiceLister{services: []*corev1.Service{svc}}

	owners := 0
	for range 3 {
		dw := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
		dw.SetNameRegistry(reg)
		dw.syncServices(ctx, lister)
		if backendExists(dw, "ns1/web") {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("exactly one of three watchers should own 'web', got %d", owners)
	}
}

// TestDiscoveryWatcher_NameRegistryDisabledNoCrossWatcherDedup documents that
// cross-watcher dedup is opt-in: without a shared registry, two watchers each
// manage their own same-named Service (the pre-registry behavior).
func TestDiscoveryWatcher_NameRegistryDisabledNoCrossWatcherDedup(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dwA := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dwB := NewBackendDiscoveryWatcher(clientset, "ns2", false, sel, "", nil)
	// Neither watcher gets a registry.

	dwA.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{
		makeDiscoverableService("ns1", "web", map[string]string{"app": "web"}),
	}})
	dwB.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{
		makeDiscoverableService("ns2", "web", map[string]string{"app": "web"}),
	}})

	if !backendExists(dwA, "ns1/web") || !backendExists(dwB, "ns2/web") {
		t.Fatal("without a shared registry, each watcher should manage its own 'web'")
	}
}

// TestDiscoveryWatcher_NameRegistryExplicitNameNotClaimed verifies a Service
// whose name is reserved by an explicit --backend is skipped before the registry
// is consulted, so it neither becomes a backend nor consumes a claim.
func TestDiscoveryWatcher_NameRegistryExplicitNameNotClaimed(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	reg := NewNameRegistry()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dw := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", map[string]bool{"web": true})
	dw.SetNameRegistry(reg)

	dw.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{
		makeDiscoverableService("ns1", "web", map[string]string{"app": "web"}),
	}})

	if backendExists(dw, "ns1/web") {
		t.Fatal("a Service whose name is an explicit --backend must be skipped")
	}
	if registryClaimed(reg, "web") {
		t.Fatal("explicit-name skip must not consume a registry claim")
	}
}

// TestDiscoveryWatcher_NameRegistryReclaimAfterRemovalSameWatcher verifies that
// removal releases the claim and the same watcher can re-claim the name with a
// strictly higher generation (the registry and gen tombstone working together).
func TestDiscoveryWatcher_NameRegistryReclaimAfterRemovalSameWatcher(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	reg := NewNameRegistry()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dw := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dw.SetNameRegistry(reg)

	svc := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	present := &stubServiceLister{services: []*corev1.Service{svc}}

	dw.syncServices(ctx, present)
	dw.mu.Lock()
	gen1 := dw.backends["ns1/web"].gen
	dw.mu.Unlock()
	if !registryClaimed(reg, "web") {
		t.Fatal("discovering 'web' should claim it in the registry")
	}

	// Remove → claim released.
	dw.syncServices(ctx, &stubServiceLister{})
	if registryClaimed(reg, "web") {
		t.Fatal("removal must release the registry claim")
	}

	// Re-add by the same watcher → re-claims with a strictly higher gen.
	dw.syncServices(ctx, present)
	dw.mu.Lock()
	mb, ok := dw.backends["ns1/web"]
	dw.mu.Unlock()
	if !ok {
		t.Fatal("same watcher should re-claim 'web' after releasing it")
	}
	if mb.gen <= gen1 {
		t.Fatalf("re-added gen = %d, want strictly greater than %d", mb.gen, gen1)
	}
}

// TestDiscoveryWatcher_NameRegistryShutdownReleasesClaims drives the real Run()
// path: a running watcher holds its claim, and shutting it down (ctx cancel)
// releases the claim so another watcher sharing the registry can take over.
func TestDiscoveryWatcher_NameRegistryShutdownReleasesClaims(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("ns1", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)
	sel, _ := labels.Parse("app=web")
	reg := NewNameRegistry()

	dwA := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dwA.SetNameRegistry(reg)

	ctxA, cancelA := context.WithCancel(t.Context())
	go func() { _ = dwA.Run(ctxA) }()
	waitForInitial(t, dwA)

	if !registryClaimed(reg, "web") {
		t.Fatal("a running watcher should hold the 'web' claim")
	}

	// Shut A down; Run's cleanup must release the claim.
	cancelA()
	deadline := time.After(5 * time.Second)
	for registryClaimed(reg, "web") {
		select {
		case <-deadline:
			t.Fatal("shutdown did not release the registry claim")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// A fresh watcher sharing the registry can now claim "web".
	dwB := NewBackendDiscoveryWatcher(clientset, "ns1", false, sel, "", nil)
	dwB.SetNameRegistry(reg)
	dwB.syncServices(t.Context(), &stubServiceLister{services: []*corev1.Service{svc}})
	if !backendExists(dwB, "ns1/web") {
		t.Fatal("after A shut down, B should claim 'web'")
	}
}

func TestDiscoveryWatcher_ScaleToZeroIsNotRemoval(t *testing.T) {
	t.Parallel()
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)
	waitForWatch()

	// All endpoints become unready (e.g. rolling restart). The Service still
	// exists, so the update must NOT look like a removal: nil Endpoints is
	// reserved for "Service removed".
	updated := slice.DeepCopy()
	updated.Endpoints[0].Conditions.Ready = new(false)
	_, err := clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	u := readDiscoveryUpdate(t, dw)
	if u.Name != "web" {
		t.Errorf("Name = %q, want web", u.Name)
	}
	if len(u.Endpoints) != 0 {
		t.Fatalf("expected 0 endpoints, got %d", len(u.Endpoints))
	}
	if u.Endpoints == nil {
		t.Fatal("Endpoints = nil for a Service that still exists; nil must be reserved for removal")
	}
}

func TestDiscoveryWatcher_DuplicateNameInOtherNamespaceSkipped(t *testing.T) {
	t.Parallel()
	svc1 := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	slice1 := makeDiscoverableEndpointSlice("ns1", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc1, slice1)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "", true, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	waitForInitial(t, dw)
	waitForWatch()

	// A same-named Service appears in another namespace. Updates are keyed
	// by bare Service name, so adopting it would make two watchers fight
	// over the "web" backend group downstream — it must be skipped.
	svc2 := makeDiscoverableService("ns2", "web", map[string]string{"app": "web"})
	slice2 := makeDiscoverableEndpointSlice("ns2", "web", "web-def", "10.0.0.99", 8080)
	_, err := clientset.CoreV1().Services("ns2").Create(ctx, svc2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Service: %v", err)
	}
	_, err = clientset.DiscoveryV1().EndpointSlices("ns2").Create(ctx, slice2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	waitForWatch()

	// Positive sync barrier: change ns1's endpoints and wait for that update.
	updated := slice1.DeepCopy()
	updated.Endpoints[0].Addresses = []string{"10.0.0.2"}
	_, err = clientset.DiscoveryV1().EndpointSlices("ns1").Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Name != "web" {
				t.Errorf("Name = %q, want web", u.Name)
			}
			for _, ep := range u.Endpoints {
				if ep.Host == "10.0.0.99" {
					t.Fatal("received endpoints of the same-named Service from another namespace; it should be skipped")
				}
				if ep.Host == "10.0.0.2" {
					return // ns1 change arrived without any collision update
				}
			}
		case <-deadline:
			t.Fatal("timeout waiting for ns1 endpoint update")
		}
	}
}

// TestDiscoveryWatcher_SurvivorLazilyPromotedAfterWinnerRemoved locks in the
// intentional handling of the ambiguous "same bare name in two namespaces"
// config. The first match wins (first-come-first-served) and the same-name
// Service in the other namespace is skipped. When the winner is removed, the
// survivor is NOT promoted in that same reconcile (the add pass still sees the
// about-to-be-removed winner), but IS adopted on the next reconcile that
// observes the conflict cleared. The watcher never manages two same-named
// Services at once and never re-points a backend mid-reconcile.
func TestDiscoveryWatcher_SurvivorLazilyPromotedAfterWinnerRemoved(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "", true, sel, "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	svc1 := makeDiscoverableService("ns1", "web", map[string]string{"app": "web"})
	svc2 := makeDiscoverableService("ns2", "web", map[string]string{"app": "web"})

	// Both present: exactly one wins.
	dw.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{svc1, svc2}})
	has1, has2 := backendExists(dw, "ns1/web"), backendExists(dw, "ns2/web")
	if has1 == has2 {
		t.Fatalf("expected exactly one winner, got ns1/web=%v ns2/web=%v", has1, has2)
	}

	winnerKey, survivorKey, survivor := "ns1/web", "ns2/web", svc2
	if has2 {
		winnerKey, survivorKey, survivor = "ns2/web", "ns1/web", svc1
	}

	// Reconcile that removes the winner: the winner is cleanly removed and the
	// survivor is NOT promoted in the same pass.
	dw.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{survivor}})
	if backendExists(dw, winnerKey) {
		t.Errorf("removed winner %q should no longer be managed", winnerKey)
	}
	if backendExists(dw, survivorKey) {
		t.Errorf("survivor %q must not be promoted in the winner's removal reconcile", survivorKey)
	}

	// Next reconcile (conflict cleared): the survivor is adopted.
	dw.syncServices(ctx, &stubServiceLister{services: []*corev1.Service{survivor}})
	if !backendExists(dw, survivorKey) {
		t.Errorf("survivor %q should be adopted on the next reconcile after the winner is gone", survivorKey)
	}
}

func TestDiscoveryWatcher_ContextCancelDuringInitialCollection(t *testing.T) {
	t.Parallel()

	// A ClusterIP service needs an EndpointSlice watcher to resolve endpoints.
	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	clientset := fake.NewClientset(svc)

	// Block EndpointSlice list operations so the child BackendWatcher
	// never completes its initial sync and never sends to Changes().
	blocker := make(chan struct{})
	t.Cleanup(func() { close(blocker) })
	blocked := make(chan struct{}, 1)
	clientset.PrependReactor("list", "endpointslices", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-blocker

		return false, nil, nil
	})

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- dw.Run(ctx)
	}()

	// Wait until the child watcher's endpointslice list is blocked,
	// confirming the discovery watcher is stuck in initial collection.
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for endpointslice list to be attempted")
	}

	// Cancel the context while waiting for initial endpoint collection.
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}

func TestDiscoveryWatcher_ExcludeAnnotations(t *testing.T) {
	t.Parallel()

	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	svc.Annotations = map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"big":"json"}`,
		"example.com/version":                              "v1",
	}
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)
	dw.SetExcludeAnnotations(BuildAnnotationFilter(DefaultExcludeAnnotations))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	// InitialAnnotations should not contain the excluded key.
	initialAnnotations := dw.InitialAnnotations()
	webAnnotations, ok := initialAnnotations["web"]
	if !ok {
		t.Fatal("expected 'web' in initial annotations")
	}
	if _, excluded := webAnnotations["kubectl.kubernetes.io/last-applied-configuration"]; excluded {
		t.Error("expected kubectl.kubernetes.io/last-applied-configuration to be excluded from InitialAnnotations")
	}
	if webAnnotations["example.com/version"] != "v1" {
		t.Errorf("expected example.com/version=v1, got %q", webAnnotations["example.com/version"])
	}

	waitForWatch()

	// Update annotations — the forwarded update should also be filtered.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Annotations["example.com/version"] = "v2"
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Annotations != nil && u.Annotations["example.com/version"] == "v2" {
				if _, excluded := u.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; excluded {
					t.Error("expected excluded annotation to not appear in forwarded BackendUpdate")
				}

				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for annotation update")
		}
	}
}

func TestDiscoveryWatcher_ExcludeAnnotationsPrefixMatch(t *testing.T) {
	t.Parallel()

	svc := makeDiscoverableService("default", "web", map[string]string{"app": "web"})
	svc.Annotations = map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"big":"json"}`,
		"kubectl.kubernetes.io/restartedAt":                "2026-01-01",
		"example.com/version":                              "v1",
	}
	slice := makeDiscoverableEndpointSlice("default", "web", "web-abc", "10.0.0.1", 8080)
	clientset := fake.NewClientset(svc, slice)

	sel, _ := labels.Parse("app=web")
	dw := NewBackendDiscoveryWatcher(clientset, "default", false, sel, "", nil)
	dw.SetExcludeAnnotations(BuildAnnotationFilter([]string{"kubectl.kubernetes.io/*"}))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = dw.Run(ctx) }()

	select {
	case <-dw.Initial():
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for initial sync")
	}

	// InitialAnnotations should exclude all kubectl.kubernetes.io/* keys.
	initialAnnotations := dw.InitialAnnotations()
	webAnnotations, ok := initialAnnotations["web"]
	if !ok {
		t.Fatal("expected 'web' in initial annotations")
	}
	if _, excluded := webAnnotations["kubectl.kubernetes.io/last-applied-configuration"]; excluded {
		t.Error("expected kubectl.kubernetes.io/last-applied-configuration to be excluded by prefix")
	}
	if _, excluded := webAnnotations["kubectl.kubernetes.io/restartedAt"]; excluded {
		t.Error("expected kubectl.kubernetes.io/restartedAt to be excluded by prefix")
	}
	if webAnnotations["example.com/version"] != "v1" {
		t.Errorf("expected example.com/version=v1, got %q", webAnnotations["example.com/version"])
	}
	if len(webAnnotations) != 1 {
		t.Errorf("expected exactly 1 annotation remaining, got %d: %v", len(webAnnotations), webAnnotations)
	}

	waitForWatch()

	// Update annotations — forwarded update should also be prefix-filtered.
	svc2, err := clientset.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	svc2.Annotations["example.com/version"] = "v2"
	svc2.Annotations["kubectl.kubernetes.io/new-key"] = "new-value"
	_, err = clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	deadline := time.After(60 * time.Second)
	for {
		select {
		case u := <-dw.Changes():
			if u.Annotations != nil && u.Annotations["example.com/version"] == "v2" {
				if _, excluded := u.Annotations["kubectl.kubernetes.io/new-key"]; excluded {
					t.Error("expected kubectl.kubernetes.io/new-key to be excluded by prefix in update")
				}
				if _, excluded := u.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; excluded {
					t.Error("expected kubectl.kubernetes.io/last-applied-configuration to be excluded in update")
				}
				if len(u.Annotations) != 1 {
					t.Errorf("expected exactly 1 annotation in update, got %d: %v", len(u.Annotations), u.Annotations)
				}

				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for prefix-filtered annotation update")
		}
	}
}
