package watcher

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// syncBuffer is a goroutine-safe wrapper around bytes.Buffer for use
// with per-instance loggers in tests where multiple goroutines may
// write concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // test helper wraps bytes.Buffer
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// waitForWatch lets the fake clientset's informer finish establishing its
// watch after the initial cache sync.  The Kubernetes reflector has a small
// window between HasSynced (list done) and Watch (watch started) in which
// the fake clientset drops events that have no registered watcher.  In
// production this is not an issue because the real API server buffers
// events, but the fake clientset delivers only to currently registered
// watchers.  A brief pause after reading the initial state gives the
// informer goroutine time to call Watch and register with the tracker.
func waitForWatch() {
	time.Sleep(100 * time.Millisecond)
}

// readBackendChanges reads from the BackendWatcher's Changes channel with a timeout.
func readBackendChanges(t *testing.T, bw *BackendWatcher) []Endpoint {
	t.Helper()
	select {
	case eps := <-bw.Changes():
		return eps
	case <-time.After(60 * time.Second):
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
	t.Parallel()
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
	t.Parallel()
	buf, logger := captureLogs(t, slog.LevelWarn)

	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "")
	bw.log = logger

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 80 {
		t.Errorf("Port = %d, want 80 (default)", eps[0].Port)
	}

	output := buf.String()
	if !strings.Contains(output, "no port specified for ExternalName service, defaulting to 80") {
		t.Errorf("expected default port warning, got:\n%s", output)
	}
}

func TestBackendWatcherExternalNameNamedPort(t *testing.T) {
	t.Parallel()
	buf, logger := captureLogs(t, slog.LevelWarn)

	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "http")
	bw.log = logger

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Named ports cannot be resolved for ExternalName services (no EndpointSlice),
	// so the watcher should emit empty endpoints.
	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints for named port on ExternalName, got %d: %v", len(eps), eps)
	}

	output := buf.String()
	if !strings.Contains(output, "cannot resolve port for ExternalName service") {
		t.Errorf("expected port resolution error, got:\n%s", output)
	}
	if !strings.Contains(output, "backend has no ready endpoints") {
		t.Errorf("expected 'backend has no ready endpoints' warning, got:\n%s", output)
	}
}

func TestBackendWatcherClusterIPService(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ExternalName endpoint.
	waitForWatch()
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
	t.Parallel()
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
	waitForWatch()
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
	t.Parallel()
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ExternalName endpoint.
	waitForWatch()
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

func TestBackendWatcherDebugLogging(t *testing.T) {
	t.Parallel()
	buf, logger := captureLogs(t, slog.LevelDebug)

	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	bw.log = logger

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: one endpoint.
	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}

	// Update to a different hostname → triggers added + removed.
	buf.Reset()
	current := getService(t, ctx, clientset)
	current.Spec.ExternalName = "api-v2.example.com"
	_, err := clientset.CoreV1().Services("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	readBackendChanges(t, bw)

	output := buf.String()
	if !strings.Contains(output, "backend endpoint added") {
		t.Errorf("expected 'backend endpoint added' log, got:\n%s", output)
	}
	if !strings.Contains(output, "api-v2.example.com:8080") {
		t.Errorf("expected new endpoint address in log, got:\n%s", output)
	}
	if !strings.Contains(output, "backend endpoint removed") {
		t.Errorf("expected 'backend endpoint removed' log, got:\n%s", output)
	}
	if !strings.Contains(output, "api.example.com:8080") {
		t.Errorf("expected old endpoint address in log, got:\n%s", output)
	}
}

func TestBackendWatcherExternalNameUpdated(t *testing.T) {
	t.Parallel()
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial.
	waitForWatch()
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
	t.Parallel()
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
	t.Parallel()
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Consume initial state.
	waitForWatch()
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
	t.Parallel()
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ExternalName endpoint.
	waitForWatch()
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
	t.Parallel()
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
	t.Parallel()
	buf, logger := captureLogs(t, slog.LevelWarn)

	svc := makeService(corev1.ServiceTypeExternalName, "")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	bw.log = logger

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Empty externalName should be treated as no endpoints.
	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints for empty externalName, got %d: %v", len(eps), eps)
	}

	output := buf.String()
	if !strings.Contains(output, "ExternalName service has empty externalName") {
		t.Errorf("expected empty externalName warning, got:\n%s", output)
	}
	if !strings.Contains(output, "backend has no ready endpoints") {
		t.Errorf("expected 'backend has no ready endpoints' warning, got:\n%s", output)
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

// captureLogs creates a per-test logger at the given level and returns the
// logger and its backing buffer. The returned syncBuffer is goroutine-safe.
func captureLogs(_ *testing.T, level slog.Level) (*syncBuffer, *slog.Logger) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level}))

	return buf, logger
}

