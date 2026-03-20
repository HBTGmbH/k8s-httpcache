package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseNamespacedService(t *testing.T) {
	t.Parallel()
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
			name:      "multiple slashes rejected",
			input:     "ns/svc/extra",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "uppercase service rejected",
			input:     "MyService",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "uppercase namespace rejected",
			input:     "NS/svc",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "service starts with hyphen",
			input:     "-svc",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "service ends with hyphen",
			input:     "svc-",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "namespace starts with hyphen",
			input:     "-ns/svc",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "namespace ends with hyphen",
			input:     "ns-/svc",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "service contains underscore",
			input:     "my_svc",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "service contains dot",
			input:     "my.svc",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "service too long",
			input:     "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz01",
			defaultNS: "default",
			wantErr:   true,
		},
		{
			name:      "service at max length 63",
			input:     "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0",
			defaultNS: "default",
			wantNS:    "default",
			wantSvc:   "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0",
		},
		{
			name:      "single char service",
			input:     "a",
			defaultNS: "default",
			wantNS:    "default",
			wantSvc:   "a",
		},
		{
			name:      "numeric service",
			input:     "123",
			defaultNS: "default",
			wantNS:    "default",
			wantSvc:   "123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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

func TestListenAddrFlagsString(t *testing.T) {
	t.Parallel()
	var lf listenAddrFlags
	_ = lf.Set("http=:8080,HTTP")
	s := lf.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestBackendFlagsString(t *testing.T) {
	t.Parallel()
	var bf backendFlags
	_ = bf.Set("api:my-svc:3000")
	s := bf.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestListenAddrPortMinBoundary(t *testing.T) {
	t.Parallel()
	var lf listenAddrFlags
	err := lf.Set("http=:1")
	if err != nil {
		t.Fatalf("unexpected error for port 1: %v", err)
	}
	if lf[0].Port != 1 {
		t.Errorf("Port = %d, want 1", lf[0].Port)
	}
}

func TestListenAddrTrailingComma(t *testing.T) {
	t.Parallel()
	// Trailing comma (empty proto string after comma) should still parse the address.
	var lf listenAddrFlags
	err := lf.Set("http=:8080,")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lf[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080", lf[0].Port)
	}
	if lf[0].Raw != "http=:8080," {
		t.Errorf("Raw = %q, want %q", lf[0].Raw, "http=:8080,")
	}
}

func TestListenAddrMultipleEquals(t *testing.T) {
	t.Parallel()
	// Multiple '=' signs: only the first splits name from address.
	// "a=b=:8080" → name="a", rest="b=:8080", SplitHostPort("b=:8080") → host="b=", port=8080
	var lf listenAddrFlags
	err := lf.Set("a=b=:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lf[0].Name != "a" {
		t.Errorf("Name = %q, want a", lf[0].Name)
	}
	if lf[0].Host != "b=" {
		t.Errorf("Host = %q, want b=", lf[0].Host)
	}
	if lf[0].Port != 8080 {
		t.Errorf("Port = %d, want 8080", lf[0].Port)
	}
}

func TestListenAddrNegativePort(t *testing.T) {
	t.Parallel()
	var lf listenAddrFlags
	err := lf.Set("http=:-1")
	if err == nil {
		t.Fatal("expected error for negative port")
	}
}

func TestListenAddrPortBoundary(t *testing.T) {
	t.Parallel()
	var lf listenAddrFlags
	err := lf.Set("http=:65535")
	if err != nil {
		t.Fatalf("unexpected error for port 65535: %v", err)
	}
	if lf[0].Port != 65535 {
		t.Errorf("Port = %d, want 65535", lf[0].Port)
	}
}

func TestListenAddrPortOverBoundary(t *testing.T) {
	t.Parallel()
	var lf listenAddrFlags
	err := lf.Set("http=:65536")
	if err == nil {
		t.Fatal("expected error for port 65536")
	}
}

func TestListenAddrMultipleAccumulate(t *testing.T) {
	t.Parallel()
	var lf listenAddrFlags
	err := lf.Set("http=:8080,HTTP")
	if err != nil {
		t.Fatal(err)
	}
	err = lf.Set("purge=:8081")
	if err != nil {
		t.Fatal(err)
	}
	if len(lf) != 2 {
		t.Fatalf("expected 2 addrs, got %d", len(lf))
	}
	if lf[0].Name != "http" || lf[1].Name != "purge" {
		t.Errorf("names = [%q, %q], want [http, purge]", lf[0].Name, lf[1].Name)
	}
}

func TestBackendPortMinBoundary(t *testing.T) {
	t.Parallel()
	var bf backendFlags
	err := bf.Set("api:my-svc:1")
	if err != nil {
		t.Fatalf("unexpected error for port 1: %v", err)
	}
	if bf[0].Port != "1" {
		t.Errorf("Port = %q, want 1", bf[0].Port)
	}
}

func TestBackendNegativePort(t *testing.T) {
	t.Parallel()
	var bf backendFlags
	err := bf.Set("api:my-svc:-1")
	if err == nil {
		t.Fatal("expected error for negative port")
	}
}

func TestBackendPortBoundary(t *testing.T) {
	t.Parallel()
	var bf backendFlags
	err := bf.Set("api:my-svc:65535")
	if err != nil {
		t.Fatalf("unexpected error for port 65535: %v", err)
	}
	if bf[0].Port != "65535" {
		t.Errorf("Port = %q, want 65535", bf[0].Port)
	}
}

func TestBackendPortOverBoundary(t *testing.T) {
	t.Parallel()
	var bf backendFlags
	err := bf.Set("api:my-svc:65536")
	if err == nil {
		t.Fatal("expected error for port 65536")
	}
}

func TestBackendMultipleAccumulate(t *testing.T) {
	t.Parallel()
	var bf backendFlags
	err := bf.Set("api:api-svc:8080")
	if err != nil {
		t.Fatal(err)
	}
	err = bf.Set("web:web-svc:http")
	if err != nil {
		t.Fatal(err)
	}
	if len(bf) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(bf))
	}
	if bf[0].Name != "api" || bf[1].Name != "web" {
		t.Errorf("names = [%q, %q], want [api, web]", bf[0].Name, bf[1].Name)
	}
}

// --- Parse() tests ---

// makeTempVCL creates a temporary VCL template file and returns its path.
func makeTempVCL(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "vcl-*.vcl")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("vcl 4.0;")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })

	return f.Name()
}

func TestParseMissingServiceName(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err == nil {
		t.Fatal("expected error for missing --service-name")
	}
	if !strings.Contains(err.Error(), "--service-name is required") {
		t.Errorf("error = %q, want substring '--service-name is required'", err)
	}
}

func TestParseMissingNamespace(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--vcl-template=" + vcl,
	})
	if err == nil {
		t.Fatal("expected error for missing --namespace")
	}
	if !strings.Contains(err.Error(), "--namespace is required") {
		t.Errorf("error = %q, want substring '--namespace is required'", err)
	}
}

func TestParseMissingVCLTemplate(t *testing.T) {
	t.Parallel()
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
	})
	if err == nil {
		t.Fatal("expected error for missing --vcl-template")
	}
	if !strings.Contains(err.Error(), "--vcl-template is required") {
		t.Errorf("error = %q, want substring '--vcl-template is required'", err)
	}
}

func TestParseVCLTemplateNotExist(t *testing.T) {
	t.Parallel()
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=/nonexistent/path/to/file.vcl",
	})
	if err == nil {
		t.Fatal("expected error for non-existent vcl-template")
	}
	if !strings.Contains(err.Error(), "vcl-template file") {
		t.Errorf("error = %q, want substring 'vcl-template file'", err)
	}
}

func TestParseMinimalValid(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServiceName != "my-svc" {
		t.Errorf("ServiceName = %q, want my-svc", cfg.ServiceName)
	}
	if cfg.ServiceNamespace != "default" {
		t.Errorf("ServiceNamespace = %q, want default", cfg.ServiceNamespace)
	}
	if cfg.VCLTemplate != vcl {
		t.Errorf("VCLTemplate = %q, want %q", cfg.VCLTemplate, vcl)
	}
	// Default listen address should be applied.
	if len(cfg.ListenAddrs) != 1 {
		t.Fatalf("expected 1 default listen addr, got %d", len(cfg.ListenAddrs))
	}
	if cfg.ListenAddrs[0].Name != "http" || cfg.ListenAddrs[0].Port != 8080 {
		t.Errorf("default listen addr = %+v, want http:8080", cfg.ListenAddrs[0])
	}
	// Default broadcast target port should resolve to first listen addr.
	if cfg.BroadcastTargetPort != 8080 {
		t.Errorf("BroadcastTargetPort = %d, want 8080", cfg.BroadcastTargetPort)
	}
}

func TestParseServiceNameWithNamespace(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=staging/my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServiceNamespace != "staging" {
		t.Errorf("ServiceNamespace = %q, want staging", cfg.ServiceNamespace)
	}
	if cfg.ServiceName != "my-svc" {
		t.Errorf("ServiceName = %q, want my-svc", cfg.ServiceName)
	}
}

func TestParseInvalidServiceNamePrefix(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=/my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err == nil {
		t.Fatal("expected error for empty namespace prefix in --service-name")
	}
	if !strings.Contains(err.Error(), "--service-name") {
		t.Errorf("error = %q, want substring '--service-name'", err)
	}
}

func TestParseDuplicateListenAddrNames(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080",
		"--listen-addr=http=:9090",
	})
	if err == nil {
		t.Fatal("expected error for duplicate --listen-addr name")
	}
	if !strings.Contains(err.Error(), "duplicate --listen-addr name") {
		t.Errorf("error = %q, want substring 'duplicate --listen-addr name'", err)
	}
}

func TestParseUnnamedListenAddrsNoDuplicateError(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=:8080",
		"--listen-addr=:9090",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ListenAddrs) != 2 {
		t.Fatalf("expected 2 listen addrs, got %d", len(cfg.ListenAddrs))
	}
}

func TestParseBroadcastTargetListenAddr(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080,HTTP",
		"--listen-addr=purge=:8081",
		"--broadcast-target-listen-addr=purge",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BroadcastTargetPort != 8081 {
		t.Errorf("BroadcastTargetPort = %d, want 8081", cfg.BroadcastTargetPort)
	}
}

