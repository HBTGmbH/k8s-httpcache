package config

import (
	"testing"
)

func TestParseNamespacedService(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		defaultNS string
		wantNS    string
		wantSvc   string
		wantErr   bool
	}{
		{
			name:      "bare service uses default namespace",
			input:     "my-service",
			defaultNS: "default",
			wantNS:    "default",
			wantSvc:   "my-service",
		},
		{
			name:      "prefixed service",
			input:     "staging/my-service",
			defaultNS: "default",
			wantNS:    "staging",
			wantSvc:   "my-service",
		},
		{
			name:      "empty namespace prefix",
			input:     "/svc",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "empty service name",
			input:     "ns/",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "empty string",
			input:     "",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "multiple slashes uses first",
			input:     "ns/svc/extra",
			defaultNS: "default",
			wantNS:    "ns",
			wantSvc:   "svc/extra",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, svc, err := parseNamespacedService(tt.input, tt.defaultNS)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got ns=%q svc=%q", ns, svc)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ns != tt.wantNS {
				t.Errorf("namespace = %q, want %q", ns, tt.wantNS)
			}
			if svc != tt.wantSvc {
				t.Errorf("service = %q, want %q", svc, tt.wantSvc)
			}
		})
	}
}

func TestBackendFlagsSet(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantBS  BackendSpec
		wantErr bool
	}{
		{
			name:  "name and service",
			input: "api:my-svc",
			wantBS: BackendSpec{
				Name:        "api",
				ServiceName: "my-svc",
			},
		},
		{
			name:  "name service and port",
			input: "api:my-svc:3000",
			wantBS: BackendSpec{
				Name:        "api",
				ServiceName: "my-svc",
				Port:        "3000",
			},
		},
		{
			name:  "name service and named port",
			input: "api:my-svc:http",
			wantBS: BackendSpec{
				Name:        "api",
				ServiceName: "my-svc",
				Port:        "http",
			},
		},
		{
			name:  "namespace prefix without port",
			input: "api:staging/my-svc",
			wantBS: BackendSpec{
				Name:        "api",
				ServiceName: "staging/my-svc",
			},
		},
		{
			name:  "namespace prefix with port",
			input: "api:staging/my-svc:8080",
			wantBS: BackendSpec{
				Name:        "api",
				ServiceName: "staging/my-svc",
				Port:        "8080",
			},
		},
		{
			name:    "missing service",
			input:   "api",
			wantErr: true,
		},
		{
			name:    "empty port",
			input:   "api:my-svc:",
			wantErr: true,
		},
		{
			name:    "port out of range",
			input:   "api:my-svc:99999",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bf backendFlags
			err := bf.Set(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", bf)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(bf) != 1 {
				t.Fatalf("expected 1 backend, got %d", len(bf))
			}
			got := bf[0]
			if got.Name != tt.wantBS.Name {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantBS.Name)
			}
			if got.ServiceName != tt.wantBS.ServiceName {
				t.Errorf("ServiceName = %q, want %q", got.ServiceName, tt.wantBS.ServiceName)
			}
			if got.Port != tt.wantBS.Port {
				t.Errorf("Port = %q, want %q", got.Port, tt.wantBS.Port)
			}
		})
	}
}