func TestBackendWatcherWarnServiceNotFound(t *testing.T) {
	t.Parallel()
	buf, logger := captureLogs(t, slog.LevelWarn)

	clientset := fake.NewClientset()
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	bw.log = logger

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// No Service exists → empty endpoints.
	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints, got %d", len(eps))
	}

	output := buf.String()
	if !strings.Contains(output, "backend Service not found") {
		t.Errorf("expected 'backend Service not found' warning, got:\n%s", output)
	}
	if !strings.Contains(output, "backend has no ready endpoints") {
		t.Errorf("expected 'backend has no ready endpoints' warning, got:\n%s", output)
	}
}

func TestBackendWatcherWarnEndpointsBecomeEmpty(t *testing.T) {
	t.Parallel()
	buf, logger := captureLogs(t, slog.LevelWarn)

	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	bw.log = logger

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: 1 endpoint — no empty-endpoint warning expected.
	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}

	output := buf.String()
	if strings.Contains(output, "backend has no ready endpoints") {
		t.Errorf("unexpected 'no ready endpoints' warning when endpoints exist:\n%s", output)
	}

	// Delete the Service → endpoints become empty.
	buf.Reset()
	err := clientset.CoreV1().Services("default").Delete(ctx, "svc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints after delete, got %d: %v", len(eps), eps)
	}

	output = buf.String()
	if !strings.Contains(output, "backend Service not found") {
		t.Errorf("expected 'backend Service not found' warning after delete, got:\n%s", output)
	}
	if !strings.Contains(output, "backend has no ready endpoints") {
		t.Errorf("expected 'backend has no ready endpoints' warning after delete, got:\n%s", output)
	}
}

func TestBackendWatcherWarnReappearsAndDisappears(t *testing.T) {
	t.Parallel()
	buf, logger := captureLogs(t, slog.LevelWarn)

	clientset := fake.NewClientset()
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	bw.log = logger

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: no Service → warns.
	waitForWatch()
	readBackendChanges(t, bw)

	output := buf.String()
	if count := strings.Count(output, "backend Service not found"); count != 1 {
		t.Errorf("expected exactly 1 'backend Service not found' warning, got %d:\n%s", count, output)
	}

	// Service appears → endpoints arrive, no more "not found" warnings.
	buf.Reset()
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	_, err := clientset.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Service: %v", err)
	}

	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}

	output = buf.String()
	if strings.Contains(output, "backend Service not found") {
		t.Errorf("unexpected 'Service not found' warning after Service appeared:\n%s", output)
	}

	// Service disappears again → should warn again (flag was reset).
	buf.Reset()
	err = clientset.CoreV1().Services("default").Delete(ctx, "svc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Service: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints, got %d", len(eps))
	}

	output = buf.String()
	if !strings.Contains(output, "backend Service not found") {
		t.Errorf("expected 'backend Service not found' warning after second disappearance, got:\n%s", output)
	}
	if !strings.Contains(output, "backend has no ready endpoints") {
		t.Errorf("expected 'backend has no ready endpoints' warning after second disappearance, got:\n%s", output)
	}
}