func TestParseBroadcastTargetListenAddrNotFound(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080",
		"--broadcast-target-listen-addr=nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for unknown --broadcast-target-listen-addr")
	}
	if !strings.Contains(err.Error(), "does not match any --listen-addr name") {
		t.Errorf("error = %q, want substring 'does not match any --listen-addr name'", err)
	}
}

func TestParseBroadcastDisabled(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-addr=none",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BroadcastAddr != "" {
		t.Errorf("BroadcastAddr = %q, want empty", cfg.BroadcastAddr)
	}
	// BroadcastTargetPort should remain 0 when broadcast is disabled.
	if cfg.BroadcastTargetPort != 0 {
		t.Errorf("BroadcastTargetPort = %d, want 0", cfg.BroadcastTargetPort)
	}
}

func TestParseBroadcastAddrEmptyValueIsError(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-addr=",
	})
	if err == nil {
		t.Fatal("expected error for --broadcast-addr= (empty value), got nil")
	}
}

func TestParseDrainEnabled(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--drain",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Drain {
		t.Error("Drain = false, want true")
	}
}

func TestParseMetricsAddrNone(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--metrics-addr=none",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MetricsAddr != "" {
		t.Errorf("MetricsAddr = %q, want empty", cfg.MetricsAddr)
	}
}

func TestParseDuplicateBackendNames(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc",
		"--backend=api:other-svc",
	})
	if err == nil {
		t.Fatal("expected error for duplicate --backend name")
	}
	if !strings.Contains(err.Error(), "duplicate --backend name") {
		t.Errorf("error = %q, want substring 'duplicate --backend name'", err)
	}
}

func TestParseBackendNamespaceResolution(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc",
		"--backend=web:staging/web-svc:http",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(cfg.Backends))
	}
	// First backend uses default namespace.
	if cfg.Backends[0].Namespace != "default" {
		t.Errorf("Backends[0].Namespace = %q, want default", cfg.Backends[0].Namespace)
	}
	if cfg.Backends[0].ServiceName != "api-svc" {
		t.Errorf("Backends[0].ServiceName = %q, want api-svc", cfg.Backends[0].ServiceName)
	}
	// Second backend uses explicit namespace.
	if cfg.Backends[1].Namespace != "staging" {
		t.Errorf("Backends[1].Namespace = %q, want staging", cfg.Backends[1].Namespace)
	}
	if cfg.Backends[1].ServiceName != "web-svc" {
		t.Errorf("Backends[1].ServiceName = %q, want web-svc", cfg.Backends[1].ServiceName)
	}
	if cfg.Backends[1].Port != "http" {
		t.Errorf("Backends[1].Port = %q, want http", cfg.Backends[1].Port)
	}
}

func TestParseBackendInvalidServiceRef(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:/svc",
	})
	if err == nil {
		t.Fatal("expected error for invalid backend service reference")
	}
	if !strings.Contains(err.Error(), "--backend") {
		t.Errorf("error = %q, want substring '--backend'", err)
	}
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VarnishdPath != "varnishd" {
		t.Errorf("VarnishdPath = %q, want varnishd", cfg.VarnishdPath)
	}
	if cfg.VarnishadmPath != "varnishadm" {
		t.Errorf("VarnishadmPath = %q, want varnishadm", cfg.VarnishadmPath)
	}
	if cfg.BroadcastAddr != ":8088" {
		t.Errorf("BroadcastAddr = %q, want :8088", cfg.BroadcastAddr)
	}
	if cfg.Debounce != 2_000_000_000 { // 2s
		t.Errorf("Debounce = %v, want 2s", cfg.Debounce)
	}
	if cfg.ShutdownTimeout != 30_000_000_000 { // 30s
		t.Errorf("ShutdownTimeout = %v, want 30s", cfg.ShutdownTimeout)
	}
	if cfg.VarnishstatPath != "varnishstat" {
		t.Errorf("VarnishstatPath = %q, want varnishstat", cfg.VarnishstatPath)
	}
	if cfg.MetricsAddr != ":9101" {
		t.Errorf("MetricsAddr = %q, want :9101", cfg.MetricsAddr)
	}
	if cfg.ValuesDirPollInterval != 5*time.Second {
		t.Errorf("ValuesDirPollInterval = %v, want 5s", cfg.ValuesDirPollInterval)
	}
	if cfg.Drain {
		t.Error("Drain = true, want false")
	}
	if cfg.DrainDelay != 15*time.Second {
		t.Errorf("DrainDelay = %v, want 15s", cfg.DrainDelay)
	}
	if cfg.DrainTimeout != 0 {
		t.Errorf("DrainTimeout = %v, want 0", cfg.DrainTimeout)
	}
	// Per-source debounce fields default to global values.
	if cfg.FrontendDebounce != cfg.Debounce {
		t.Errorf("FrontendDebounce = %v, want %v (same as Debounce)", cfg.FrontendDebounce, cfg.Debounce)
	}
	if cfg.FrontendDebounceMax != cfg.DebounceMax {
		t.Errorf("FrontendDebounceMax = %v, want %v (same as DebounceMax)", cfg.FrontendDebounceMax, cfg.DebounceMax)
	}
	if cfg.BackendDebounce != cfg.Debounce {
		t.Errorf("BackendDebounce = %v, want %v (same as Debounce)", cfg.BackendDebounce, cfg.Debounce)
	}
	if cfg.BackendDebounceMax != cfg.DebounceMax {
		t.Errorf("BackendDebounceMax = %v, want %v (same as DebounceMax)", cfg.BackendDebounceMax, cfg.DebounceMax)
	}
	if !slices.Equal(cfg.DebounceLatencyBuckets, DefaultDebounceLatencyBuckets) {
		t.Errorf("DebounceLatencyBuckets = %v, want %v", cfg.DebounceLatencyBuckets, DefaultDebounceLatencyBuckets)
	}
}

func TestParseDebounceLatencyBucketsCustom(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-latency-buckets=0.001,0.01,0.1,1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []float64{0.001, 0.01, 0.1, 1}
	if !slices.Equal(cfg.DebounceLatencyBuckets, want) {
		t.Errorf("DebounceLatencyBuckets = %v, want %v", cfg.DebounceLatencyBuckets, want)
	}
}

func TestParseDebounceLatencyBucketsInvalidValue(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-latency-buckets=0.1,abc",
	})
	if err == nil {
		t.Fatal("expected error for invalid bucket value")
	}
	if !strings.Contains(err.Error(), "invalid value") {
		t.Errorf("error = %q, want it to mention invalid value", err)
	}
}

func TestParseDebounceLatencyBucketsNegativeValue(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-latency-buckets=0.1,-0.5",
	})
	if err == nil {
		t.Fatal("expected error for negative bucket value")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error = %q, want it to mention positive", err)
	}
}

func TestParseDebounceLatencyBucketsZeroValue(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-latency-buckets=0",
	})
	if err == nil {
		t.Fatal("expected error for zero bucket value")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error = %q, want it to mention positive", err)
	}
}

func TestParseDebounceLatencyBucketsSorted(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-latency-buckets=1,0.1,0.5",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []float64{0.1, 0.5, 1}
	if !slices.Equal(cfg.DebounceLatencyBuckets, want) {
		t.Errorf("DebounceLatencyBuckets = %v, want %v", cfg.DebounceLatencyBuckets, want)
	}
}

func TestParseExtraVarnishdArgs(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--", "-p", "default_ttl=3600", "-p", "default_grace=3600",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"-p", "default_ttl=3600", "-p", "default_grace=3600"}
	if len(cfg.ExtraVarnishd) != len(want) {
		t.Fatalf("ExtraVarnishd = %v, want %v", cfg.ExtraVarnishd, want)
	}
	for i := range want {
		if cfg.ExtraVarnishd[i] != want[i] {
			t.Errorf("ExtraVarnishd[%d] = %q, want %q", i, cfg.ExtraVarnishd[i], want[i])
		}
	}
}

func TestParseCustomListenAddrs(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080,HTTP",
		"--listen-addr=purge=:8081",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ListenAddrs) != 2 {
		t.Fatalf("expected 2 listen addrs, got %d", len(cfg.ListenAddrs))
	}
	if cfg.ListenAddrs[0].Name != "http" || cfg.ListenAddrs[0].Port != 8080 {
		t.Errorf("ListenAddrs[0] = %+v, want http:8080", cfg.ListenAddrs[0])
	}
	if cfg.ListenAddrs[1].Name != "purge" || cfg.ListenAddrs[1].Port != 8081 {
		t.Errorf("ListenAddrs[1] = %+v, want purge:8081", cfg.ListenAddrs[1])
	}
	// Default broadcast target should be first listen addr.
	if cfg.BroadcastTargetPort != 8080 {
		t.Errorf("BroadcastTargetPort = %d, want 8080", cfg.BroadcastTargetPort)
	}
}

func TestParseVCLTemplateIsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + dir,
	})
	// os.Stat succeeds for directories, but this exercises the path where
	// the "file" exists. Parse() doesn't distinguish files from dirs —
	// the template parser in renderer.New() would fail later. So Parse()
	// should succeed here.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLTemplate != dir {
		t.Errorf("VCLTemplate = %q, want %q", cfg.VCLTemplate, dir)
	}
}

func TestParseVCLTemplateRelativePath(t *testing.T) {
	t.Parallel()
	// Create a temp file in the current working directory.
	f, err := os.CreateTemp(".", "vcl-test-*.vcl")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	relPath := filepath.Base(f.Name())
	t.Cleanup(func() { _ = os.Remove(relPath) })

	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + relPath,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLTemplate != relPath {
		t.Errorf("VCLTemplate = %q, want %q", cfg.VCLTemplate, relPath)
	}
}

