// Package config parses and validates command-line flags for k8s-httpcache.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
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

// ListenAddrSpec describes a Varnish listen address parsed from the
// varnishd -a flag format: [name=][bind-ip]:port[,proto[,proto...]].
type ListenAddrSpec struct {
	Name string // optional name (e.g. "http")
	Host string // bind IP (e.g. "0.0.0.0", "127.0.0.1", or "" for all interfaces)
	Port int32  // numeric port extracted from the address
	Raw  string // original flag value passed through to varnishd
}

// listenAddrFlags implements flag.Value for repeatable --listen-addr flags.
type listenAddrFlags []ListenAddrSpec

func (l *listenAddrFlags) String() string { return fmt.Sprintf("%v", *l) }

func (l *listenAddrFlags) Set(val string) error {
	spec := ListenAddrSpec{Raw: val}

	rest := val
	if name, after, ok := strings.Cut(val, "="); ok {
		spec.Name = name
		if spec.Name == "" {
			return fmt.Errorf("empty name in --listen-addr %q", val)
		}
		rest = after
	}

	// Strip protocol suffixes: ":8080,HTTP" → ":8080"
	addr, _, _ := strings.Cut(rest, ",")

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address in --listen-addr %q: %w", val, err)
	}
	spec.Host = host
	p, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("invalid port in --listen-addr %q", val)
	}
	spec.Port = int32(p)

	*l = append(*l, spec)
	return nil
}

// backendFlags implements flag.Value for repeatable --backend flags.
type backendFlags []BackendSpec

func (b *backendFlags) String() string { return fmt.Sprintf("%v", *b) }

