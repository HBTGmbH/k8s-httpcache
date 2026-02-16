package watcher

import "testing"

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
