package watcher

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolvePort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ports    []discoveryv1.EndpointPort
		override string
		want     int32
	}{
		{
			name:     "numeric override ignores port list",
			ports:    []discoveryv1.EndpointPort{{Name: new("http"), Port: new(int32(80))}},
			override: "3000",
			want:     3000,
		},
		{
			name:     "numeric override with empty port list",
			ports:    nil,
			override: "8080",
			want:     8080,
		},
		{
			name: "named override matches port",
			ports: []discoveryv1.EndpointPort{
				{Name: new("metrics"), Port: new(int32(9090))},
				{Name: new("http"), Port: new(int32(80))},
			},
			override: "http",
			want:     80,
		},
		{
			name: "named override no match returns zero",
			ports: []discoveryv1.EndpointPort{
				{Name: new("http"), Port: new(int32(80))},
			},
			override: "grpc",
			want:     0,
		},
		{
			name: "named override skips port with nil name",
			ports: []discoveryv1.EndpointPort{
				{Name: nil, Port: new(int32(80))},
				{Name: new("http"), Port: new(int32(8080))},
			},
			override: "http",
			want:     8080,
		},
		{
			name: "named override skips port with nil port value",
			ports: []discoveryv1.EndpointPort{
				{Name: new("http"), Port: nil},
			},
			override: "http",
			want:     0,
		},
		{
			name: "empty override uses first port",
			ports: []discoveryv1.EndpointPort{
				{Name: new("http"), Port: new(int32(80))},
				{Name: new("metrics"), Port: new(int32(9090))},
			},
			override: "",
			want:     80,
		},
		{
			name:     "empty override with empty port list returns zero",
			ports:    nil,
			override: "",
			want:     0,
		},
		{
			name: "empty override with nil first port value returns zero",
			ports: []discoveryv1.EndpointPort{
				{Name: new("http"), Port: nil},
			},
			override: "",
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolvePort(tt.ports, tt.override)
			if got != tt.want {
				t.Errorf("resolvePort(%v, %q) = %d, want %d", tt.ports, tt.override, got, tt.want)
			}
		})
	}
}

