package config

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestListenAddrFlagsString(t *testing.T) {
	var lf listenAddrFlags
	_ = lf.Set("http=:8080,HTTP")
	s := lf.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestBackendFlagsString(t *testing.T) {
	var bf backendFlags
	_ = bf.Set("api:my-svc:3000")
	s := bf.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestListenAddrPortMinBoundary(t *testing.T) {
	var lf listenAddrFlags
	if err := lf.Set("http=:1"); err != nil {
		t.Fatalf("unexpected error for port 1: %v", err)
	}
	if lf[0].Port != 1 {
		t.Errorf("Port = %d, want 1", lf[0].Port)
	}
}

func TestListenAddrTrailingComma(t *testing.T) {
	// Trailing comma (empty proto string after comma) should still parse the address.
	var lf listenAddrFlags
	if err := lf.Set("http=:8080,"); err != nil {
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
	// Multiple '=' signs: only the first splits name from address.
	// "a=b=:8080" → name="a", rest="b=:8080", SplitHostPort("b=:8080") → host="b=", port=8080
	var lf listenAddrFlags
	if err := lf.Set("a=b=:8080"); err != nil {
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
	var lf listenAddrFlags
	if err := lf.Set("http=:-1"); err == nil {
		t.Fatal("expected error for negative port")
	}
}

func TestListenAddrPortBoundary(t *testing.T) {
	var lf listenAddrFlags
	if err := lf.Set("http=:65535"); err != nil {
		t.Fatalf("unexpected error for port 65535: %v", err)
	}
	if lf[0].Port != 65535 {
		t.Errorf("Port = %d, want 65535", lf[0].Port)
	}
}

func TestListenAddrPortOverBoundary(t *testing.T) {
	var lf listenAddrFlags
	if err := lf.Set("http=:65536"); err == nil {
		t.Fatal("expected error for port 65536")
	}
}

func TestListenAddrMultipleAccumulate(t *testing.T) {
	var lf listenAddrFlags
	if err := lf.Set("http=:8080,HTTP"); err != nil {
		t.Fatal(err)
	}
	if err := lf.Set("purge=:8081"); err != nil {
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
	var bf backendFlags
	if err := bf.Set("api:my-svc:1"); err != nil {
		t.Fatalf("unexpected error for port 1: %v", err)
	}
	if bf[0].Port != "1" {
		t.Errorf("Port = %q, want 1", bf[0].Port)
	}
}

func TestBackendNegativePort(t *testing.T) {
	var bf backendFlags
	if err := bf.Set("api:my-svc:-1"); err == nil {
		t.Fatal("expected error for negative port")
	}
}

func TestBackendPortBoundary(t *testing.T) {
	var bf backendFlags
	if err := bf.Set("api:my-svc:65535"); err != nil {
		t.Fatalf("unexpected error for port 65535: %v", err)
	}
	if bf[0].Port != "65535" {
		t.Errorf("Port = %q, want 65535", bf[0].Port)
	}
}

func TestBackendPortOverBoundary(t *testing.T) {
	var bf backendFlags
	if err := bf.Set("api:my-svc:65536"); err == nil {
		t.Fatal("expected error for port 65536")
	}
}

func TestBackendMultipleAccumulate(t *testing.T) {
	var bf backendFlags
	if err := bf.Set("api:api-svc:8080"); err != nil {
		t.Fatal(err)
	}
	if err := bf.Set("web:web-svc:http"); err != nil {
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

// setupParse resets the global flag.CommandLine and os.Args for testing Parse().
// It returns a cleanup function that restores the originals.
func setupParse(t *testing.T, args []string) {
	t.Helper()
	origArgs := os.Args
	origCommandLine := flag.CommandLine
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origCommandLine
	})
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	os.Args = append([]string{"test"}, args...)
}

// makeTempVCL creates a temporary VCL template file and returns its path.
func makeTempVCL(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "vcl-*.vcl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("vcl 4.0;")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

func TestParseMissingServiceName(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for missing --service-name")
	}
	if !strings.Contains(err.Error(), "--service-name is required") {
		t.Errorf("error = %q, want substring '--service-name is required'", err)
	}
}

func TestParseMissingNamespace(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--vcl-template=" + vcl,
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for missing --namespace")
	}
	if !strings.Contains(err.Error(), "--namespace is required") {
		t.Errorf("error = %q, want substring '--namespace is required'", err)
	}
}

func TestParseMissingVCLTemplate(t *testing.T) {
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for missing --vcl-template")
	}
	if !strings.Contains(err.Error(), "--vcl-template is required") {
		t.Errorf("error = %q, want substring '--vcl-template is required'", err)
	}
}

func TestParseVCLTemplateNotExist(t *testing.T) {
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=/nonexistent/path/to/file.vcl",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for non-existent vcl-template")
	}
	if !strings.Contains(err.Error(), "vcl-template file") {
		t.Errorf("error = %q, want substring 'vcl-template file'", err)
	}
}

func TestParseMinimalValid(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=staging/my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=/my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for empty namespace prefix in --service-name")
	}
	if !strings.Contains(err.Error(), "--service-name") {
		t.Errorf("error = %q, want substring '--service-name'", err)
	}
}

