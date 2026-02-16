package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// BackendSpec describes one upstream backend service to watch.
type BackendSpec struct {
	Name        string // template key (e.g. "api")
	ServiceName string // Kubernetes Service name
	Port        string // numeric port, named port, or "" = first EndpointSlice port
	Namespace   string // Kubernetes namespace (resolved from [namespace/]service or --namespace)
}

// backendFlags implements flag.Value for repeatable --backend flags.
type backendFlags []BackendSpec

func (b *backendFlags) String() string { return fmt.Sprintf("%v", *b) }

func (b *backendFlags) Set(val string) error {
	parts := strings.SplitN(val, ":", 3)
	if len(parts) < 2 {
		return fmt.Errorf("invalid --backend %q: expected name:service-name[:port]", val)
	}
	spec := BackendSpec{
		Name:        parts[0],
		ServiceName: parts[1],
	}
	if len(parts) == 3 {
		if parts[2] == "" {
			return fmt.Errorf("empty port in --backend %q", val)
		}
		// If it looks numeric, validate the range.
		if p, err := strconv.ParseInt(parts[2], 10, 32); err == nil {
			if p <= 0 || p > 65535 {
				return fmt.Errorf("port out of range in --backend %q", val)
			}
		}
		spec.Port = parts[2]
	}
	*b = append(*b, spec)
	return nil
}

type Config struct {
	ServiceName      string
	ServiceNamespace string // resolved namespace for the frontend service
	Namespace        string
	VCLTemplate    string
	AdminAddr      string
	ListenAddr     string
	VarnishdPath   string
	VarnishadmPath string
	SecretPath     string
	Debounce        time.Duration
	ShutdownTimeout time.Duration
	Backends        []BackendSpec
	ExtraVarnishd  []string // Additional args passed to varnishd (after --)
}

// parseNamespacedService splits an optional "namespace/service" string into its
// components. When no "/" is present, defaultNS is used as the namespace.
func parseNamespacedService(s, defaultNS string) (namespace, service string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("empty service reference")
	}
	idx := strings.IndexByte(s, '/')
	if idx < 0 {
		return defaultNS, s, nil
	}
	namespace = s[:idx]
	service = s[idx+1:]
	if namespace == "" {
		return "", "", fmt.Errorf("empty namespace in %q", s)
	}
	if service == "" {
		return "", "", fmt.Errorf("empty service name in %q", s)
	}
	return namespace, service, nil
}

func Parse() (*Config, error) {
	c := &Config{}

	var backends backendFlags

	flag.StringVar(&c.ServiceName, "service-name", "", "Kubernetes Service to watch: [namespace/]service (required)")
	flag.StringVar(&c.Namespace, "namespace", "", "Kubernetes namespace (required, used as default for services without a namespace/ prefix)")
	flag.StringVar(&c.VCLTemplate, "vcl-template", "", "Path to VCL Go template file (required)")
	flag.StringVar(&c.AdminAddr, "admin-addr", "127.0.0.1:6082", "Varnish admin listen address")
	flag.StringVar(&c.ListenAddr, "listen-addr", "http=:8080,HTTP", "Varnish client listen address")
	flag.StringVar(&c.VarnishdPath, "varnishd-path", "varnishd", "Path to varnishd binary")
	flag.StringVar(&c.VarnishadmPath, "varnishadm-path", "varnishadm", "Path to varnishadm binary")
	flag.StringVar(&c.SecretPath, "secret-path", "", "Path to write the varnishadm secret file (default: auto-generated temp file)")
	flag.DurationVar(&c.Debounce, "debounce", 2*time.Second, "Debounce duration for endpoint changes")
	flag.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "Time to wait for varnishd to exit before sending SIGKILL")
	flag.Var(&backends, "backend", "Backend service: name:[namespace/]service[:port|:port-name] (repeatable)")

	flag.Parse()

	if c.ServiceName == "" {
		return nil, fmt.Errorf("--service-name is required")
	}
	if c.Namespace == "" {
		return nil, fmt.Errorf("--namespace is required")
	}
	if c.VCLTemplate == "" {
		return nil, fmt.Errorf("--vcl-template is required")
	}

	if _, err := os.Stat(c.VCLTemplate); err != nil {
		return nil, fmt.Errorf("vcl-template file %q: %w", c.VCLTemplate, err)
	}

	// Resolve frontend service namespace.
	ns, svc, err := parseNamespacedService(c.ServiceName, c.Namespace)
	if err != nil {
		return nil, fmt.Errorf("--service-name: %w", err)
	}
	c.ServiceNamespace = ns
	c.ServiceName = svc

	// Validate backend name uniqueness and resolve namespaces.
	seen := make(map[string]bool, len(backends))
	for i, b := range backends {
		if seen[b.Name] {
			return nil, fmt.Errorf("duplicate --backend name %q", b.Name)
		}
		seen[b.Name] = true

		ns, svc, err := parseNamespacedService(b.ServiceName, c.Namespace)
		if err != nil {
			return nil, fmt.Errorf("--backend %q: %w", b.Name, err)
		}
		backends[i].Namespace = ns
		backends[i].ServiceName = svc
	}
	c.Backends = []BackendSpec(backends)

	c.ExtraVarnishd = flag.Args()

	return c, nil
}