func TestParseOverrideStringFlags(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--varnishd-path=/usr/sbin/varnishd",
		"--varnishadm-path=/usr/bin/varnishadm",
		"--varnishstat-path=/usr/bin/varnishstat",
		"--broadcast-addr=:9999",
		"--metrics-addr=:2112",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VarnishdPath != "/usr/sbin/varnishd" {
		t.Errorf("VarnishdPath = %q, want /usr/sbin/varnishd", cfg.VarnishdPath)
	}
	if cfg.VarnishadmPath != "/usr/bin/varnishadm" {
		t.Errorf("VarnishadmPath = %q, want /usr/bin/varnishadm", cfg.VarnishadmPath)
	}
	if cfg.VarnishstatPath != "/usr/bin/varnishstat" {
		t.Errorf("VarnishstatPath = %q, want /usr/bin/varnishstat", cfg.VarnishstatPath)
	}
	if cfg.BroadcastAddr != ":9999" {
		t.Errorf("BroadcastAddr = %q, want :9999", cfg.BroadcastAddr)
	}
	if cfg.MetricsAddr != ":2112" {
		t.Errorf("MetricsAddr = %q, want :2112", cfg.MetricsAddr)
	}
}

func TestParseVinylPathsExplicit(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vinyld-path=/opt/vinyl/bin/vinyld",
		"--vinyladm-path=/opt/vinyl/bin/vinyladm",
		"--vinylstat-path=/opt/vinyl/bin/vinylstat",
		"--vinylncsa-path=/opt/vinyl/bin/vinylncsa",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VarnishdPath != "/opt/vinyl/bin/vinyld" {
		t.Errorf("VarnishdPath = %q, want /opt/vinyl/bin/vinyld", cfg.VarnishdPath)
	}
	if cfg.VarnishadmPath != "/opt/vinyl/bin/vinyladm" {
		t.Errorf("VarnishadmPath = %q, want /opt/vinyl/bin/vinyladm", cfg.VarnishadmPath)
	}
	if cfg.VarnishstatPath != "/opt/vinyl/bin/vinylstat" {
		t.Errorf("VarnishstatPath = %q, want /opt/vinyl/bin/vinylstat", cfg.VarnishstatPath)
	}
	if cfg.VarnishncsaPath != "/opt/vinyl/bin/vinylncsa" {
		t.Errorf("VarnishncsaPath = %q, want /opt/vinyl/bin/vinylncsa", cfg.VarnishncsaPath)
	}
}

func TestParseVinylncsaEnabledAlias(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vinylncsa-enabled",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.VarnishncsaEnabled {
		t.Error("VarnishncsaEnabled = false, want true (via --vinylncsa-enabled alias)")
	}
}

func TestParseVinylPartialDefaultsFilledIn(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vinyld-path=/opt/vinyld",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VarnishdPath != "/opt/vinyld" {
		t.Errorf("VarnishdPath = %q, want /opt/vinyld", cfg.VarnishdPath)
	}
	if cfg.VarnishadmPath != "vinyladm" {
		t.Errorf("VarnishadmPath = %q, want vinyladm", cfg.VarnishadmPath)
	}
	if cfg.VarnishstatPath != "vinylstat" {
		t.Errorf("VarnishstatPath = %q, want vinylstat", cfg.VarnishstatPath)
	}
	if cfg.VarnishncsaPath != "vinylncsa" {
		t.Errorf("VarnishncsaPath = %q, want vinylncsa", cfg.VarnishncsaPath)
	}
}

func TestResolveBinaryPaths(t *testing.T) {
	t.Parallel()

	vinylFound := func(file string) (string, error) {
		if file == "vinyld" {
			return "/usr/bin/vinyld", nil
		}

		return "", errors.New("not found")
	}
	vinylNotFound := func(string) (string, error) {
		return "", errors.New("not found")
	}

	tests := []struct {
		name            string
		vinyld          string
		vinyladm        string
		vinylstat       string
		vinylncsa       string
		varnishExplicit bool   // simulates cmd.IsSet on any --varnish*-path flag
		initD           string // initial VarnishdPath (simulates --varnishd-path default or explicit)
		initAdm         string
		initStat        string
		initNcsa        string
		lookPathFn      func(string) (string, error)
		wantD           string
		wantAdm         string
		wantStat        string
		wantNcsa        string
	}{
		{
			name:       "auto-detect finds vinyl on PATH",
			lookPathFn: vinylFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "vinyld",
			wantAdm:    "vinyladm",
			wantStat:   "vinylstat",
			wantNcsa:   "vinylncsa",
		},
		{
			name:       "auto-detect fallback to varnish when vinyl not on PATH",
			lookPathFn: vinylNotFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "varnishd",
			wantAdm:    "varnishadm",
			wantStat:   "varnishstat",
			wantNcsa:   "varnishncsa",
		},
		{
			name:       "explicit vinyl flags override varnish defaults — all four",
			vinyld:     "/opt/vinyld",
			vinyladm:   "/opt/vinyladm",
			vinylstat:  "/opt/vinylstat",
			vinylncsa:  "/opt/vinylncsa",
			lookPathFn: vinylNotFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "/opt/vinyld",
			wantAdm:    "/opt/vinyladm",
			wantStat:   "/opt/vinylstat",
			wantNcsa:   "/opt/vinylncsa",
		},
		{
			name:       "partial vinyl — only vinyld-path set, others default to vinyl names",
			vinyld:     "/opt/vinyld",
			lookPathFn: vinylNotFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "/opt/vinyld",
			wantAdm:    "vinyladm",
			wantStat:   "vinylstat",
			wantNcsa:   "vinylncsa",
		},
		{
			name:       "partial vinyl — only vinyladm-path set",
			vinyladm:   "/opt/vinyladm",
			lookPathFn: vinylNotFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "vinyld",
			wantAdm:    "/opt/vinyladm",
			wantStat:   "vinylstat",
			wantNcsa:   "vinylncsa",
		},
		{
			name:       "partial vinyl — only vinylstat-path set",
			vinylstat:  "/opt/vinylstat",
			lookPathFn: vinylNotFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "vinyld",
			wantAdm:    "vinyladm",
			wantStat:   "/opt/vinylstat",
			wantNcsa:   "vinylncsa",
		},
		{
			name:       "partial vinyl — only vinylncsa-path set",
			vinylncsa:  "/opt/vinylncsa",
			lookPathFn: vinylNotFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "vinyld",
			wantAdm:    "vinyladm",
			wantStat:   "vinylstat",
			wantNcsa:   "/opt/vinylncsa",
		},
		{
			name:       "explicit vinyl takes priority even when vinyl is also on PATH",
			vinyld:     "/custom/vinyld",
			lookPathFn: vinylFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "/custom/vinyld",
			wantAdm:    "vinyladm",
			wantStat:   "vinylstat",
			wantNcsa:   "vinylncsa",
		},
		{
			name:            "explicit varnish flags prevent auto-detect even when vinyl is on PATH",
			varnishExplicit: true,
			lookPathFn:      vinylFound,
			initD:           "/usr/sbin/varnishd",
			initAdm:         "/usr/sbin/varnishadm",
			initStat:        "/usr/sbin/varnishstat",
			initNcsa:        "/usr/sbin/varnishncsa",
			wantD:           "/usr/sbin/varnishd",
			wantAdm:         "/usr/sbin/varnishadm",
			wantStat:        "/usr/sbin/varnishstat",
			wantNcsa:        "/usr/sbin/varnishncsa",
		},
		{
			name:            "explicit varnish flags kept when vinyl not on PATH",
			varnishExplicit: true,
			lookPathFn:      vinylNotFound,
			initD:           "/usr/sbin/varnishd",
			initAdm:         "/usr/sbin/varnishadm",
			initStat:        "/usr/sbin/varnishstat",
			initNcsa:        "/usr/sbin/varnishncsa",
			wantD:           "/usr/sbin/varnishd",
			wantAdm:         "/usr/sbin/varnishadm",
			wantStat:        "/usr/sbin/varnishstat",
			wantNcsa:        "/usr/sbin/varnishncsa",
		},
		{
			name:       "defaults kept when vinyl not on PATH and no explicit flags",
			lookPathFn: vinylNotFound,
			initD:      "varnishd",
			initAdm:    "varnishadm",
			initStat:   "varnishstat",
			initNcsa:   "varnishncsa",
			wantD:      "varnishd",
			wantAdm:    "varnishadm",
			wantStat:   "varnishstat",
			wantNcsa:   "varnishncsa",
		},
		{
			name:            "vinyl flags win over varnishExplicit",
			vinyld:          "/opt/vinyld",
			varnishExplicit: true,
			lookPathFn:      vinylNotFound,
			initD:           "/usr/sbin/varnishd",
			initAdm:         "/usr/sbin/varnishadm",
			initStat:        "/usr/sbin/varnishstat",
			initNcsa:        "/usr/sbin/varnishncsa",
			wantD:           "/opt/vinyld",
			wantAdm:         "vinyladm",
			wantStat:        "vinylstat",
			wantNcsa:        "vinylncsa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &Config{
				VarnishdPath:    tt.initD,
				VarnishadmPath:  tt.initAdm,
				VarnishstatPath: tt.initStat,
				VarnishncsaPath: tt.initNcsa,
			}
			resolveBinaryPaths(c, tt.vinyld, tt.vinyladm, tt.vinylstat, tt.vinylncsa, tt.varnishExplicit, tt.lookPathFn)
			if c.VarnishdPath != tt.wantD {
				t.Errorf("VarnishdPath = %q, want %q", c.VarnishdPath, tt.wantD)
			}
			if c.VarnishadmPath != tt.wantAdm {
				t.Errorf("VarnishadmPath = %q, want %q", c.VarnishadmPath, tt.wantAdm)
			}
			if c.VarnishstatPath != tt.wantStat {
				t.Errorf("VarnishstatPath = %q, want %q", c.VarnishstatPath, tt.wantStat)
			}
			if c.VarnishncsaPath != tt.wantNcsa {
				t.Errorf("VarnishncsaPath = %q, want %q", c.VarnishncsaPath, tt.wantNcsa)
			}
		})
	}
}