func TestParseDuplicateListenAddrNames(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080",
		"--listen-addr=http=:9090",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for duplicate --listen-addr name")
	}
	if !strings.Contains(err.Error(), "duplicate --listen-addr name") {
		t.Errorf("error = %q, want substring 'duplicate --listen-addr name'", err)
	}
}

func TestParseUnnamedListenAddrsNoDuplicateError(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=:8080",
		"--listen-addr=:9090",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ListenAddrs) != 2 {
		t.Fatalf("expected 2 listen addrs, got %d", len(cfg.ListenAddrs))
	}
}

func TestParseBroadcastTargetListenAddr(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080,HTTP",
		"--listen-addr=purge=:8081",
		"--broadcast-target-listen-addr=purge",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BroadcastTargetPort != 8081 {
		t.Errorf("BroadcastTargetPort = %d, want 8081", cfg.BroadcastTargetPort)
	}
}

func TestParseBroadcastTargetListenAddrNotFound(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080",
		"--broadcast-target-listen-addr=nonexistent",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for unknown --broadcast-target-listen-addr")
	}
	if !strings.Contains(err.Error(), "does not match any --listen-addr name") {
		t.Errorf("error = %q, want substring 'does not match any --listen-addr name'", err)
	}
}

func TestParseBroadcastDisabled(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-addr=",
	})
	cfg, err := Parse()
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

func TestParseDuplicateBackendNames(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc",
		"--backend=api:other-svc",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for duplicate --backend name")
	}
	if !strings.Contains(err.Error(), "duplicate --backend name") {
		t.Errorf("error = %q, want substring 'duplicate --backend name'", err)
	}
}

func TestParseBackendNamespaceResolution(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc",
		"--backend=web:staging/web-svc:http",
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:/svc",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for invalid backend service reference")
	}
	if !strings.Contains(err.Error(), "--backend") {
		t.Errorf("error = %q, want substring '--backend'", err)
	}
}

func TestParseDefaults(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AdminAddr != "127.0.0.1:6082" {
		t.Errorf("AdminAddr = %q, want 127.0.0.1:6082", cfg.AdminAddr)
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
}

func TestParseExtraVarnishdArgs(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--", "-p", "default_ttl=3600", "-p", "default_grace=3600",
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080,HTTP",
		"--listen-addr=purge=:8081",
	})
	cfg, err := Parse()
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
	dir := t.TempDir()
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + dir,
	})
	// os.Stat succeeds for directories, but this exercises the path where
	// the "file" exists. Parse() doesn't distinguish files from dirs —
	// the template parser in renderer.New() would fail later. So Parse()
	// should succeed here.
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLTemplate != dir {
		t.Errorf("VCLTemplate = %q, want %q", cfg.VCLTemplate, dir)
	}
}

