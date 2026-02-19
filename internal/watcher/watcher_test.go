package watcher

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
)

func ptr[T any](v T) *T { return &v }

func TestResolvePort(t *testing.T) {
	tests := []struct {
		name     string
		ports    []discoveryv1.EndpointPort
		override string
		want     int32
	}{
		{
			name:     "numeric override ignores port list",
			ports:    []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(int32(80))}},
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
				{Name: ptr("metrics"), Port: ptr(int32(9090))},
				{Name: ptr("http"), Port: ptr(int32(80))},
			},
			override: "http",
			want:     80,
		},
		{
			name: "named override no match returns zero",
			ports: []discoveryv1.EndpointPort{
				{Name: ptr("http"), Port: ptr(int32(80))},
			},
			override: "grpc",
			want:     0,
		},
		{
			name: "named override skips port with nil name",
			ports: []discoveryv1.EndpointPort{
				{Name: nil, Port: ptr(int32(80))},
				{Name: ptr("http"), Port: ptr(int32(8080))},
			},
			override: "http",
			want:     8080,
		},
		{
			name: "named override skips port with nil port value",
			ports: []discoveryv1.EndpointPort{
				{Name: ptr("http"), Port: nil},
			},
			override: "http",
			want:     0,
		},
		{
			name: "empty override uses first port",
			ports: []discoveryv1.EndpointPort{
				{Name: ptr("http"), Port: ptr(int32(80))},
				{Name: ptr("metrics"), Port: ptr(int32(9090))},
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
				{Name: ptr("http"), Port: nil},
			},
			override: "",
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePort(tt.ports, tt.override)
			if got != tt.want {
				t.Errorf("resolvePort(%v, %q) = %d, want %d", tt.ports, tt.override, got, tt.want)
			}
		})
	}
}

func TestFrontendsEqual(t *testing.T) {
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
			got := endpointsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("endpointsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDiffEndpoints(t *testing.T) {
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
			added, removed := diffEndpoints(tt.old, tt.new)
			if !endpointsEqual(added, tt.wantAdded) {
				t.Errorf("added = %v, want %v", added, tt.wantAdded)
			}
			if !endpointsEqual(removed, tt.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
		})
	}
}

func TestDebugLogging(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := &Watcher{
		namespace:   "ns",
		serviceName: "svc",
		synced:      true,
		previous: []Endpoint{
			{IP: "10.0.0.1", Port: 80, Name: "pod-a"},
		},
		ch: make(chan []Endpoint, 1),
	}

	// Simulate what sync() does for the diff logging path.
	endpoints := []Endpoint{
		{IP: "10.0.0.2", Port: 80, Name: "pod-b"},
	}

	added, removed := diffEndpoints(w.previous, endpoints)
	for _, ep := range added {
		slog.Debug("endpoint added", "namespace", w.namespace, "service", w.serviceName,
			"pod", ep.Name, "addr", fmt.Sprintf("%s:%d", ep.IP, ep.Port))
	}
	for _, ep := range removed {
		slog.Debug("endpoint removed", "namespace", w.namespace, "service", w.serviceName,
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