func TestParseOverrideDurationFlags(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--admin-timeout=45s",
		"--debounce=5s",
		"--debounce-max=15s",
		"--frontend-debounce=500ms",
		"--frontend-debounce-max=3s",
		"--backend-debounce=1s",
		"--backend-debounce-max=8s",
		"--shutdown-timeout=60s",
		"--broadcast-drain-timeout=45s",
		"--broadcast-shutdown-timeout=10s",
		"--broadcast-server-idle-timeout=180s",
		"--broadcast-read-header-timeout=15s",
		"--broadcast-client-idle-timeout=8s",
		"--broadcast-client-timeout=6s",
		"--drain-poll-interval=2s",
		"--drain-delay=20s",
		"--drain-timeout=90s",
		"--values-dir-poll-interval=10s",
		"--metrics-read-header-timeout=20s",
		"--vcl-template-watch-interval=10s",
		"--vcl-reload-retry-interval=5s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"AdminTimeout", cfg.AdminTimeout, 45 * time.Second},
		{"Debounce", cfg.Debounce, 5 * time.Second},
		{"DebounceMax", cfg.DebounceMax, 15 * time.Second},
		{"FrontendDebounce", cfg.FrontendDebounce, 500 * time.Millisecond},
		{"FrontendDebounceMax", cfg.FrontendDebounceMax, 3 * time.Second},
		{"BackendDebounce", cfg.BackendDebounce, 1 * time.Second},
		{"BackendDebounceMax", cfg.BackendDebounceMax, 8 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 60 * time.Second},
		{"BroadcastDrainTimeout", cfg.BroadcastDrainTimeout, 45 * time.Second},
		{"BroadcastShutdownTimeout", cfg.BroadcastShutdownTimeout, 10 * time.Second},
		{"BroadcastServerIdleTimeout", cfg.BroadcastServerIdleTimeout, 180 * time.Second},
		{"BroadcastReadHeaderTimeout", cfg.BroadcastReadHeaderTimeout, 15 * time.Second},
		{"BroadcastClientIdleTimeout", cfg.BroadcastClientIdleTimeout, 8 * time.Second},
		{"BroadcastClientTimeout", cfg.BroadcastClientTimeout, 6 * time.Second},
		{"DrainPollInterval", cfg.DrainPollInterval, 2 * time.Second},
		{"DrainDelay", cfg.DrainDelay, 20 * time.Second},
		{"DrainTimeout", cfg.DrainTimeout, 90 * time.Second},
		{"ValuesDirPollInterval", cfg.ValuesDirPollInterval, 10 * time.Second},
		{"MetricsReadHeaderTimeout", cfg.MetricsReadHeaderTimeout, 20 * time.Second},
		{"VCLTemplateWatchInterval", cfg.VCLTemplateWatchInterval, 10 * time.Second},
		{"VCLReloadRetryInterval", cfg.VCLReloadRetryInterval, 5 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		level string
		want  slog.Level
	}{
		{"DEBUG", "DEBUG", slog.LevelDebug},
		{"INFO", "INFO", slog.LevelInfo},
		{"WARN", "WARN", slog.LevelWarn},
		{"ERROR", "ERROR", slog.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vcl := makeTempVCL(t)
			cfg, err := Parse("", []string{
				"test",
				"--service-name=my-svc",
				"--namespace=default",
				"--vcl-template=" + vcl,
				"--log-level=" + tt.level,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.LogLevel != tt.want {
				t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, tt.want)
			}
		})
	}
}

func TestParseLogLevelDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
}

func TestParseLogFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		format string
	}{
		{"text", "text"},
		{"json", "json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vcl := makeTempVCL(t)
			cfg, err := Parse("", []string{
				"test",
				"--service-name=my-svc",
				"--namespace=default",
				"--vcl-template=" + vcl,
				"--log-format=" + tt.format,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.LogFormat != tt.format {
				t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, tt.format)
			}
		})
	}
}

func TestParseLogFormatDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want \"text\"", cfg.LogFormat)
	}
}

func TestParseLogFormatInvalid(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--log-format=yaml",
	})
	if err == nil {
		t.Fatal("expected error for invalid log format")
	}
}

func TestParseNoBackends(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 0 {
		t.Errorf("Backends = %v, want empty", cfg.Backends)
	}
}

func TestParseNoExtraVarnishdArgs(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ExtraVarnishd) != 0 {
		t.Errorf("ExtraVarnishd = %v, want empty", cfg.ExtraVarnishd)
	}
}

func TestParseBackendTrailingSlash(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:ns/",
	})
	if err == nil {
		t.Fatal("expected error for backend with trailing slash (empty service)")
	}
	if !strings.Contains(err.Error(), "--backend") {
		t.Errorf("error = %q, want substring '--backend'", err)
	}
}

func TestParseBroadcastTargetMatchesUnnamedListenAddr(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	// Unnamed listen addrs have Name="". Setting --broadcast-target-listen-addr
	// to a value that doesn't match any named addr should fail.
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=:8080",
		"--broadcast-target-listen-addr=default",
	})
	if err == nil {
		t.Fatal("expected error when broadcast target doesn't match any named listen addr")
	}
	if !strings.Contains(err.Error(), "does not match any --listen-addr name") {
		t.Errorf("error = %q, want substring 'does not match any --listen-addr name'", err)
	}
}

func TestParseBroadcastTargetDefaultsToFirstListenAddr(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=:9090",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With no broadcast-target-listen-addr, the first listen addr port is used.
	if cfg.BroadcastTargetPort != 9090 {
		t.Errorf("BroadcastTargetPort = %d, want 9090", cfg.BroadcastTargetPort)
	}
}

func TestParseListenAddrIPv6AllInterfaces(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=[::]:8080,HTTP",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ListenAddrs) != 1 {
		t.Fatalf("expected 1 listen addr, got %d", len(cfg.ListenAddrs))
	}
	la := cfg.ListenAddrs[0]
	if la.Name != "http" {
		t.Errorf("Name = %q, want http", la.Name)
	}
	if la.Host != "::" {
		t.Errorf("Host = %q, want ::", la.Host)
	}
	if la.Port != 8080 {
		t.Errorf("Port = %d, want 8080", la.Port)
	}
}

func TestParseListenAddrNameWithSpecialChars(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=my-http.v2=:8080",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddrs[0].Name != "my-http.v2" {
		t.Errorf("Name = %q, want my-http.v2", cfg.ListenAddrs[0].Name)
	}
}

func TestParseDefaultDurations(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"AdminTimeout", cfg.AdminTimeout, 30 * time.Second},
		{"Debounce", cfg.Debounce, 2 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 30 * time.Second},
		{"BroadcastDrainTimeout", cfg.BroadcastDrainTimeout, 30 * time.Second},
		{"BroadcastShutdownTimeout", cfg.BroadcastShutdownTimeout, 5 * time.Second},
		{"BroadcastServerIdleTimeout", cfg.BroadcastServerIdleTimeout, 120 * time.Second},
		{"BroadcastReadHeaderTimeout", cfg.BroadcastReadHeaderTimeout, 10 * time.Second},
		{"BroadcastClientIdleTimeout", cfg.BroadcastClientIdleTimeout, 4 * time.Second},
		{"BroadcastClientTimeout", cfg.BroadcastClientTimeout, 3 * time.Second},
		{"DrainPollInterval", cfg.DrainPollInterval, 1 * time.Second},
		{"MetricsReadHeaderTimeout", cfg.MetricsReadHeaderTimeout, 10 * time.Second},
		{"VCLTemplateWatchInterval", cfg.VCLTemplateWatchInterval, 5 * time.Second},
		{"VCLReloadRetryInterval", cfg.VCLReloadRetryInterval, 2 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseBackendWithNumericPort(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc:3000",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	if cfg.Backends[0].Port != "3000" {
		t.Errorf("Backends[0].Port = %q, want 3000", cfg.Backends[0].Port)
	}
	if cfg.Backends[0].Namespace != "default" {
		t.Errorf("Backends[0].Namespace = %q, want default", cfg.Backends[0].Namespace)
	}
}

func TestParseServiceNameEmptyServiceAfterSlash(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=ns/",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err == nil {
		t.Fatal("expected error for --service-name=ns/ (empty service)")
	}
	if !strings.Contains(err.Error(), "--service-name") {
		t.Errorf("error = %q, want substring '--service-name'", err)
	}
}

func TestParseBroadcastDisabledSkipsTargetResolution(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	// With broadcast disabled, --broadcast-target-listen-addr should be ignored
	// even if it references a nonexistent name.
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-addr=none",
		"--broadcast-target-listen-addr=nonexistent",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BroadcastTargetPort != 0 {
		t.Errorf("BroadcastTargetPort = %d, want 0", cfg.BroadcastTargetPort)
	}
}

func TestParseBroadcastTargetMatchesFirstNamedAddr(t *testing.T) {
	t.Parallel()
	// When --broadcast-target-listen-addr explicitly names the first listen addr,
	// the code takes the loop path rather than the shortcut.
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080,HTTP",
		"--listen-addr=purge=:8081",
		"--broadcast-target-listen-addr=http",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BroadcastTargetPort != 8080 {
		t.Errorf("BroadcastTargetPort = %d, want 8080", cfg.BroadcastTargetPort)
	}
}

func TestParseDurationZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=0s",
		"--shutdown-timeout=0s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Debounce != 0 {
		t.Errorf("Debounce = %v, want 0s", cfg.Debounce)
	}
	if cfg.ShutdownTimeout != 0 {
		t.Errorf("ShutdownTimeout = %v, want 0s", cfg.ShutdownTimeout)
	}
}

func TestParseDurationSubSecond(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=100ms",
		"--broadcast-client-timeout=500ms",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Debounce != 100*time.Millisecond {
		t.Errorf("Debounce = %v, want 100ms", cfg.Debounce)
	}
	if cfg.BroadcastClientTimeout != 500*time.Millisecond {
		t.Errorf("BroadcastClientTimeout = %v, want 500ms", cfg.BroadcastClientTimeout)
	}
}