func (b *backendFlags) Set(val string) error {
	parts := strings.SplitN(val, ":", 3)
	if len(parts) < 2 {
		return fmt.Errorf("invalid --backend %q: expected name:service-name[:port]", val)
	}
	if parts[0] == "" {
		return fmt.Errorf("empty name in --backend %q", val)
	}
	if parts[1] == "" {
		return fmt.Errorf("empty service in --backend %q", val)
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

// Config holds the parsed configuration for the k8s-httpcache process.
type Config struct {
	ServiceName                string
	ServiceNamespace           string // resolved namespace for the frontend service
	Namespace                  string
	VCLTemplate                string
	AdminAddr                  string
	ListenAddrs                []ListenAddrSpec
	VarnishdPath               string
	VarnishadmPath             string
	SecretPath                 string
	BroadcastAddr              string
	BroadcastTargetListenAddr  string
	BroadcastTargetPort        int32 // resolved from BroadcastTargetListenAddr
	BroadcastDrainTimeout      time.Duration
	BroadcastShutdownTimeout   time.Duration
	BroadcastServerIdleTimeout time.Duration
	BroadcastReadHeaderTimeout time.Duration
	BroadcastClientIdleTimeout time.Duration
	BroadcastClientTimeout     time.Duration
	Debounce                   time.Duration
	ShutdownTimeout            time.Duration
	Backends                   []BackendSpec
	MetricsAddr                string
	ExtraVarnishd              []string // Additional args passed to varnishd (after --)
	LogLevel                   slog.Level
	LogFormat                  string // "text" or "json"
}

// isValidDNSLabel checks whether s is a valid RFC 1123 DNS label:
// 1-63 lowercase alphanumeric characters or hyphens, starting and ending
// with an alphanumeric character. Kubernetes uses this for Service and
// Namespace names.
func isValidDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, c := range s {
		isAlnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		isHyphen := c == '-'
		if !isAlnum && !isHyphen {
			return false
		}
		if isHyphen && (i == 0 || i == len(s)-1) {
			return false
		}
	}
	return true
}

// parseNamespacedService splits an optional "namespace/service" string into its
// components. When no "/" is present, defaultNS is used as the namespace.
// Both namespace and service must be valid RFC 1123 DNS labels.
func parseNamespacedService(s, defaultNS string) (namespace, service string, err error) {
	if s == "" {
		return "", "", errors.New("empty service reference")
	}
	namespace, service, hasSep := strings.Cut(s, "/")
	if !hasSep {
		if !isValidDNSLabel(s) {
			return "", "", fmt.Errorf("invalid service name %q: must be a valid RFC 1123 DNS label", s)
		}
		return defaultNS, s, nil
	}
	if namespace == "" {
		return "", "", fmt.Errorf("empty namespace in %q", s)
	}
	if service == "" {
		return "", "", fmt.Errorf("empty service name in %q", s)
	}
	if !isValidDNSLabel(namespace) {
		return "", "", fmt.Errorf("invalid namespace %q in %q: must be a valid RFC 1123 DNS label", namespace, s)
	}
	if !isValidDNSLabel(service) {
		return "", "", fmt.Errorf("invalid service name %q in %q: must be a valid RFC 1123 DNS label", service, s)
	}
	return namespace, service, nil
}

// Parse parses command-line flags and returns a validated Config.
func Parse() (*Config, error) {
	c := &Config{}

	var (
		backends    backendFlags
		listenAddrs listenAddrFlags
	)

	flag.StringVar(&c.ServiceName, "service-name", "", "Kubernetes Service to watch: [namespace/]service (required)")
	flag.StringVar(&c.Namespace, "namespace", "", "Kubernetes namespace (required, used as default for services without a namespace/ prefix)")
	flag.StringVar(&c.VCLTemplate, "vcl-template", "", "Path to VCL Go template file (required)")
	flag.StringVar(&c.AdminAddr, "admin-addr", "127.0.0.1:6082", "Varnish admin listen address")
	flag.Var(&listenAddrs, "listen-addr", "Varnish listen address: [name=]address[,proto] (repeatable, default: http=:8080,HTTP)")
	flag.StringVar(&c.VarnishdPath, "varnishd-path", "varnishd", "Path to varnishd binary")
	flag.StringVar(&c.VarnishadmPath, "varnishadm-path", "varnishadm", "Path to varnishadm binary")
	flag.StringVar(&c.SecretPath, "secret-path", "", "Path to write the varnishadm secret file (default: auto-generated temp file)")
	flag.DurationVar(&c.Debounce, "debounce", 2*time.Second, "Debounce duration for endpoint changes")
	flag.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "Time to wait for varnishd to exit before sending SIGKILL")
	flag.StringVar(&c.BroadcastAddr, "broadcast-addr", ":8088", "Listen address for the broadcast HTTP server (set empty to disable)")
	flag.StringVar(&c.BroadcastTargetListenAddr, "broadcast-target-listen-addr", "", "Name of the --listen-addr to target for fan-out (default: first --listen-addr)")
	flag.DurationVar(&c.BroadcastDrainTimeout, "broadcast-drain-timeout", 30*time.Second, "Time to wait for broadcast connections to drain before shutting down. This should ideally be the idle timeout of the clients calling the broadcast endpoint.")
	flag.DurationVar(&c.BroadcastShutdownTimeout, "broadcast-shutdown-timeout", 5*time.Second, "Time to wait for in-flight broadcast requests to finish after draining")
	flag.DurationVar(&c.BroadcastServerIdleTimeout, "broadcast-server-idle-timeout", 120*time.Second, "Max time a client keep-alive connection to the broadcast server can stay idle. This should ideally be greater than the idle timeout of the clients calling the broadcast endpoint.")
	flag.DurationVar(&c.BroadcastReadHeaderTimeout, "broadcast-read-header-timeout", 10*time.Second, "Max time to read request headers on the broadcast server")
	flag.DurationVar(&c.BroadcastClientIdleTimeout, "broadcast-client-idle-timeout", 4*time.Second, "Max time an idle connection to a Varnish pod is kept in the broadcast client pool. This should ideally be lower than the Varnish timeout_idle parameter.")
	flag.DurationVar(&c.BroadcastClientTimeout, "broadcast-client-timeout", 3*time.Second, "Timeout for each fan-out request to a Varnish pod")
	flag.Var(&backends, "backend", "Backend service: name:[namespace/]service[:port|:port-name] (repeatable)")
	flag.TextVar(&c.LogLevel, "log-level", slog.LevelInfo, "Log level (DEBUG, INFO, WARN, ERROR)")
	flag.StringVar(&c.LogFormat, "log-format", "text", "Log format (text, json)")
	flag.StringVar(&c.MetricsAddr, "metrics-addr", ":9101", "Listen address for Prometheus metrics (set empty to disable)")

	flag.Parse()

	switch c.LogFormat {
	case "text", "json":
	default:
		return nil, fmt.Errorf("--log-format must be \"text\" or \"json\", got %q", c.LogFormat)
	}

	if c.ServiceName == "" {
		return nil, errors.New("--service-name is required")
	}
	if c.Namespace == "" {
		return nil, errors.New("--namespace is required")
	}
	if c.VCLTemplate == "" {
		return nil, errors.New("--vcl-template is required")
	}

	if _, err := os.Stat(c.VCLTemplate); err != nil {
		return nil, fmt.Errorf("vcl-template file %q: %w", c.VCLTemplate, err)
	}

	// Default listen address if none provided.
	if len(listenAddrs) == 0 {
		if err := listenAddrs.Set("http=:8080,HTTP"); err != nil {
			return nil, fmt.Errorf("default --listen-addr: %w", err)
		}
	}

	// Validate listen address name uniqueness.
	seenLA := make(map[string]bool, len(listenAddrs))
	for _, la := range listenAddrs {
		if la.Name != "" {
			if seenLA[la.Name] {
				return nil, fmt.Errorf("duplicate --listen-addr name %q", la.Name)
			}
			seenLA[la.Name] = true
		}
	}
	c.ListenAddrs = []ListenAddrSpec(listenAddrs)

	// Resolve broadcast target port from the named listen address.
	if c.BroadcastAddr != "" {
		if c.BroadcastTargetListenAddr == "" {
			c.BroadcastTargetPort = c.ListenAddrs[0].Port
		} else {
			found := false
			for _, la := range c.ListenAddrs {
				if la.Name == c.BroadcastTargetListenAddr {
					c.BroadcastTargetPort = la.Port
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("--broadcast-target-listen-addr %q does not match any --listen-addr name", c.BroadcastTargetListenAddr)
			}
		}
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