func TestFrontendsEqual(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b []Frontend
		want bool
	}{
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "both empty",
			a:    []Frontend{},
			b:    []Frontend{},
			want: true,
		},
		{
			name: "nil vs empty",
			a:    nil,
			b:    []Frontend{},
			want: true,
		},
		{
			name: "equal single",
			a:    []Frontend{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			b:    []Frontend{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			want: true,
		},
		{
			name: "different length",
			a:    []Frontend{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			b:    []Frontend{},
			want: false,
		},
		{
			name: "different IP",
			a:    []Frontend{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			b:    []Frontend{{IP: "10.0.0.2", Port: 80, Name: "a"}},
			want: false,
		},
		{
			name: "different port",
			a:    []Frontend{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			b:    []Frontend{{IP: "10.0.0.1", Port: 8080, Name: "a"}},
			want: false,
		},
		{
			name: "different name",
			a:    []Frontend{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			b:    []Frontend{{IP: "10.0.0.1", Port: 80, Name: "b"}},
			want: false,
		},
		{
			name: "multiple equal",
			a: []Frontend{
				{IP: "10.0.0.1", Port: 80, Name: "a"},
				{IP: "10.0.0.2", Port: 80, Name: "b"},
			},
			b: []Frontend{
				{IP: "10.0.0.1", Port: 80, Name: "a"},
				{IP: "10.0.0.2", Port: 80, Name: "b"},
			},
			want: true,
		},
		{
			name: "same elements different order",
			a: []Frontend{
				{IP: "10.0.0.1", Port: 80, Name: "a"},
				{IP: "10.0.0.2", Port: 80, Name: "b"},
			},
			b: []Frontend{
				{IP: "10.0.0.2", Port: 80, Name: "b"},
				{IP: "10.0.0.1", Port: 80, Name: "a"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := EndpointsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("EndpointsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDiffEndpoints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		old, new    []Endpoint
		wantAdded   []Endpoint
		wantRemoved []Endpoint
	}{
		{
			name:        "no change",
			old:         []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			new:         []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			wantAdded:   nil,
			wantRemoved: nil,
		},
		{
			name:        "add one",
			old:         []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			new:         []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}, {IP: "10.0.0.2", Port: 80, Name: "b"}},
			wantAdded:   []Endpoint{{IP: "10.0.0.2", Port: 80, Name: "b"}},
			wantRemoved: nil,
		},
		{
			name:        "remove one",
			old:         []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}, {IP: "10.0.0.2", Port: 80, Name: "b"}},
			new:         []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			wantAdded:   nil,
			wantRemoved: []Endpoint{{IP: "10.0.0.2", Port: 80, Name: "b"}},
		},
		{
			name:        "replace",
			old:         []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}},
			new:         []Endpoint{{IP: "10.0.0.2", Port: 80, Name: "b"}},
			wantAdded:   []Endpoint{{IP: "10.0.0.2", Port: 80, Name: "b"}},
			wantRemoved: []Endpoint{{IP: "10.0.0.1", Port: 80, Name: "a"}},
		},
		{
			name:        "both nil",
			old:         nil,
			new:         nil,
			wantAdded:   nil,
			wantRemoved: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			added, removed := diffEndpoints(tt.old, tt.new)
			if !EndpointsEqual(added, tt.wantAdded) {
				t.Errorf("added = %v, want %v", added, tt.wantAdded)
			}
			if !EndpointsEqual(removed, tt.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
		})
	}
}

func TestDebugLogging(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	w := &Watcher{
		namespace:   "ns",
		serviceName: "svc",
		log:         logger,
		previous: []Endpoint{
			{IP: "10.0.0.1", Port: 80, Name: "pod-a"},
		},
	}

	// Simulate what sync() does for the diff logging path.
	endpoints := []Endpoint{
		{IP: "10.0.0.2", Port: 80, Name: "pod-b"},
	}

	added, removed := diffEndpoints(w.previous, endpoints)
	for _, ep := range added {
		w.log.Debug("endpoint added", "namespace", w.namespace, "service", w.serviceName,
			"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
	}
	for _, ep := range removed {
		w.log.Debug("endpoint removed", "namespace", w.namespace, "service", w.serviceName,
			"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
	}

	output := buf.String()
	if !strings.Contains(output, "endpoint added") {
		t.Errorf("expected 'endpoint added' log line, got: %s", output)
	}
	if !strings.Contains(output, "pod-b") {
		t.Errorf("expected pod-b in log, got: %s", output)
	}
	if !strings.Contains(output, "10.0.0.2:80") {
		t.Errorf("expected address in log, got: %s", output)
	}
	if !strings.Contains(output, "endpoint removed") {
		t.Errorf("expected 'endpoint removed' log line, got: %s", output)
	}
	if !strings.Contains(output, "pod-a") {
		t.Errorf("expected pod-a in log, got: %s", output)
	}
}

func TestNewAndChanges(t *testing.T) {
	t.Parallel()
	w := New(nil, "test-ns", "test-svc", "8080")

	if w.namespace != "test-ns" {
		t.Errorf("namespace = %q, want test-ns", w.namespace)
	}
	if w.serviceName != "test-svc" {
		t.Errorf("serviceName = %q, want test-svc", w.serviceName)
	}
	if w.portOverride != "8080" {
		t.Errorf("portOverride = %q, want 8080", w.portOverride)
	}
	if w.ch == nil {
		t.Error("channel should not be nil")
	}

	// Changes() should return the same channel.
	ch := w.Changes()
	if ch == nil {
		t.Error("Changes() returned nil")
	}
}

func TestDebugLoggingDisabled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	w := &Watcher{
		namespace:   "ns",
		serviceName: "svc",
		log:         logger,
		previous: []Endpoint{
			{IP: "10.0.0.1", Port: 80, Name: "pod-a"},
		},
	}

	endpoints := []Endpoint{
		{IP: "10.0.0.2", Port: 80, Name: "pod-b"},
	}

	added, removed := diffEndpoints(w.previous, endpoints)
	for _, ep := range added {
		w.log.Debug("endpoint added", "namespace", w.namespace, "service", w.serviceName,
			"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
	}
	for _, ep := range removed {
		w.log.Debug("endpoint removed", "namespace", w.namespace, "service", w.serviceName,
			"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
	}

	output := buf.String()
	if strings.Contains(output, "level=DEBUG") {
		t.Errorf("expected no debug log lines when level is Info, got: %s", output)
	}
}

// --- Run() integration tests using fake clientset ---

// makeEndpointSlice builds a discoveryv1.EndpointSlice for testing.
func makeEndpointSlice(name string, addressType discoveryv1.AddressType, endpoints []discoveryv1.Endpoint, ports []discoveryv1.EndpointPort) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "svc",
			},
		},
		AddressType: addressType,
		Endpoints:   endpoints,
		Ports:       ports,
	}
}

// readChanges reads from the watcher's Changes channel with a timeout.
func readChanges(t *testing.T, w *Watcher) []Endpoint {
	t.Helper()
	select {
	case eps := <-w.Changes():
		return eps
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for endpoint change")

		return nil
	}
}

// assertNoChanges verifies no message arrives within the timeout.
func assertNoChanges(t *testing.T, w *Watcher, timeout time.Duration) {
	t.Helper()
	select {
	case eps := <-w.Changes():
		t.Fatalf("unexpected change received: %v", eps)
	case <-time.After(timeout):
		// OK — no change
	}
}

func TestRunDeliversInitialEndpoints(t *testing.T) {
	t.Parallel()
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

	clientset := fake.NewClientset(slice)
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
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

func TestRunDeliversEmptyInitialState(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints, got %d: %v", len(eps), eps)
	}
}

func TestRunDetectsAddedEndpointSlice(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume the initial empty state.
	readChanges(t, w)

	// Now create an EndpointSlice via the fake client.
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
			{Name: new("http"), Port: new(int32(9090))},
		},
	)
	_, err := clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	eps := readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].IP != "10.0.0.5" || eps[0].Port != 9090 {
		t.Errorf("endpoint = %+v, want IP=10.0.0.5 Port=9090", eps[0])
	}
}