func TestParseMultipleBackendsWithPorts(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc:8080",
		"--backend=web:web-svc:http",
		"--backend=grpc:other-ns/grpc-svc:9090",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 3 {
		t.Fatalf("expected 3 backends, got %d", len(cfg.Backends))
	}
	// Third backend: explicit namespace
	if cfg.Backends[2].Name != "grpc" {
		t.Errorf("Backends[2].Name = %q, want grpc", cfg.Backends[2].Name)
	}
	if cfg.Backends[2].Namespace != "other-ns" {
		t.Errorf("Backends[2].Namespace = %q, want other-ns", cfg.Backends[2].Namespace)
	}
	if cfg.Backends[2].ServiceName != "grpc-svc" {
		t.Errorf("Backends[2].ServiceName = %q, want grpc-svc", cfg.Backends[2].ServiceName)
	}
	if cfg.Backends[2].Port != "9090" {
		t.Errorf("Backends[2].Port = %q, want 9090", cfg.Backends[2].Port)
	}
}

// --- --values flag tests ---

func TestValuesFlagsSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantVS  ValuesSpec
		wantErr bool
	}{
		{
			name:  "name and configmap",
			input: "tuning:my-cm",
			wantVS: ValuesSpec{
				Name:          "tuning",
				ConfigMapName: "my-cm",
			},
		},
		{
			name:  "with namespace prefix",
			input: "tuning:staging/my-cm",
			wantVS: ValuesSpec{
				Name:          "tuning",
				ConfigMapName: "staging/my-cm",
			},
		},
		{
			name:    "missing configmap",
			input:   "tuning",
			wantErr: true,
		},
		{
			name:    "empty name",
			input:   ":my-cm",
			wantErr: true,
		},
		{
			name:    "empty configmap",
			input:   "tuning:",
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
			t.Parallel()
			var vf valuesFlags
			err := vf.Set(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", vf)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(vf) != 1 {
				t.Fatalf("expected 1 values spec, got %d", len(vf))
			}
			got := vf[0]
			if got.Name != tt.wantVS.Name {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantVS.Name)
			}
			if got.ConfigMapName != tt.wantVS.ConfigMapName {
				t.Errorf("ConfigMapName = %q, want %q", got.ConfigMapName, tt.wantVS.ConfigMapName)
			}
		})
	}
}

func TestValuesFlagsString(t *testing.T) {
	t.Parallel()
	var vf valuesFlags
	_ = vf.Set("tuning:my-cm")
	s := vf.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestParseDuplicateValuesNames(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values=tuning:cm-a",
		"--values=tuning:cm-b",
	})
	if err == nil {
		t.Fatal("expected error for duplicate --values name")
	}
	if !strings.Contains(err.Error(), "duplicate --values name") {
		t.Errorf("error = %q, want substring 'duplicate --values name'", err)
	}
}

func TestParseValuesNamespaceResolution(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values=tuning:my-cm",
		"--values=config:staging/other-cm",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(cfg.Values))
	}
	// First values uses default namespace.
	if cfg.Values[0].Namespace != "default" {
		t.Errorf("Values[0].Namespace = %q, want default", cfg.Values[0].Namespace)
	}
	if cfg.Values[0].ConfigMapName != "my-cm" {
		t.Errorf("Values[0].ConfigMapName = %q, want my-cm", cfg.Values[0].ConfigMapName)
	}
	// Second values uses explicit namespace.
	if cfg.Values[1].Namespace != "staging" {
		t.Errorf("Values[1].Namespace = %q, want staging", cfg.Values[1].Namespace)
	}
	if cfg.Values[1].ConfigMapName != "other-cm" {
		t.Errorf("Values[1].ConfigMapName = %q, want other-cm", cfg.Values[1].ConfigMapName)
	}
}

func TestParseValuesInvalidConfigMapRef(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values=tuning:/cm",
	})
	if err == nil {
		t.Fatal("expected error for invalid values configmap reference")
	}
	if !strings.Contains(err.Error(), "--values") {
		t.Errorf("error = %q, want substring '--values'", err)
	}
}

func TestParseNoValues(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Values) != 0 {
		t.Errorf("Values = %v, want empty", cfg.Values)
	}
}

// --- --secrets flag tests ---

func TestSecretsFlagsSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantSS  SecretsSpec
		wantErr bool
	}{
		{
			name:  "name and secret",
			input: "auth:my-secret",
			wantSS: SecretsSpec{
				Name:       "auth",
				SecretName: "my-secret",
			},
		},
		{
			name:  "with namespace prefix",
			input: "auth:staging/my-secret",
			wantSS: SecretsSpec{ //nolint:gosec // G101: test fixture, not a credential
				Name:       "auth",
				SecretName: "staging/my-secret",
			},
		},
		{
			name:    "missing secret",
			input:   "auth",
			wantErr: true,
		},
		{
			name:    "empty name",
			input:   ":my-secret",
			wantErr: true,
		},
		{
			name:    "empty secret",
			input:   "auth:",
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
			t.Parallel()
			var sf secretsFlags
			err := sf.Set(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", sf)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(sf) != 1 {
				t.Fatalf("expected 1 secrets spec, got %d", len(sf))
			}
			got := sf[0]
			if got.Name != tt.wantSS.Name {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantSS.Name)
			}
			if got.SecretName != tt.wantSS.SecretName {
				t.Errorf("SecretName = %q, want %q", got.SecretName, tt.wantSS.SecretName)
			}
		})
	}
}

func TestSecretsFlagsString(t *testing.T) {
	t.Parallel()
	var sf secretsFlags
	_ = sf.Set("auth:my-secret")
	s := sf.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestParseDuplicateSecretsNames(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--secrets=auth:secret-a",
		"--secrets=auth:secret-b",
	})
	if err == nil {
		t.Fatal("expected error for duplicate --secrets name")
	}
	if !strings.Contains(err.Error(), "duplicate --secrets name") {
		t.Errorf("error = %q, want substring 'duplicate --secrets name'", err)
	}
}

func TestParseSecretsNamespaceResolution(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--secrets=auth:my-secret",
		"--secrets=config:staging/other-secret",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(cfg.Secrets))
	}
	// First secret uses default namespace.
	if cfg.Secrets[0].Namespace != "default" {
		t.Errorf("Secrets[0].Namespace = %q, want default", cfg.Secrets[0].Namespace)
	}
	if cfg.Secrets[0].SecretName != "my-secret" {
		t.Errorf("Secrets[0].SecretName = %q, want my-secret", cfg.Secrets[0].SecretName)
	}
	// Second secret uses explicit namespace.
	if cfg.Secrets[1].Namespace != "staging" {
		t.Errorf("Secrets[1].Namespace = %q, want staging", cfg.Secrets[1].Namespace)
	}
	if cfg.Secrets[1].SecretName != "other-secret" {
		t.Errorf("Secrets[1].SecretName = %q, want other-secret", cfg.Secrets[1].SecretName)
	}
}

func TestParseSecretsInvalidRef(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--secrets=auth:/secret",
	})
	if err == nil {
		t.Fatal("expected error for invalid secrets reference")
	}
	if !strings.Contains(err.Error(), "--secrets") {
		t.Errorf("error = %q, want substring '--secrets'", err)
	}
}

// --- --values-dir flag tests ---

func TestValuesDirFlagsSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantSpec ValuesDirSpec
		wantErr  bool
	}{
		{
			name:     "name and path",
			input:    "tuning:/etc/values",
			wantSpec: ValuesDirSpec{Name: "tuning", Path: "/etc/values"},
		},
		{
			name:     "path with colons",
			input:    "tuning:C:/data/values",
			wantSpec: ValuesDirSpec{Name: "tuning", Path: "C:/data/values"},
		},
		{
			name:    "missing path",
			input:   "tuning",
			wantErr: true,
		},
		{
			name:    "empty name",
			input:   ":/etc/values",
			wantErr: true,
		},
		{
			name:    "empty path",
			input:   "tuning:",
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
			t.Parallel()
			var vdf valuesDirFlags
			err := vdf.Set(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", vdf)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(vdf) != 1 {
				t.Fatalf("expected 1 values-dir spec, got %d", len(vdf))
			}
			got := vdf[0]
			if got.Name != tt.wantSpec.Name {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantSpec.Name)
			}
			if got.Path != tt.wantSpec.Path {
				t.Errorf("Path = %q, want %q", got.Path, tt.wantSpec.Path)
			}
		})
	}
}

func TestValuesDirFlagsString(t *testing.T) {
	t.Parallel()
	var vdf valuesDirFlags
	_ = vdf.Set("tuning:/etc/values")
	s := vdf.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestParseDuplicateValuesDirNames(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	dir := t.TempDir()
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values-dir=tuning:" + dir,
		"--values-dir=tuning:" + dir,
	})
	if err == nil {
		t.Fatal("expected error for duplicate --values-dir name")
	}
	if !strings.Contains(err.Error(), "duplicate values name") {
		t.Errorf("error = %q, want substring 'duplicate values name'", err)
	}
}

func TestParseDuplicateValuesAndValuesDirNames(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	dir := t.TempDir()
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values=tuning:my-cm",
		"--values-dir=tuning:" + dir,
	})
	if err == nil {
		t.Fatal("expected error for name collision across --values and --values-dir")
	}
	if !strings.Contains(err.Error(), "duplicate values name") {
		t.Errorf("error = %q, want substring 'duplicate values name'", err)
	}
}

func TestParseValuesDirNotADirectory(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	// Create a regular file, not a directory.
	f, err := os.CreateTemp(t.TempDir(), "not-a-dir-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	_, err = Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values-dir=tuning:" + f.Name(),
	})
	if err == nil {
		t.Fatal("expected error for path that is not a directory")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want substring 'not a directory'", err)
	}
}

func TestParseValuesDirNotExist(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values-dir=tuning:/nonexistent/path/to/dir",
	})
	if err == nil {
		t.Fatal("expected error for non-existent directory")
	}
	if !strings.Contains(err.Error(), "--values-dir") {
		t.Errorf("error = %q, want substring '--values-dir'", err)
	}
}

func TestParseValuesDirValid(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	dir := t.TempDir()
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values-dir=tuning:" + dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ValuesDirs) != 1 {
		t.Fatalf("expected 1 values-dir, got %d", len(cfg.ValuesDirs))
	}
	if cfg.ValuesDirs[0].Name != "tuning" {
		t.Errorf("ValuesDirs[0].Name = %q, want tuning", cfg.ValuesDirs[0].Name)
	}
	if cfg.ValuesDirs[0].Path != dir {
		t.Errorf("ValuesDirs[0].Path = %q, want %q", cfg.ValuesDirs[0].Path, dir)
	}
}