func TestBackendWatcher_Labels(t *testing.T) {
	t.Parallel()

	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	svc.Labels = map[string]string{"version": "v1", "tier": "backend"}
	clientset := fake.NewClientset(svc)

	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Read initial endpoints.
	_ = readBackendChanges(t, bw)

	// Labels() should return the Service labels.
	labels := bw.Labels()
	if labels == nil {
		t.Fatal("expected non-nil labels")
	}
	if labels["version"] != "v1" {
		t.Errorf("expected version=v1, got %q", labels["version"])
	}
	if labels["tier"] != "backend" {
		t.Errorf("expected tier=backend, got %q", labels["tier"])
	}

	// Mutating the returned map should not affect internal state.
	labels["version"] = "mutated"
	labels2 := bw.Labels()
	if labels2["version"] != "v1" {
		t.Errorf("Labels() returned shared map; expected v1 after mutation, got %q", labels2["version"])
	}

	waitForWatch()

	// Update Service labels — should trigger a resend even though endpoints haven't changed.
	svc2 := getService(t, ctx, clientset)
	svc2.Labels = map[string]string{"version": "v2", "tier": "backend"}
	_, err := clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Should receive a resend with the same endpoints.
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 || eps[0].IP != "api.example.com" {
		t.Errorf("unexpected endpoints after label change: %v", eps)
	}

	// Labels should now reflect the update.
	labels3 := bw.Labels()
	if labels3["version"] != "v2" {
		t.Errorf("expected version=v2 after update, got %q", labels3["version"])
	}
}

func TestBackendWatcher_LabelsNilToPopulated(t *testing.T) {
	t.Parallel()

	// Service starts with no labels.
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	clientset := fake.NewClientset(svc)

	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	_ = readBackendChanges(t, bw)

	// Labels() should return nil (service has no labels).
	if labels := bw.Labels(); labels != nil {
		t.Fatalf("expected nil labels initially, got %v", labels)
	}

	waitForWatch()

	// Add labels to the service.
	svc2 := getService(t, ctx, clientset)
	svc2.Labels = map[string]string{"tier": "backend"}
	_, err := clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Should trigger a resend.
	_ = readBackendChanges(t, bw)

	labels := bw.Labels()
	if labels == nil {
		t.Fatal("expected non-nil labels after adding labels")
	}
	if labels["tier"] != "backend" {
		t.Errorf("expected tier=backend, got %q", labels["tier"])
	}
}

func TestBackendWatcher_LabelsPopulatedToNil(t *testing.T) {
	t.Parallel()

	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	svc.Labels = map[string]string{"version": "v1"}
	clientset := fake.NewClientset(svc)

	bw := NewBackendWatcher(clientset, "default", "svc", "8080")
	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	_ = readBackendChanges(t, bw)

	if labels := bw.Labels(); labels["version"] != "v1" {
		t.Fatalf("expected version=v1, got %v", labels)
	}

	waitForWatch()

	// Remove all labels from the service.
	svc2 := getService(t, ctx, clientset)
	svc2.Labels = nil
	_, err := clientset.CoreV1().Services("default").Update(ctx, svc2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Service: %v", err)
	}

	// Should trigger a resend.
	_ = readBackendChanges(t, bw)

	labels := bw.Labels()
	if len(labels) != 0 {
		t.Errorf("expected empty labels after removing all, got %v", labels)
	}
}

func TestBackendWatcher_LabelsOnLateAppearingService(t *testing.T) {
	t.Parallel()

	// No service at startup.
	clientset := fake.NewClientset()
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initially no Service → empty endpoints, nil labels.
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints initially, got %d", len(eps))
	}
	if labels := bw.Labels(); labels != nil {
		t.Fatalf("expected nil labels for missing service, got %v", labels)
	}

	waitForWatch()

	// Service appears with labels.
	svc := makeService(corev1.ServiceTypeExternalName, "api.example.com")
	svc.Labels = map[string]string{"env": "prod", "tier": "api"}
	_, err := clientset.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Service: %v", err)
	}

	// Read until we get actual endpoints (may see a nil resend first).
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ep := <-bw.Changes():
			if len(ep) == 1 && ep[0].IP == "api.example.com" {
				goto gotEndpoints
			}
		case <-deadline:
			t.Fatal("timeout waiting for endpoints from late-appearing service")
		}
	}
