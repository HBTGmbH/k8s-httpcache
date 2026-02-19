package watcher

import (
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