// --- --template-delims flag tests ---

func TestParseTemplateDelimsDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TemplateDelimLeft != "<<" {
		t.Errorf("TemplateDelimLeft = %q, want %q", cfg.TemplateDelimLeft, "<<")
	}
	if cfg.TemplateDelimRight != ">>" {
		t.Errorf("TemplateDelimRight = %q, want %q", cfg.TemplateDelimRight, ">>")
	}
}

func TestParseTemplateDelimsCustom(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--template-delims={{ }}",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TemplateDelimLeft != "{{" {
		t.Errorf("TemplateDelimLeft = %q, want %q", cfg.TemplateDelimLeft, "{{")
	}
	if cfg.TemplateDelimRight != "}}" {
		t.Errorf("TemplateDelimRight = %q, want %q", cfg.TemplateDelimRight, "}}")
	}
}

func TestParseTemplateDelimsSingleToken(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--template-delims=<<",
	})
	if err == nil {
		t.Fatal("expected error for single-token --template-delims")
	}
	if !strings.Contains(err.Error(), "--template-delims must be exactly two tokens") {
		t.Errorf("error = %q, want substring '--template-delims must be exactly two tokens'", err)
	}
}

func TestParseTemplateDelimsThreeTokens(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--template-delims=<< >> extra",
	})
	if err == nil {
		t.Fatal("expected error for three-token --template-delims")
	}
	if !strings.Contains(err.Error(), "--template-delims must be exactly two tokens") {
		t.Errorf("error = %q, want substring '--template-delims must be exactly two tokens'", err)
	}
}

// --- --template-funcs flag tests ---

func TestParseTemplateFuncsDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TemplateFuncs != "sprig" {
		t.Errorf("TemplateFuncs = %q, want %q", cfg.TemplateFuncs, "sprig")
	}
}

func TestParseTemplateFuncsSprout(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--template-funcs=sprout",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TemplateFuncs != "sprout" {
		t.Errorf("TemplateFuncs = %q, want %q", cfg.TemplateFuncs, "sprout")
	}
}

func TestParseTemplateFuncsInvalid(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--template-funcs=invalid",
	})
	if err == nil {
		t.Fatal("expected error for invalid --template-funcs")
	}
	if !strings.Contains(err.Error(), "--template-funcs must be") {
		t.Errorf("error = %q, want substring '--template-funcs must be'", err)
	}
}

func TestParseVCLReloadRetriesDefaults(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLReloadRetries != 3 {
		t.Errorf("VCLReloadRetries = %d, want 3", cfg.VCLReloadRetries)
	}
	if cfg.VCLReloadRetryInterval != 2*time.Second {
		t.Errorf("VCLReloadRetryInterval = %v, want 2s", cfg.VCLReloadRetryInterval)
	}
}

func TestParseVCLReloadRetriesCustom(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retries=5",
		"--vcl-reload-retry-interval=500ms",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLReloadRetries != 5 {
		t.Errorf("VCLReloadRetries = %d, want 5", cfg.VCLReloadRetries)
	}
	if cfg.VCLReloadRetryInterval != 500*time.Millisecond {
		t.Errorf("VCLReloadRetryInterval = %v, want 500ms", cfg.VCLReloadRetryInterval)
	}
}

func TestParseVCLReloadRetriesZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retries=0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLReloadRetries != 0 {
		t.Errorf("VCLReloadRetries = %d, want 0", cfg.VCLReloadRetries)
	}
}

func TestParseVCLReloadRetriesNegative(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retries=-1",
	})
	if err == nil {
		t.Fatal("expected error for negative --vcl-reload-retries")
	}
	if !strings.Contains(err.Error(), "--vcl-reload-retries must be >= 0") {
		t.Errorf("error = %q, want substring '--vcl-reload-retries must be >= 0'", err)
	}
}

func TestParseVCLReloadRetryIntervalNegative(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retry-interval=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --vcl-reload-retry-interval")
	}
	if !strings.Contains(err.Error(), "--vcl-reload-retry-interval must be >= 0") {
		t.Errorf("error = %q, want substring '--vcl-reload-retry-interval must be >= 0'", err)
	}
}

func TestParseVCLReloadRetryIntervalZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retry-interval=0s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLReloadRetryInterval != 0 {
		t.Errorf("VCLReloadRetryInterval = %v, want 0s", cfg.VCLReloadRetryInterval)
	}
}

func TestParseVCLReloadRetryIntervalSubSecond(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retry-interval=100ms",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLReloadRetryInterval != 100*time.Millisecond {
		t.Errorf("VCLReloadRetryInterval = %v, want 100ms", cfg.VCLReloadRetryInterval)
	}
}

func TestParseVCLReloadRetriesOnlyRetriesSet(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retries=10",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLReloadRetries != 10 {
		t.Errorf("VCLReloadRetries = %d, want 10", cfg.VCLReloadRetries)
	}
	// Interval should remain at default.
	if cfg.VCLReloadRetryInterval != 2*time.Second {
		t.Errorf("VCLReloadRetryInterval = %v, want 2s (default)", cfg.VCLReloadRetryInterval)
	}
}

func TestParseVCLReloadRetriesOnlyIntervalSet(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-reload-retry-interval=10s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Retries should remain at default.
	if cfg.VCLReloadRetries != 3 {
		t.Errorf("VCLReloadRetries = %d, want 3 (default)", cfg.VCLReloadRetries)
	}
	if cfg.VCLReloadRetryInterval != 10*time.Second {
		t.Errorf("VCLReloadRetryInterval = %v, want 10s", cfg.VCLReloadRetryInterval)
	}
}

// --- --vcl-kept flag tests ---

func TestParseVCLKeptDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLKept != 0 {
		t.Errorf("VCLKept = %d, want 0", cfg.VCLKept)
	}
}

func TestParseVCLKeptCustom(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-kept=3",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLKept != 3 {
		t.Errorf("VCLKept = %d, want 3", cfg.VCLKept)
	}
}

func TestParseVCLKeptNegative(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-kept=-1",
	})
	if err == nil {
		t.Fatal("expected error for negative --vcl-kept")
	}
	if !strings.Contains(err.Error(), "--vcl-kept must be >= 0") {
		t.Errorf("error = %q, want substring '--vcl-kept must be >= 0'", err)
	}
}

// --- --file-watch flag tests ---

func TestParseFileWatchDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.FileWatch {
		t.Error("FileWatch = false, want true (default)")
	}
}

func TestParseFileWatchDisabled(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--file-watch=false",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FileWatch {
		t.Error("FileWatch = true, want false")
	}
}

func TestParseHelp(t *testing.T) {
	t.Parallel()
	cfg, err := Parse("", []string{"test", "--help"})
	if !errors.Is(err, ErrHelp) {
		t.Fatalf("expected ErrHelp, got: %v", err)
	}
	if cfg != nil {
		t.Error("expected nil cfg for --help")
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cfg, parseErr := parse("v1.2.3", []string{"test", "--version"}, &buf)

	if !errors.Is(parseErr, ErrHelp) {
		t.Fatalf("expected ErrHelp, got: %v", parseErr)
	}
	if cfg != nil {
		t.Error("expected nil cfg for --version")
	}
	if got := strings.TrimSpace(buf.String()); got != "v1.2.3" {
		t.Errorf("--version output = %q, want %q", got, "v1.2.3")
	}
}

func TestParseShortAliases(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"-s", "my-svc",
		"-n", "default",
		"-t", vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServiceName != "my-svc" {
		t.Errorf("ServiceName = %q, want my-svc", cfg.ServiceName)
	}
	if cfg.Namespace != "default" {
		t.Errorf("Namespace = %q, want default", cfg.Namespace)
	}
}

func TestParseInvalidLogLevel(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--log-level=BOGUS",
	})
	if err == nil {
		t.Fatal("expected error for invalid log level")
	}
	if !strings.Contains(err.Error(), "--log-level") {
		t.Errorf("error = %q, want substring '--log-level'", err)
	}
}

func TestParseInvalidListenAddr(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=not-a-valid-addr",
	})
	if err == nil {
		t.Fatal("expected error for invalid listen address")
	}
}

func TestParseInvalidBackendFormat(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=no-colon",
	})
	if err == nil {
		t.Fatal("expected error for backend without colon separator")
	}
}

func TestParseInvalidValuesFormat(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values=no-colon",
	})
	if err == nil {
		t.Fatal("expected error for values without colon separator")
	}
}

func TestParseInvalidValuesDirFormat(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values-dir=no-colon",
	})
	if err == nil {
		t.Fatal("expected error for values-dir without colon separator")
	}
}

// --- --debounce-max flag tests ---

func TestParseDebounceMaxDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DebounceMax != 0 {
		t.Errorf("DebounceMax = %v, want 0", cfg.DebounceMax)
	}
}

func TestParseDebounceMaxCustom(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-max=10s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DebounceMax != 10*time.Second {
		t.Errorf("DebounceMax = %v, want 10s", cfg.DebounceMax)
	}
}

func TestParseDebounceMaxNegative(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-max=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --debounce-max")
	}
	if !strings.Contains(err.Error(), "--debounce-max must be >= 0") {
		t.Errorf("error = %q, want substring '--debounce-max must be >= 0'", err)
	}
}

func TestParseDebounceMaxLessThanDebounce(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=5s",
		"--debounce-max=2s",
	})
	if err == nil {
		t.Fatal("expected error when --debounce-max < --debounce")
	}
	if !strings.Contains(err.Error(), "--debounce-max") && !strings.Contains(err.Error(), "--debounce") {
		t.Errorf("error = %q, want substring about --debounce-max and --debounce", err)
	}
}

func TestParseDebounceMaxEqualToDebounce(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=2s",
		"--debounce-max=2s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DebounceMax != 2*time.Second {
		t.Errorf("DebounceMax = %v, want 2s", cfg.DebounceMax)
	}
}

// --- Per-source debounce tests ---

