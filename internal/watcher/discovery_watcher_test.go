package watcher

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"
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
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", eps[0].IP)
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

	u := readDiscoveryUpdate(t, dw)
	if u.Name != "web" {
		t.Errorf("Name = %q, want web", u.Name)
	}
	if u.Endpoints != nil {
		t.Errorf("Endpoints = %v, want nil (removal)", u.Endpoints)
	}
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
	if eps := state["web"]; len(eps) != 1 || eps[0].IP != "10.0.0.1" {
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
	if eps[0].IP != "example.com" {
		t.Errorf("IP = %q, want example.com", eps[0].IP)
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

	// Both services have the same Name "web", so InitialState is keyed by
	// service name. The second one overwrites the first — only one entry.
	state := dw.InitialState()
	// This documents current behavior: last-write-wins for duplicate names.
	eps, ok := state["web"]
	if !ok {
		t.Fatal("expected 'web' in initial state")
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint for 'web', got %d", len(eps))
	}
	// We can't predict which one wins, but at least one IP should be present.
	if eps[0].IP != "10.0.0.1" && eps[0].IP != "10.0.0.2" {
		t.Errorf("unexpected IP %q", eps[0].IP)
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
				if u.Endpoints[0].IP != "10.0.0.2" {
					t.Errorf("IP = %q, want 10.0.0.2", u.Endpoints[0].IP)
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

	u := readDiscoveryUpdate(t, dw)
	if u.Name != "web" {
		t.Errorf("Name = %q, want web", u.Name)
	}
	if u.Endpoints != nil {
		t.Errorf("Endpoints = %v, want nil", u.Endpoints)
	}
	// Removal events don't carry labels (the Service is gone).
	// This documents the current behavior.
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
			if u.Name == "web" && len(u.Endpoints) > 0 && u.Endpoints[0].IP == "10.0.0.99" {
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
	if len(eps) != 1 || eps[0].IP != "api.example.com" {
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
			if u.Name == "api" && len(u.Endpoints) > 0 && u.Endpoints[0].IP == "10.0.0.5" {
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
	if len(webEps) != 1 || webEps[0].IP != "10.0.0.1" {
		t.Errorf("web endpoints = %v, want [{IP:10.0.0.1}]", webEps)
	}

	cdnEps, ok := state["cdn"]
	if !ok {
		t.Fatal("expected 'cdn' in initial state")
	}
	if len(cdnEps) != 1 || cdnEps[0].IP != "cdn.example.com" {
		t.Errorf("cdn endpoints = %v, want [{IP:cdn.example.com}]", cdnEps)
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
	if len(webEps) != 1 || webEps[0].IP != "10.0.0.1" {
		t.Errorf("web = %v, want [{IP:10.0.0.1 Port:8080}]", webEps)
	}
	if webEps[0].Port != 8080 {
		t.Errorf("web port = %d, want 8080", webEps[0].Port)
	}

	apiEps, ok := state["api"]
	if !ok {
		t.Fatal("expected 'api' in initial state")
	}
	if len(apiEps) != 1 || apiEps[0].IP != "10.0.0.2" {
		t.Errorf("api = %v, want [{IP:10.0.0.2 Port:9090}]", apiEps)
	}
	if apiEps[0].Port != 9090 {
		t.Errorf("api port = %d, want 9090", apiEps[0].Port)
	}
}