func TestRunDetectsUpdatedEndpointSlice(t *testing.T) {
	t.Parallel()
	// Start empty, then create, then update — avoids timing issues between
	// the informer's initial list+watch and the pre-loaded objects.
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial empty state.
	readChanges(t, w)

	// Create the slice with one endpoint.
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

	eps := readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after create, got %d", len(eps))
	}

	// Fetch the current object to get the correct ResourceVersion.
	current, err := clientset.DiscoveryV1().EndpointSlices("default").Get(ctx, "svc-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting EndpointSlice: %v", err)
	}

	// Update the slice to add a second endpoint.
	current.Endpoints = append(current.Endpoints, discoveryv1.Endpoint{
		Addresses:  []string{"10.0.0.2"},
		Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		TargetRef:  &corev1.ObjectReference{Name: "pod-b"},
	})
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	eps = readChanges(t, w)
	if len(eps) != 2 {
		t.Fatalf("expected 2 endpoints after update, got %d", len(eps))
	}
}

func TestRunDetectsDeletedEndpointSlice(t *testing.T) {
	t.Parallel()
	// Start with no slices, then create one, then delete it.
	// This avoids timing issues between the informer's initial list+watch
	// and the subsequent delete.
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial empty state.
	initial := readChanges(t, w)
	if len(initial) != 0 {
		t.Fatalf("expected 0 initial endpoints, got %d", len(initial))
	}

	// Create a slice.
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

	added := readChanges(t, w)
	if len(added) != 1 {
		t.Fatalf("expected 1 endpoint after add, got %d", len(added))
	}

	// Delete the slice.
	err = clientset.DiscoveryV1().EndpointSlices("default").Delete(ctx, "svc-abc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting EndpointSlice: %v", err)
	}

	eps := readChanges(t, w)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints after delete, got %d: %v", len(eps), eps)
	}
}

func TestRunFiltersNonReadyEndpoints(t *testing.T) {
	t.Parallel()
	slice := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-ready"},
			},
			{
				Addresses:  []string{"10.0.0.2"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(false)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-not-ready"},
			},
			{
				Addresses:  []string{"10.0.0.3"},
				Conditions: discoveryv1.EndpointConditions{}, // Ready is nil
				TargetRef:  &corev1.ObjectReference{Name: "pod-nil-ready"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)

	clientset := fake.NewClientset(slice)
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 ready endpoint, got %d: %v", len(eps), eps)
	}
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("expected ready endpoint 10.0.0.1, got %s", eps[0].IP)
	}
}