func TestParsePerSourceDebounceDefaultsToGlobal(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=3s",
		"--debounce-max=15s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FrontendDebounce != 3*time.Second {
		t.Errorf("FrontendDebounce = %v, want 3s", cfg.FrontendDebounce)
	}
	if cfg.FrontendDebounceMax != 15*time.Second {
		t.Errorf("FrontendDebounceMax = %v, want 15s", cfg.FrontendDebounceMax)
	}
	if cfg.BackendDebounce != 3*time.Second {
		t.Errorf("BackendDebounce = %v, want 3s", cfg.BackendDebounce)
	}
	if cfg.BackendDebounceMax != 15*time.Second {
		t.Errorf("BackendDebounceMax = %v, want 15s", cfg.BackendDebounceMax)
	}
}

func TestParsePerSourceDebounceExplicitOverride(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=3s",
		"--debounce-max=15s",
		"--frontend-debounce=500ms",
		"--frontend-debounce-max=3s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Frontend uses explicit values.
	if cfg.FrontendDebounce != 500*time.Millisecond {
		t.Errorf("FrontendDebounce = %v, want 500ms", cfg.FrontendDebounce)
	}
	if cfg.FrontendDebounceMax != 3*time.Second {
		t.Errorf("FrontendDebounceMax = %v, want 3s", cfg.FrontendDebounceMax)
	}
	// Backend inherits global.
	if cfg.BackendDebounce != 3*time.Second {
		t.Errorf("BackendDebounce = %v, want 3s", cfg.BackendDebounce)
	}
	if cfg.BackendDebounceMax != 15*time.Second {
		t.Errorf("BackendDebounceMax = %v, want 15s", cfg.BackendDebounceMax)
	}
}

func TestParsePerSourceDebounceMaxExplicitZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce-max=10s",
		"--frontend-debounce-max=0s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Frontend ceiling disabled.
	if cfg.FrontendDebounceMax != 0 {
		t.Errorf("FrontendDebounceMax = %v, want 0", cfg.FrontendDebounceMax)
	}
	// Backend inherits global.
	if cfg.BackendDebounceMax != 10*time.Second {
		t.Errorf("BackendDebounceMax = %v, want 10s", cfg.BackendDebounceMax)
	}
}

func TestParsePerSourceFrontendDebounceMaxLessThanDebounceError(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=5s",
		"--frontend-debounce-max=1s",
	})
	if err == nil {
		t.Fatal("expected error when resolved frontend debounce-max < debounce")
	}
	if !strings.Contains(err.Error(), "--frontend-debounce-max") {
		t.Errorf("error = %q, want substring '--frontend-debounce-max'", err)
	}
}

func TestParsePerSourceBackendDebounceMaxLessThanDebounceError(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=5s",
		"--backend-debounce-max=1s",
	})
	if err == nil {
		t.Fatal("expected error when resolved backend debounce-max < debounce")
	}
	if !strings.Contains(err.Error(), "--backend-debounce-max") {
		t.Errorf("error = %q, want substring '--backend-debounce-max'", err)
	}
}

func TestParsePerSourceBackendOverrideFrontendInheritsGlobal(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=3s",
		"--debounce-max=15s",
		"--backend-debounce=1s",
		"--backend-debounce-max=5s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Backend uses explicit values.
	if cfg.BackendDebounce != 1*time.Second {
		t.Errorf("BackendDebounce = %v, want 1s", cfg.BackendDebounce)
	}
	if cfg.BackendDebounceMax != 5*time.Second {
		t.Errorf("BackendDebounceMax = %v, want 5s", cfg.BackendDebounceMax)
	}
	// Frontend inherits global.
	if cfg.FrontendDebounce != 3*time.Second {
		t.Errorf("FrontendDebounce = %v, want 3s", cfg.FrontendDebounce)
	}
	if cfg.FrontendDebounceMax != 15*time.Second {
		t.Errorf("FrontendDebounceMax = %v, want 15s", cfg.FrontendDebounceMax)
	}
}

func TestParsePerSourceDebounceExplicitZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=3s",
		"--frontend-debounce=0s",
		"--backend-debounce=0s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FrontendDebounce != 0 {
		t.Errorf("FrontendDebounce = %v, want 0", cfg.FrontendDebounce)
	}
	if cfg.BackendDebounce != 0 {
		t.Errorf("BackendDebounce = %v, want 0", cfg.BackendDebounce)
	}
}

func TestParseZoneDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Zone != "" {
		t.Errorf("Zone = %q, want empty", cfg.Zone)
	}
}

func TestParseZoneExplicit(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--zone=europe-west3-a",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Zone != "europe-west3-a" {
		t.Errorf("Zone = %q, want europe-west3-a", cfg.Zone)
	}
}

func TestParseVarnishstatExportDefault(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VarnishstatExport {
		t.Error("VarnishstatExport = true, want false (default)")
	}
	if len(cfg.VarnishstatExportFilter) != 0 {
		t.Errorf("VarnishstatExportFilter = %v, want empty", cfg.VarnishstatExportFilter)
	}
}

func TestParseVarnishstatExportEnabled(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--varnishstat-export",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.VarnishstatExport {
		t.Error("VarnishstatExport = false, want true")
	}
}

func TestParseVarnishstatExportFilter(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--varnishstat-export",
		"--varnishstat-export-filter=MAIN",
		"--varnishstat-export-filter=SMA",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"MAIN", "SMA"}
	if !slices.Equal(cfg.VarnishstatExportFilter, want) {
		t.Errorf("VarnishstatExportFilter = %v, want %v", cfg.VarnishstatExportFilter, want)
	}
}

func TestParseVarnishncsaDefaults(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VarnishncsaEnabled {
		t.Error("VarnishncsaEnabled = true, want false (default)")
	}
	if cfg.VarnishncsaPath != "varnishncsa" {
		t.Errorf("VarnishncsaPath = %q, want %q", cfg.VarnishncsaPath, "varnishncsa")
	}
	if cfg.VarnishncsaFormat != "" {
		t.Errorf("VarnishncsaFormat = %q, want empty", cfg.VarnishncsaFormat)
	}
	if cfg.VarnishncsaQuery != "" {
		t.Errorf("VarnishncsaQuery = %q, want empty", cfg.VarnishncsaQuery)
	}
	if cfg.VarnishncsaBackend {
		t.Error("VarnishncsaBackend = true, want false")
	}
	if cfg.VarnishncsaOutput != "" {
		t.Errorf("VarnishncsaOutput = %q, want empty", cfg.VarnishncsaOutput)
	}
	if cfg.VarnishncsaPrefix != "[access] " {
		t.Errorf("VarnishncsaPrefix = %q, want %q", cfg.VarnishncsaPrefix, "[access] ")
	}
}

func TestParseVarnishncsaAllFlags(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--varnishncsa-enabled",
		"--varnishncsa-path=/usr/bin/varnishncsa",
		"--varnishncsa-format=%h %s %b",
		"--varnishncsa-query=ReqURL ~ /api",
		"--varnishncsa-backend",
		"--varnishncsa-output=/var/log/access.log",
		"--varnishncsa-prefix", "[ncsa] ",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.VarnishncsaEnabled {
		t.Error("VarnishncsaEnabled = false, want true")
	}
	if cfg.VarnishncsaPath != "/usr/bin/varnishncsa" {
		t.Errorf("VarnishncsaPath = %q, want /usr/bin/varnishncsa", cfg.VarnishncsaPath)
	}
	if cfg.VarnishncsaFormat != "%h %s %b" {
		t.Errorf("VarnishncsaFormat = %q, want %%h %%s %%b", cfg.VarnishncsaFormat)
	}
	if cfg.VarnishncsaQuery != "ReqURL ~ /api" {
		t.Errorf("VarnishncsaQuery = %q, want ReqURL ~ /api", cfg.VarnishncsaQuery)
	}
	if !cfg.VarnishncsaBackend {
		t.Error("VarnishncsaBackend = false, want true")
	}
	if cfg.VarnishncsaOutput != "/var/log/access.log" {
		t.Errorf("VarnishncsaOutput = %q, want /var/log/access.log", cfg.VarnishncsaOutput)
	}
	if cfg.VarnishncsaPrefix != "[ncsa] " {
		t.Errorf("VarnishncsaPrefix = %q, want %q", cfg.VarnishncsaPrefix, "[ncsa] ")
	}
}

func TestParseBackendSelectorSimple(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=app=myapp",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app=myapp" {
		t.Errorf("Selector = %q, want app=myapp", bs.Selector)
	}
	if bs.Namespace != "default" {
		t.Errorf("Namespace = %q, want default", bs.Namespace)
	}
	if bs.AllNamespaces {
		t.Error("AllNamespaces = true, want false")
	}
	if bs.Port != "" {
		t.Errorf("Port = %q, want empty", bs.Port)
	}
}

func TestParseBackendSelectorWithNamespaceAndPort(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=myns//app=myapp:8080",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app=myapp" {
		t.Errorf("Selector = %q, want app=myapp", bs.Selector)
	}
	if bs.Namespace != "myns" {
		t.Errorf("Namespace = %q, want myns", bs.Namespace)
	}
	if bs.AllNamespaces {
		t.Error("AllNamespaces = true, want false")
	}
	if bs.Port != "8080" {
		t.Errorf("Port = %q, want 8080", bs.Port)
	}
}

func TestParseBackendSelectorAllNamespaces(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=*//app=myapp",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app=myapp" {
		t.Errorf("Selector = %q, want app=myapp", bs.Selector)
	}
	if !bs.AllNamespaces {
		t.Error("AllNamespaces = false, want true")
	}
	if bs.Namespace != "" {
		t.Errorf("Namespace = %q, want empty", bs.Namespace)
	}
}

func TestParseBackendSelectorInvalidSelector(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=!!!invalid",
	})
	if err == nil {
		t.Fatal("expected error for invalid selector")
	}
}

func TestParseBackendSelectorMultiple(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=app=web",
		"--backend-selector=*//tier=api:9090",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 2 {
		t.Fatalf("expected 2 backend selectors, got %d", len(cfg.BackendSelectors))
	}
	if cfg.BackendSelectors[0].Selector != "app=web" {
		t.Errorf("[0].Selector = %q, want app=web", cfg.BackendSelectors[0].Selector)
	}
	if !cfg.BackendSelectors[1].AllNamespaces {
		t.Error("[1].AllNamespaces = false, want true")
	}
	if cfg.BackendSelectors[1].Port != "9090" {
		t.Errorf("[1].Port = %q, want 9090", cfg.BackendSelectors[1].Port)
	}
}

