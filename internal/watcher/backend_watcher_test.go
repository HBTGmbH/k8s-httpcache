package watcher

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// readBackendChanges reads from the BackendWatcher's Changes channel with a timeout.
func readBackendChanges(t *testing.T, bw *BackendWatcher) []Endpoint {
	t.Helper()
	select {
	case eps := <-bw.Changes():
		return eps
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for backend endpoint change")
		return nil
	}
}

// assertNoBackendChanges verifies no message arrives within the timeout.
func assertNoBackendChanges(t *testing.T, bw *BackendWatcher, timeout time.Duration) {
	t.Helper()
	select {
	case eps := <-bw.Changes():
		t.Fatalf("unexpected backend change received: %v", eps)
	case <-time.After(timeout):
		// OK — no change
	}
}

func makeService(serviceType corev1.ServiceType, externalName string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type: serviceType,
		},
	}
	if externalName != "" {
		svc.Spec.ExternalName = externalName
	}
	return svc
}

// getService fetches the latest Service from the fake clientset (returns a deep
// copy, safe to mutate without affecting the informer's cache).
func getService(t *testing.T, ctx context.Context, clientset *fake.Clientset) *corev1.Service {
	t.Helper()
	svc, err := clientset.CoreV1().Services("default").Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Service default/svc: %v", err)
	}
	return svc
}

func TestBackendWatcherExternalNameService(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].IP != "api.example.com" {
		t.Errorf("IP = %q, want api.example.com", eps[0].IP)
	}
	if eps[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080", eps[0].Port)
	}
	if eps[0].Name != "external" {
		t.Errorf("Name = %q, want external", eps[0].Name)
	}
}

func TestBackendWatcherExternalNameDefaultPort(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 80 {
		t.Errorf("Port = %d, want 80 (default)", eps[0].Port)
	}
}

func TestBackendWatcherExternalNameNamedPort(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "http")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Named ports cannot be resolved for ExternalName services (no EndpointSlice),
	// so the watcher should emit empty endpoints.
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints for named port on ExternalName, got %d: %v", len(eps), eps)
	}
}

func TestBackendWatcherClusterIPService(t *testing.T) {
	svc := makeService(corev1.ServiceTypeClusterIP, "")
	slice := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-a"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)

	clientset := fake.NewClientset(svc, slice)
	bw := NewBackendWatcher(clientset, "default", "svc", "")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", eps[0].IP)
	}
	if eps[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080", eps[0].Port)
	}
	if eps[0].Name != "pod-a" {
		t.Errorf("Name = %q, want pod-a", eps[0].Name)
	}
}

func TestBackendWatcherServiceNotExists(t *testing.T) {
	clientset := fake.NewClientset()
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints, got %d: %v", len(eps), eps)
	}
}

func TestBackendWatcherServiceAppearsLate(t *testing.T) {
	clientset := fake.NewClientset()
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initially no Service → empty endpoints.
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints initially, got %d: %v", len(eps), eps)
	}

	// Service appears after startup.
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	_, err := clientset.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after late Service creation, got %d", len(eps))
	}
	if eps[0].IP != "api.example.com" {
		t.Errorf("IP = %q, want api.example.com", eps[0].IP)
	}
	if eps[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080", eps[0].Port)
	}
}

func TestBackendWatcherExternalNameToClusterIP(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ExternalName endpoint.
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 || eps[0].IP != "api.example.com" {
		t.Fatalf("expected ExternalName endpoint, got %v", eps)
	}

	// Transition to ClusterIP via Get-then-Update to avoid shared-pointer issues.
	current := getService(t, ctx, clientset)
	current.Spec.Type = corev1.ServiceTypeClusterIP
	current.Spec.ExternalName = ""
	current.Spec.ClusterIP = "10.96.0.1"
	_, err := clientset.CoreV1().Services("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Create an EndpointSlice for the ClusterIP service.
	slice := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.5"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-x"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	// Should eventually get the ClusterIP endpoints.
	eps = readBackendChanges(t, bw)
	if len(eps) != 1 || eps[0].IP != "10.0.0.5" {
		t.Fatalf("expected ClusterIP endpoint 10.0.0.5, got %v", eps)
	}
}

func TestBackendWatcherClusterIPToExternalName(t *testing.T) {
	svc := makeService(corev1.ServiceTypeClusterIP, "")
	svc.Spec.ClusterIP = "10.96.0.1"
	slice := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-a"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)

	clientset := fake.NewClientset(svc, slice)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ClusterIP endpoint.
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 || eps[0].IP != "10.0.0.1" {
		t.Fatalf("expected ClusterIP endpoint, got %v", eps)
	}

	// Transition to ExternalName via Get-then-Update.
	current := getService(t, ctx, clientset)
	current.Spec.Type = corev1.ServiceTypeExternalName
	current.Spec.ExternalName = "cdn.example.com"
	current.Spec.ClusterIP = ""
	_, err := clientset.CoreV1().Services("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].IP != "cdn.example.com" {
		t.Errorf("IP = %q, want cdn.example.com", eps[0].IP)
	}
	if eps[0].Name != "external" {
		t.Errorf("Name = %q, want external", eps[0].Name)
	}
}

func TestBackendWatcherServiceDeleted(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ExternalName endpoint.
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}

	// Delete the service.
	err := clientset.CoreV1().Services("default").Delete(ctx, "svc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints after delete, got %d: %v", len(eps), eps)
	}
}