func TestRunFiltersNonIPv4v6AddressTypes(t *testing.T) {
	t.Parallel()
	fqdnSlice := makeEndpointSlice("svc-fqdn",
		discoveryv1.AddressTypeFQDN,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"my-host.example.com"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)

	clientset := fake.NewClientset(fqdnSlice)
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
	if len(eps) != 0 {
		t.Fatalf("expected 0 endpoints for FQDN address type, got %d: %v", len(eps), eps)
	}
}

func TestRunPortOverrideName(t *testing.T) {
	t.Parallel()
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
			{Name: new("metrics"), Port: new(int32(9090))},
			{Name: new("http"), Port: new(int32(8080))},
		},
	)

	clientset := fake.NewClientset(slice)
	w := New(clientset, "default", "svc", "http")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080 (resolved from named port 'http')", eps[0].Port)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Let Run start and deliver initial state.
	readChanges(t, w)

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

func TestRunExtractsTopologyFields(t *testing.T) {
	t.Parallel()
	zone := "europe-west3-a"
	nodeName := "node-1"
	slice := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-a"},
				Zone:       &zone,
				NodeName:   &nodeName,
				Hints: &discoveryv1.EndpointHints{
					ForZones: []discoveryv1.ForZone{
						{Name: "europe-west3-a"},
						{Name: "europe-west3-b"},
					},
				},
			},
			{
				Addresses:  []string{"10.0.0.2"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-b"},
				// No zone, nodeName, or hints — fields should be zero-valued.
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)

	clientset := fake.NewClientset(slice)
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
	if len(eps) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(eps))
	}

	// Endpoints are sorted by IP.
	if eps[0].Zone != "europe-west3-a" {
		t.Errorf("eps[0].Zone = %q, want europe-west3-a", eps[0].Zone)
	}
	if eps[0].NodeName != "node-1" {
		t.Errorf("eps[0].NodeName = %q, want node-1", eps[0].NodeName)
	}
	if len(eps[0].ForZones) != 2 || eps[0].ForZones[0] != "europe-west3-a" || eps[0].ForZones[1] != "europe-west3-b" {
		t.Errorf("eps[0].ForZones = %v, want [europe-west3-a europe-west3-b]", eps[0].ForZones)
	}

	if eps[1].Zone != "" {
		t.Errorf("eps[1].Zone = %q, want empty", eps[1].Zone)
	}
	if eps[1].NodeName != "" {
		t.Errorf("eps[1].NodeName = %q, want empty", eps[1].NodeName)
	}
	if eps[1].ForZones != nil {
		t.Errorf("eps[1].ForZones = %v, want nil", eps[1].ForZones)
	}
}

func TestRunDeduplicatesUnchangedEndpoints(t *testing.T) {
	t.Parallel()
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

	clientset := fake.NewClientset(slice)
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial state.
	readChanges(t, w)

	// Update an unrelated field (annotation) — endpoints stay the same.
	slice.Annotations = map[string]string{"unrelated": "change"}
	_, err := clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, slice, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	// Should NOT deliver a duplicate change.
	assertNoChanges(t, w, 500*time.Millisecond)
}

func TestRunMultipleEndpointSlices(t *testing.T) {
	t.Parallel()
	slice1 := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.2"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-b"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)
	slice2 := makeEndpointSlice("svc-def",
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

	clientset := fake.NewClientset(slice1, slice2)
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
	if len(eps) != 2 {
		t.Fatalf("expected 2 endpoints from 2 slices, got %d: %v", len(eps), eps)
	}
	// Endpoints should be sorted by IP.
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("eps[0].IP = %q, want 10.0.0.1", eps[0].IP)
	}
	if eps[1].IP != "10.0.0.2" {
		t.Errorf("eps[1].IP = %q, want 10.0.0.2", eps[1].IP)
	}
}