func TestParseBackendSelectorEmpty(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=",
	})
	if err == nil {
		t.Fatal("expected error for empty selector")
	}
}

func TestParseBackendSelectorInvalidNamespace(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=INVALID//app=myapp",
	})
	if err == nil {
		t.Fatal("expected error for invalid namespace")
	}
}

func TestParseBackendSelectorNamedPort(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=app=myapp:http",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app=myapp" {
		t.Errorf("Selector = %q, want app=myapp", bs.Selector)
	}
	if bs.Port != "http" {
		t.Errorf("Port = %q, want http", bs.Port)
	}
}

func TestParseBackendSelectorNamedPortWithNamespace(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=myns//app=myapp:grpc",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Namespace != "myns" {
		t.Errorf("Namespace = %q, want myns", bs.Namespace)
	}
	if bs.Selector != "app=myapp" {
		t.Errorf("Selector = %q, want app=myapp", bs.Selector)
	}
	if bs.Port != "grpc" {
		t.Errorf("Port = %q, want grpc", bs.Port)
	}
}

func TestParseBackendSelectorDomainPrefixedLabel(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=app.kubernetes.io/name=myapp",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app.kubernetes.io/name=myapp" {
		t.Errorf("Selector = %q, want app.kubernetes.io/name=myapp", bs.Selector)
	}
	if bs.Namespace != "default" {
		t.Errorf("Namespace = %q, want default", bs.Namespace)
	}
	if bs.AllNamespaces {
		t.Error("AllNamespaces = true, want false")
	}
	if bs.Port != "" {
		t.Errorf("Port = %q, want empty", bs.Port)
	}
}

func TestParseBackendSelectorDomainPrefixedLabelWithNamespace(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=myns//app.kubernetes.io/name=myapp:8080",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app.kubernetes.io/name=myapp" {
		t.Errorf("Selector = %q, want app.kubernetes.io/name=myapp", bs.Selector)
	}
	if bs.Namespace != "myns" {
		t.Errorf("Namespace = %q, want myns", bs.Namespace)
	}
	if bs.AllNamespaces {
		t.Error("AllNamespaces = true, want false")
	}
	if bs.Port != "8080" {
		t.Errorf("Port = %q, want 8080", bs.Port)
	}
}

func TestParseBackendSelectorDomainPrefixedLabelAllNamespaces(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=*//app.kubernetes.io/name=myapp",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app.kubernetes.io/name=myapp" {
		t.Errorf("Selector = %q, want app.kubernetes.io/name=myapp", bs.Selector)
	}
	if !bs.AllNamespaces {
		t.Error("AllNamespaces = false, want true")
	}
	if bs.Namespace != "" {
		t.Errorf("Namespace = %q, want empty", bs.Namespace)
	}
	if bs.Port != "" {
		t.Errorf("Port = %q, want empty", bs.Port)
	}
}

func TestParseBackendSelectorDomainPrefixedLabelAllNamespacesWithPort(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=*//app.kubernetes.io/name=myapp:8080",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.BackendSelectors) != 1 {
		t.Fatalf("expected 1 backend selector, got %d", len(cfg.BackendSelectors))
	}
	bs := cfg.BackendSelectors[0]
	if bs.Selector != "app.kubernetes.io/name=myapp" {
		t.Errorf("Selector = %q, want app.kubernetes.io/name=myapp", bs.Selector)
	}
	if !bs.AllNamespaces {
		t.Error("AllNamespaces = false, want true")
	}
	if bs.Port != "8080" {
		t.Errorf("Port = %q, want 8080", bs.Port)
	}
}

func TestParseBackendSelectorEmptyNamespacePrefix(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=//app=myapp",
	})
	if err == nil {
		t.Fatal("expected error for empty namespace prefix")
	}
}

func TestParseBackendSelectorEmptyPort(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=app=myapp:",
	})
	if err == nil {
		t.Fatal("expected error for empty port suffix")
	}
}

func TestParseBackendSelectorPortOutOfRange(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=app=myapp:99999",
	})
	if err == nil {
		t.Fatal("expected error for port out of range")
	}
}

func TestParseBackendSelectorPortZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=app=myapp:0",
	})
	if err == nil {
		t.Fatal("expected error for port 0")
	}
}

func TestParseExcludeAnnotations(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--exclude-annotations=internal.example.com/*",
		"--exclude-annotations=custom-key",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"internal.example.com/*", "custom-key"}
	if !slices.Equal(cfg.ExcludeAnnotations, want) {
		t.Errorf("ExcludeAnnotations = %v, want %v", cfg.ExcludeAnnotations, want)
	}
}

func TestParseExcludeAnnotationsEmpty(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	cfg, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ExcludeAnnotations) != 0 {
		t.Errorf("ExcludeAnnotations = %v, want empty", cfg.ExcludeAnnotations)
	}
}

func TestParseNamespaceInvalidDNSLabel(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=My_Namespace",
		"--vcl-template=" + vcl,
	})
	if err == nil {
		t.Fatal("expected error for invalid --namespace DNS label")
	}
	if !strings.Contains(err.Error(), "not a valid RFC 1123 DNS label") {
		t.Errorf("error = %q, want substring 'not a valid RFC 1123 DNS label'", err)
	}
}

func TestParseValuesDirPollIntervalNegative(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values-dir-poll-interval=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --values-dir-poll-interval")
	}
	if !strings.Contains(err.Error(), "--values-dir-poll-interval must be > 0") {
		t.Errorf("error = %q, want substring '--values-dir-poll-interval must be > 0'", err)
	}
}

func TestParseValuesDirPollIntervalZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--values-dir-poll-interval=0",
	})
	if err == nil {
		t.Fatal("expected error for zero --values-dir-poll-interval")
	}
	if !strings.Contains(err.Error(), "--values-dir-poll-interval must be > 0") {
		t.Errorf("error = %q, want substring '--values-dir-poll-interval must be > 0'", err)
	}
}

func TestParseVCLTemplateWatchIntervalZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--vcl-template-watch-interval=0",
	})
	if err == nil {
		t.Fatal("expected error for zero --vcl-template-watch-interval")
	}
	if !strings.Contains(err.Error(), "--vcl-template-watch-interval must be > 0") {
		t.Errorf("error = %q, want substring '--vcl-template-watch-interval must be > 0'", err)
	}
}

func TestParseDrainPollIntervalZero(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--drain-poll-interval=0",
	})
	if err == nil {
		t.Fatal("expected error for zero --drain-poll-interval")
	}
	if !strings.Contains(err.Error(), "--drain-poll-interval must be > 0") {
		t.Errorf("error = %q, want substring '--drain-poll-interval must be > 0'", err)
	}
}

func TestParseMetricsReadHeaderTimeoutNeg(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--metrics-read-header-timeout=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --metrics-read-header-timeout")
	}
	if !strings.Contains(err.Error(), "--metrics-read-header-timeout must be >= 0") {
		t.Errorf("error = %q, want substring '--metrics-read-header-timeout must be >= 0'", err)
	}
}

func TestParseBroadcastReadHeaderTimeoutNeg(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-read-header-timeout=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --broadcast-read-header-timeout")
	}
	if !strings.Contains(err.Error(), "--broadcast-read-header-timeout must be >= 0") {
		t.Errorf("error = %q, want substring '--broadcast-read-header-timeout must be >= 0'", err)
	}
}

func TestParseBroadcastServerIdleTimeoutNeg(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-server-idle-timeout=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --broadcast-server-idle-timeout")
	}
	if !strings.Contains(err.Error(), "--broadcast-server-idle-timeout must be >= 0") {
		t.Errorf("error = %q, want substring '--broadcast-server-idle-timeout must be >= 0'", err)
	}
}

func TestParseDebounceNegative(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --debounce")
	}
	if !strings.Contains(err.Error(), "--debounce must be >= 0") {
		t.Errorf("error = %q, want substring '--debounce must be >= 0'", err)
	}
}

func TestParseAdminTimeoutNeg(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--admin-timeout=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --admin-timeout")
	}
	if !strings.Contains(err.Error(), "--admin-timeout must be >= 0") {
		t.Errorf("error = %q, want substring '--admin-timeout must be >= 0'", err)
	}
}

func TestParseShutdownTimeoutNeg(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--shutdown-timeout=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --shutdown-timeout")
	}
	if !strings.Contains(err.Error(), "--shutdown-timeout must be >= 0") {
		t.Errorf("error = %q, want substring '--shutdown-timeout must be >= 0'", err)
	}
}

func TestParseBroadcastDrainTimeoutNeg(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-drain-timeout=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --broadcast-drain-timeout")
	}
	if !strings.Contains(err.Error(), "--broadcast-drain-timeout must be >= 0") {
		t.Errorf("error = %q, want substring '--broadcast-drain-timeout must be >= 0'", err)
	}
}

func TestParseBroadcastShutdownTimeoutNeg(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-shutdown-timeout=-1s",
	})
	if err == nil {
		t.Fatal("expected error for negative --broadcast-shutdown-timeout")
	}
	if !strings.Contains(err.Error(), "--broadcast-shutdown-timeout must be >= 0") {
		t.Errorf("error = %q, want substring '--broadcast-shutdown-timeout must be >= 0'", err)
	}
}

func TestParseBackendSelectorEmptySelector(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend-selector=myns//:8080",
	})
	if err == nil {
		t.Fatal("expected error for empty selector expression")
	}
	if !strings.Contains(err.Error(), "empty selector") {
		t.Errorf("error = %q, want substring 'empty selector'", err)
	}
}

func TestParseInvalidSecretsFormat(t *testing.T) {
	t.Parallel()
	vcl := makeTempVCL(t)
	_, err := Parse("", []string{
		"test",
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--secrets=no-colon",
	})
	if err == nil {
		t.Fatal("expected error for secrets without colon separator")
	}
}