func TestBackendWatcherExternalNameUpdated(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial.
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 || eps[0].IP != "api.example.com" {
		t.Fatalf("expected api.example.com, got %v", eps)
	}

	// Update hostname via Get-then-Update.
	current := getService(t, ctx, clientset)
	current.Spec.ExternalName = "api-v2.example.com"
	_, err := clientset.CoreV1().Services("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].IP != "api-v2.example.com" {
		t.Errorf("IP = %q, want api-v2.example.com", eps[0].IP)
	}
}

func TestBackendWatcherStopsOnContextCancel(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- bw.Run(ctx)
	}()

	// Let Run start and deliver initial state.
	readBackendChanges(t, bw)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancel")
	}
}

func TestBackendWatcherDeduplicatesUnchanged(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Consume initial state.
	readBackendChanges(t, bw)

	// Update an unrelated field (annotation) via Get-then-Update — endpoints stay the same.
	current := getService(t, ctx, clientset)
	current.Annotations = map[string]string{"unrelated": "change"}
	_, err := clientset.CoreV1().Services("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Should NOT deliver a duplicate change.
	assertNoBackendChanges(t, bw, 500*time.Millisecond)
}

func TestBackendWatcherExternalNameDeletedAndRecreated(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ExternalName endpoint.
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 || eps[0].IP != "api.example.com" {
		t.Fatalf("expected api.example.com, got %v", eps)
	}

	// Delete the service.
	err := clientset.CoreV1().Services("default").Delete(ctx, "svc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints after delete, got %d: %v", len(eps), eps)
	}

	// Re-create with a different hostname.
	svc2 := makeService(corev1.ServiceTypeExternalName, "api-v2.example.com")
	_, err = clientset.CoreV1().Services("default").Create(ctx, svc2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("re-creating Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after re-create, got %d", len(eps))
	}
	if eps[0].IP != "api-v2.example.com" {
		t.Errorf("IP = %q, want api-v2.example.com", eps[0].IP)
	}
}

func TestBackendWatcherClusterIPAppearsLate(t *testing.T) {
	clientset := fake.NewClientset()
	bw := NewBackendWatcher(clientset, "default", "svc", "")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initially no Service → empty endpoints.
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints initially, got %d: %v", len(eps), eps)
	}

	// ClusterIP Service appears.
	svc := makeService(corev1.ServiceTypeClusterIP, "")
	_, err := clientset.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Service: %v", err)
	}

	// Create an EndpointSlice for the service.
	slice := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-a"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(9090))},
		},
	)
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", eps[0].IP)
	}
	if eps[0].Port != 9090 {
		t.Errorf("Port = %d, want 9090", eps[0].Port)
	}
}

func TestBackendWatcherExternalNameEmptyHostname(t *testing.T) {
	svc := makeService(corev1.ServiceTypeExternalName, "")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Empty externalName should be treated as no endpoints.
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints for empty externalName, got %d: %v", len(eps), eps)
	}

	// Fix the hostname via Get-then-Update.
	current := getService(t, ctx, clientset)
	current.Spec.ExternalName = "api.example.com"
	_, err := clientset.CoreV1().Services("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after fix, got %d", len(eps))
	}
	if eps[0].IP != "api.example.com" {
		t.Errorf("IP = %q, want api.example.com", eps[0].IP)
	}
}