func TestRunDeleteOneOfMultipleSlices(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial empty state.
	readChanges(t, w)

	// Create two slices.
	slice1 := makeEndpointSlice("svc-abc",
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
	_, err := clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice1, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating slice1: %v", err)
	}
	readChanges(t, w)

	slice2 := makeEndpointSlice("svc-def",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.2"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-b"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating slice2: %v", err)
	}

	eps := readChanges(t, w)
	if len(eps) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(eps))
	}

	// Delete the first slice — remaining slice's endpoints should still be delivered.
	err = clientset.DiscoveryV1().EndpointSlices("default").Delete(ctx, "svc-abc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting slice1: %v", err)
	}

	eps = readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after deleting one slice, got %d: %v", len(eps), eps)
	}
	if eps[0].IP != "10.0.0.2" {
		t.Errorf("IP = %q, want 10.0.0.2", eps[0].IP)
	}
}

func TestRunReadyStateTransition(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial empty state.
	readChanges(t, w)

	// Create a slice with a not-ready endpoint.
	notReady := false
	slice := makeEndpointSlice("svc-abc",
		discoveryv1.AddressTypeIPv4,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: &notReady},
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

	// Not-ready endpoint should not appear — no change from empty.
	assertNoChanges(t, w, 500*time.Millisecond)

	// Transition to Ready=true.
	current, err := clientset.DiscoveryV1().EndpointSlices("default").Get(ctx, "svc-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting EndpointSlice: %v", err)
	}
	ready := true
	current.Endpoints[0].Conditions.Ready = &ready
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	eps := readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after ready transition, got %d", len(eps))
	}
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", eps[0].IP)
	}
}

func TestRunPortChangeOnExistingSlice(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial empty state.
	readChanges(t, w)

	// Create a slice with port 8080.
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

	eps := readChanges(t, w)
	if len(eps) != 1 || eps[0].Port != 8080 {
		t.Fatalf("expected port 8080, got %v", eps)
	}

	// Change the port to 9090.
	current, err := clientset.DiscoveryV1().EndpointSlices("default").Get(ctx, "svc-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting EndpointSlice: %v", err)
	}
	current.Ports[0].Port = new(int32(9090))
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	eps = readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 9090 {
		t.Errorf("Port = %d, want 9090 after port change", eps[0].Port)
	}
}

func TestRunNamedPortDisappears(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := New(clientset, "default", "svc", "http")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial empty state.
	readChanges(t, w)

	// Create a slice with named port "http" on 8080.
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
	_, err := clientset.DiscoveryV1().EndpointSlices("default").Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	eps := readChanges(t, w)
	if len(eps) != 1 || eps[0].Port != 8080 {
		t.Fatalf("expected port 8080, got %v", eps)
	}

	// Remove the "http" port, leaving only "metrics".
	current, err := clientset.DiscoveryV1().EndpointSlices("default").Get(ctx, "svc-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting EndpointSlice: %v", err)
	}
	current.Ports = []discoveryv1.EndpointPort{
		{Name: new("metrics"), Port: new(int32(9090))},
	}
	_, err = clientset.DiscoveryV1().EndpointSlices("default").Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating EndpointSlice: %v", err)
	}

	eps = readChanges(t, w)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if eps[0].Port != 0 {
		t.Errorf("Port = %d, want 0 when named port disappears", eps[0].Port)
	}
}

func TestRunIPv4AndIPv6SlicesCoexist(t *testing.T) {
	t.Parallel()
	ipv4Slice := makeEndpointSlice("svc-ipv4",
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
	ipv6Slice := makeEndpointSlice("svc-ipv6",
		discoveryv1.AddressTypeIPv6,
		[]discoveryv1.Endpoint{
			{
				Addresses:  []string{"fd00::1"},
				Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
				TargetRef:  &corev1.ObjectReference{Name: "pod-a-v6"},
			},
		},
		[]discoveryv1.EndpointPort{
			{Name: new("http"), Port: new(int32(8080))},
		},
	)

	clientset := fake.NewClientset(ipv4Slice, ipv6Slice)
	w := New(clientset, "default", "svc", "")
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	eps := readChanges(t, w)
	if len(eps) != 2 {
		t.Fatalf("expected 2 endpoints (IPv4 + IPv6), got %d: %v", len(eps), eps)
	}

	// Endpoints are sorted by IP; "10.0.0.1" < "fd00::1".
	if eps[0].IP != "10.0.0.1" {
		t.Errorf("eps[0].IP = %q, want 10.0.0.1", eps[0].IP)
	}
	if eps[1].IP != "fd00::1" {
		t.Errorf("eps[1].IP = %q, want fd00::1", eps[1].IP)
	}
}
