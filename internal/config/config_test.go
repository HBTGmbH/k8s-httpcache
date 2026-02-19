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

func TestListenAddrFlagsSet(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantName string
		wantHost string
		wantPort int32
		wantRaw  string
		wantErr  bool
	}{
		{
			name:     "named with proto",
			input:    "http=:8080,HTTP",
			wantName: "http",
			wantHost: "",
			wantPort: 8080,
			wantRaw:  "http=:8080,HTTP",
		},
		{
			name:     "named without proto",
			input:    "purge=:8081",
			wantName: "purge",
			wantHost: "",
			wantPort: 8081,
			wantRaw:  "purge=:8081",
		},
		{
			name:     "unnamed",
			input:    ":9090,HTTP",
			wantName: "",
			wantHost: "",
			wantPort: 9090,
			wantRaw:  ":9090,HTTP",
		},
		{
			name:     "with bind IP all interfaces",
			input:    "http=0.0.0.0:8080,HTTP",
			wantName: "http",
			wantHost: "0.0.0.0",
			wantPort: 8080,
			wantRaw:  "http=0.0.0.0:8080,HTTP",
		},
		{
			name:     "with loopback bind IP",
			input:    "admin=127.0.0.1:8081",
			wantName: "admin",
			wantHost: "127.0.0.1",
			wantPort: 8081,
			wantRaw:  "admin=127.0.0.1:8081",
		},
		{
			name:     "multiple protos",
			input:    "http=:8080,HTTP,PROXY",
			wantName: "http",
			wantHost: "",
			wantPort: 8080,
			wantRaw:  "http=:8080,HTTP,PROXY",
		},
		{
			name:     "unnamed port only",
			input:    ":8080",
			wantName: "",
			wantHost: "",
			wantPort: 8080,
			wantRaw:  ":8080",
		},
		{
			name:     "unnamed with bind IP",
			input:    "0.0.0.0:8080",
			wantName: "",
			wantHost: "0.0.0.0",
			wantPort: 8080,
			wantRaw:  "0.0.0.0:8080",
		},
		{
			name:     "IPv6 loopback bind address",
			input:    "http=[::1]:8080",
			wantName: "http",
			wantHost: "::1",
			wantPort: 8080,
			wantRaw:  "http=[::1]:8080",
		},
		{
			name:    "empty name",
			input:   "=:8080,HTTP",
			wantErr: true,
		},
		{
			name:    "missing port",
			input:   "http=noport",
			wantErr: true,
		},
		{
			name:    "port out of range",
			input:   "http=:99999",
			wantErr: true,
		},
		{
			name:    "port zero",
			input:   "http=:0",
			wantErr: true,
		},
		{
			name:    "non-numeric port",
			input:   "http=:abc",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lf listenAddrFlags
			err := lf.Set(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", lf)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(lf) != 1 {
				t.Fatalf("expected 1 listen addr, got %d", len(lf))
			}
			got := lf[0]
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", got.Host, tt.wantHost)
			}
			if got.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", got.Port, tt.wantPort)
			}
			if got.Raw != tt.wantRaw {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.wantRaw)
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
			name:  "namespace prefix with named port",
			input: "api:staging/my-svc:http",
			wantBS: BackendSpec{
				Name:        "api",
				ServiceName: "staging/my-svc",
				Port:        "http",
			},
		},
		{
			name:    "missing service",
			input:   "api",
			wantErr: true,
		},
		{
			name:    "empty name",
			input:   ":my-svc",
			wantErr: true,
		},
		{
			name:    "empty service",
			input:   "api:",
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
		{
			name:    "port zero",
			input:   "api:my-svc:0",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
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