func TestParseVCLTemplateRelativePath(t *testing.T) {
	// Create a temp file in the current working directory.
	f, err := os.CreateTemp(".", "vcl-test-*.vcl")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	relPath := filepath.Base(f.Name())
	t.Cleanup(func() { _ = os.Remove(relPath) })

	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + relPath,
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.VCLTemplate != relPath {
		t.Errorf("VCLTemplate = %q, want %q", cfg.VCLTemplate, relPath)
	}
}

func TestParseOverrideStringFlags(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--admin-addr=0.0.0.0:6082",
		"--varnishd-path=/usr/sbin/varnishd",
		"--varnishadm-path=/usr/bin/varnishadm",
		"--secret-path=/etc/varnish/secret",
		"--broadcast-addr=:9999",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AdminAddr != "0.0.0.0:6082" {
		t.Errorf("AdminAddr = %q, want 0.0.0.0:6082", cfg.AdminAddr)
	}
	if cfg.VarnishdPath != "/usr/sbin/varnishd" {
		t.Errorf("VarnishdPath = %q, want /usr/sbin/varnishd", cfg.VarnishdPath)
	}
	if cfg.VarnishadmPath != "/usr/bin/varnishadm" {
		t.Errorf("VarnishadmPath = %q, want /usr/bin/varnishadm", cfg.VarnishadmPath)
	}
	if cfg.SecretPath != "/etc/varnish/secret" {
		t.Errorf("SecretPath = %q, want /etc/varnish/secret", cfg.SecretPath)
	}
	if cfg.BroadcastAddr != ":9999" {
		t.Errorf("BroadcastAddr = %q, want :9999", cfg.BroadcastAddr)
	}
}

func TestParseOverrideDurationFlags(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=5s",
		"--shutdown-timeout=60s",
		"--broadcast-drain-timeout=45s",
		"--broadcast-shutdown-timeout=10s",
		"--broadcast-server-idle-timeout=180s",
		"--broadcast-read-header-timeout=15s",
		"--broadcast-client-idle-timeout=8s",
		"--broadcast-client-timeout=6s",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"Debounce", cfg.Debounce, 5 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 60 * time.Second},
		{"BroadcastDrainTimeout", cfg.BroadcastDrainTimeout, 45 * time.Second},
		{"BroadcastShutdownTimeout", cfg.BroadcastShutdownTimeout, 10 * time.Second},
		{"BroadcastServerIdleTimeout", cfg.BroadcastServerIdleTimeout, 180 * time.Second},
		{"BroadcastReadHeaderTimeout", cfg.BroadcastReadHeaderTimeout, 15 * time.Second},
		{"BroadcastClientIdleTimeout", cfg.BroadcastClientIdleTimeout, 8 * time.Second},
		{"BroadcastClientTimeout", cfg.BroadcastClientTimeout, 6 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
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
			vcl := makeTempVCL(t)
			setupParse(t, []string{
				"--service-name=my-svc",
				"--namespace=default",
				"--vcl-template=" + vcl,
				"--log-level=" + tt.level,
			})
			cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
}

func TestParseNoBackends(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 0 {
		t.Errorf("Backends = %v, want empty", cfg.Backends)
	}
}

func TestParseNoExtraVarnishdArgs(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ExtraVarnishd) != 0 {
		t.Errorf("ExtraVarnishd = %v, want empty", cfg.ExtraVarnishd)
	}
}

func TestParseBackendTrailingSlash(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:ns/",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for backend with trailing slash (empty service)")
	}
	if !strings.Contains(err.Error(), "--backend") {
		t.Errorf("error = %q, want substring '--backend'", err)
	}
}