gotEndpoints:

	labels := bw.Labels()
	if labels == nil {
		t.Fatal("expected non-nil labels from late-appearing service")
	}
	if labels["env"] != "prod" {
		t.Errorf("expected env=prod, got %q", labels["env"])
	}
	if labels["tier"] != "api" {
		t.Errorf("expected tier=api, got %q", labels["tier"])
	}
}

func TestBackendWatcherClusterIPNoEndpointSlice(t *testing.T) {
	t.Parallel()
	// ClusterIP Service exists at startup, but no EndpointSlice yet.
	svc := makeService(corev1.ServiceTypeClusterIP, "")
	clientset := fake.NewClientset(svc)
	bw := NewBackendWatcher(clientset, "default", "svc", "")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ClusterIP but no EndpointSlice → empty endpoints.
	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints initially (no EndpointSlice), got %d: %v", len(eps), eps)
	}

	// Now create an EndpointSlice for the service.
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
	_, err := clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after EndpointSlice appears, got %d", len(eps))
	}
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", eps[0].IP)
	}
	if eps[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080", eps[0].Port)
	}
}

func TestBackendWatcherNumericPortOverrideClusterIP(t *testing.T) {
	t.Parallel()
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
			{Name: new("metrics"), Port: new(int32(9090))},
		},
	)

	clientset := fake.NewClientset(svc, slice)
	// Numeric port override "3000" — should use 3000 regardless of slice ports.
	bw := NewBackendWatcher(clientset, "default", "svc", "3000")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 3000 {
		t.Errorf("Port = %d, want 3000 (numeric port override)", eps[0].Port)
	}
}

func TestBackendWatcherNamedPortOverrideClusterIP(t *testing.T) {
	t.Parallel()
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
			{Name: new("metrics"), Port: new(int32(9090))},
		},
	)

	clientset := fake.NewClientset(svc, slice)
	// Named port override "metrics" — should resolve to 9090.
	bw := NewBackendWatcher(clientset, "default", "svc", "metrics")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	eps := readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 9090 {
		t.Errorf("Port = %d, want 9090 (named port 'metrics')", eps[0].Port)
	}
}

func TestBackendWatcherServiceDeletedRecreatedDifferentType(t *testing.T) {
	t.Parallel()
	// Start with a ClusterIP service + EndpointSlice.
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
	bw := NewBackendWatcher(clientset, "default", "svc", "8080")

	ctx := t.Context()
	go func() { _ = bw.Run(ctx) }()

	// Initial: ClusterIP endpoint.
	waitForWatch()
	eps := readBackendChanges(t, bw)
	if len(eps) != 1 || eps[0].IP != "10.0.0.1" {
		t.Fatalf("expected ClusterIP endpoint 10.0.0.1, got %v", eps)
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

	// Recreate as ExternalName.
	svc2 := makeService(corev1.ServiceTypeExternalName, "cdn.example.com")
	_, err = clientset.CoreV1().Services("default").Create(ctx, svc2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("re-creating Service as ExternalName: %v", err)
	}

	eps = readBackendChanges(t, bw)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after recreate as ExternalName, got %d", len(eps))
	}
	if eps[0].IP != "cdn.example.com" {
		t.Errorf("IP = %q, want cdn.example.com", eps[0].IP)
	}
	if eps[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080", eps[0].Port)
	}
	if eps[0].Name != "external" {
		t.Errorf("Name = %q, want external", eps[0].Name)
	}
}