func TestParseBroadcastTargetMatchesUnnamedListenAddr(t *testing.T) {
	vcl := makeTempVCL(t)
	// Unnamed listen addrs have Name="". Setting --broadcast-target-listen-addr
	// to a value that doesn't match any named addr should fail.
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=:8080",
		"--broadcast-target-listen-addr=default",
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error when broadcast target doesn't match any named listen addr")
	}
	if !strings.Contains(err.Error(), "does not match any --listen-addr name") {
		t.Errorf("error = %q, want substring 'does not match any --listen-addr name'", err)
	}
}

func TestParseBroadcastTargetDefaultsToFirstListenAddr(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=:9090",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With no broadcast-target-listen-addr, the first listen addr port is used.
	if cfg.BroadcastTargetPort != 9090 {
		t.Errorf("BroadcastTargetPort = %d, want 9090", cfg.BroadcastTargetPort)
	}
}

func TestParseListenAddrIPv6AllInterfaces(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=[::]:8080,HTTP",
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=my-http.v2=:8080",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddrs[0].Name != "my-http.v2" {
		t.Errorf("Name = %q, want my-http.v2", cfg.ListenAddrs[0].Name)
	}
}

func TestParseDefaultDurations(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"Debounce", cfg.Debounce, 2 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 30 * time.Second},
		{"BroadcastDrainTimeout", cfg.BroadcastDrainTimeout, 30 * time.Second},
		{"BroadcastShutdownTimeout", cfg.BroadcastShutdownTimeout, 5 * time.Second},
		{"BroadcastServerIdleTimeout", cfg.BroadcastServerIdleTimeout, 120 * time.Second},
		{"BroadcastReadHeaderTimeout", cfg.BroadcastReadHeaderTimeout, 10 * time.Second},
		{"BroadcastClientIdleTimeout", cfg.BroadcastClientIdleTimeout, 4 * time.Second},
		{"BroadcastClientTimeout", cfg.BroadcastClientTimeout, 3 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseSecretPathDefault(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SecretPath != "" {
		t.Errorf("SecretPath = %q, want empty (auto-generated)", cfg.SecretPath)
	}
}

func TestParseBackendWithNumericPort(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc:3000",
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=ns/",
		"--namespace=default",
		"--vcl-template=" + vcl,
	})
	_, err := Parse()
	if err == nil {
		t.Fatal("expected error for --service-name=ns/ (empty service)")
	}
	if !strings.Contains(err.Error(), "--service-name") {
		t.Errorf("error = %q, want substring '--service-name'", err)
	}
}

func TestParseBroadcastDisabledSkipsTargetResolution(t *testing.T) {
	vcl := makeTempVCL(t)
	// With broadcast disabled, --broadcast-target-listen-addr should be ignored
	// even if it references a nonexistent name.
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--broadcast-addr=",
		"--broadcast-target-listen-addr=nonexistent",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BroadcastTargetPort != 0 {
		t.Errorf("BroadcastTargetPort = %d, want 0", cfg.BroadcastTargetPort)
	}
}

func TestParseBroadcastTargetMatchesFirstNamedAddr(t *testing.T) {
	// When --broadcast-target-listen-addr explicitly names the first listen addr,
	// the code takes the loop path (lines 222-228) rather than the shortcut (line 219).
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--listen-addr=http=:8080,HTTP",
		"--listen-addr=purge=:8081",
		"--broadcast-target-listen-addr=http",
	})
	cfg, err := Parse()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BroadcastTargetPort != 8080 {
		t.Errorf("BroadcastTargetPort = %d, want 8080", cfg.BroadcastTargetPort)
	}
}

func TestParseDurationZero(t *testing.T) {
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=0s",
		"--shutdown-timeout=0s",
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--debounce=100ms",
		"--broadcast-client-timeout=500ms",
	})
	cfg, err := Parse()
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
	vcl := makeTempVCL(t)
	setupParse(t, []string{
		"--service-name=my-svc",
		"--namespace=default",
		"--vcl-template=" + vcl,
		"--backend=api:api-svc:8080",
		"--backend=web:web-svc:http",
		"--backend=grpc:other-ns/grpc-svc:9090",
	})
	cfg, err := Parse()
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
