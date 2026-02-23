// Package config parses and validates command-line flags for k8s-httpcache.
package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	cli "github.com/urfave/cli/v3"
)

// BackendSpec describes one upstream backend service to watch.
type BackendSpec struct {
	Name        string // template key (e.g. "api")
	ServiceName string // Kubernetes Service name
	Port        string // numeric port, named port, or "" = first EndpointSlice port
	Namespace   string // Kubernetes namespace (resolved from [namespace/]service or --namespace)
}

// ValuesSpec describes one ConfigMap to watch and expose as template values.
type ValuesSpec struct {
	Name          string // template key, accessible as .Values.<Name>.<key>
	ConfigMapName string // Kubernetes ConfigMap name
	Namespace     string // Kubernetes namespace (resolved from [namespace/]configmap or --namespace)
}

// ValuesDirSpec describes a filesystem directory to poll for YAML values.
type ValuesDirSpec struct {
	Name string // template key, accessible as .Values.<Name>.<key>
	Path string // filesystem directory path
}

// ListenAddrSpec describes a Varnish listen address parsed from the
// varnishd -a flag format: [name=][bind-ip]:port[,proto[,proto...]].
type ListenAddrSpec struct {
	Name string // optional name (e.g. "http")
	Host string // bind IP (e.g. "0.0.0.0", "127.0.0.1", or "" for all interfaces)
	Port int32  // numeric port extracted from the address
	Raw  string // original flag value passed through to varnishd
}

// listenAddrFlags implements a parser for repeatable --listen-addr flags.
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

// valuesFlags implements a parser for repeatable --values flags.
type valuesFlags []ValuesSpec

func (v *valuesFlags) String() string { return fmt.Sprintf("%v", *v) }

func (v *valuesFlags) Set(val string) error {
	name, ref, ok := strings.Cut(val, ":")
	if !ok {
		return fmt.Errorf("invalid --values %q: expected name:[namespace/]configmap", val)
	}
	if name == "" {
		return fmt.Errorf("empty name in --values %q", val)
	}
	if ref == "" {
		return fmt.Errorf("empty configmap in --values %q", val)
	}
	*v = append(*v, ValuesSpec{
		Name:          name,
		ConfigMapName: ref, // namespace resolution happens in Parse()
	})
	return nil
}

// valuesDirFlags implements a parser for repeatable --values-dir flags.
type valuesDirFlags []ValuesDirSpec

func (v *valuesDirFlags) String() string { return fmt.Sprintf("%v", *v) }

func (v *valuesDirFlags) Set(val string) error {
	name, path, ok := strings.Cut(val, ":")
	if !ok {
		return fmt.Errorf("invalid --values-dir %q: expected name:/path/to/dir", val)
	}
	if name == "" {
		return fmt.Errorf("empty name in --values-dir %q", val)
	}
	if path == "" {
		return fmt.Errorf("empty path in --values-dir %q", val)
	}
	*v = append(*v, ValuesDirSpec{
		Name: name,
		Path: path,
	})
	return nil
}

// backendFlags implements a parser for repeatable --backend flags.
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
	ListenAddrs                []ListenAddrSpec
	VarnishdPath               string
	VarnishadmPath             string
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
	DebounceMax                time.Duration
	ShutdownTimeout            time.Duration
	Backends                   []BackendSpec
	Values                     []ValuesSpec
	ValuesDirs                 []ValuesDirSpec
	ValuesDirPollInterval      time.Duration
	MetricsAddr                string
	MetricsReadHeaderTimeout   time.Duration
	ExtraVarnishd              []string // Additional args passed to varnishd (after --)
	LogLevel                   slog.Level
	LogFormat                  string // "text" or "json"
	AdminTimeout               time.Duration
	Drain                      bool
	DrainDelay                 time.Duration
	DrainPollInterval          time.Duration
	DrainTimeout               time.Duration
	VCLTemplateWatchInterval   time.Duration
	FileWatch                  bool
	VarnishstatPath            string
	TemplateDelimLeft          string
	TemplateDelimRight         string
	VCLReloadRetries           int
	VCLReloadRetryInterval     time.Duration
	VCLKept                    int
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

// ErrHelp is returned by Parse when --help is shown. The caller should
// treat this as a successful exit (exit code 0).
var ErrHelp = errors.New("help requested")

// validationError is returned from the Action for validation failures.
// It prints the error message and help before Parse returns, so callers
// can rely on output already being written.
func validationError(cmd *cli.Command, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	_, _ = fmt.Fprintf(cmd.Root().ErrWriter, "error: %v\n\n", err)
	_ = cli.ShowRootCommandHelp(cmd)
	return err
}

// Parse parses command-line flags from args and returns a validated Config.
// The first element of args should be the program name (i.e. os.Args).
// Returns (nil, nil) when --help is shown.
func Parse(args []string) (*Config, error) {
	c := &Config{}

	var (
		rawBackends    []string
		rawListenAddrs []string
		rawValues      []string
		rawValuesDirs  []string
		templateDelims string
		logLevel       string
		parsed         bool
		actionErr      error
	)

	cmd := &cli.Command{
		Name:                      "k8s-httpcache",
		Usage:                     "Kubernetes-native HTTP caching proxy built on Varnish",
		UsageText:                 "k8s-httpcache [flags] [-- varnishd-args...]",
		DisableSliceFlagSeparator: true,
		Flags: []cli.Flag{
			// Required
			&cli.StringFlag{
				Name:        "service-name",
				Aliases:     []string{"s"},
				Usage:       "Kubernetes Service to watch: [namespace/]service",
				Destination: &c.ServiceName,
			},
			&cli.StringFlag{
				Name:        "namespace",
				Aliases:     []string{"n"},
				Usage:       "Kubernetes namespace (used as default for services without a namespace/ prefix)",
				Destination: &c.Namespace,
			},
			&cli.StringFlag{
				Name:        "vcl-template",
				Aliases:     []string{"t"},
				Usage:       "Path to VCL Go template file",
				Destination: &c.VCLTemplate,
			},

			// Listen, backend, and values
			&cli.StringSliceFlag{
				Name:        "listen-addr",
				Aliases:     []string{"l"},
				Category:    "Listen, backend, and values:",
				Usage:       "Varnish listen address: [name=]address[,proto] (repeatable)",
				DefaultText: "http=:8080,HTTP",
				Destination: &rawListenAddrs,
			},
			&cli.StringSliceFlag{
				Name:        "backend",
				Aliases:     []string{"b"},
				Category:    "Listen, backend, and values:",
				Usage:       "Backend service: name:[namespace/]service[:port|:port-name] (repeatable)",
				Destination: &rawBackends,
			},
			&cli.StringSliceFlag{
				Name:        "values",
				Category:    "Listen, backend, and values:",
				Usage:       "ConfigMap to watch for template values: name:[namespace/]configmap (repeatable)",
				Destination: &rawValues,
			},
			&cli.StringSliceFlag{
				Name:        "values-dir",
				Category:    "Listen, backend, and values:",
				Usage:       "Directory to poll for YAML template values: name:/path/to/dir (repeatable)",
				Destination: &rawValuesDirs,
			},
			&cli.DurationFlag{
				Name:        "values-dir-poll-interval",
				Category:    "Listen, backend, and values:",
				Usage:       "Poll interval for --values-dir directories (only effective when --file-watch is enabled)",
				Value:       5 * time.Second,
				Destination: &c.ValuesDirPollInterval,
			},

			// Varnish paths
			&cli.StringFlag{
				Name:        "varnishd-path",
				Category:    "Varnish paths:",
				Usage:       "Path to varnishd binary",
				Value:       "varnishd",
				Destination: &c.VarnishdPath,
			},
			&cli.StringFlag{
				Name:        "varnishadm-path",
				Category:    "Varnish paths:",
				Usage:       "Path to varnishadm binary",
				Value:       "varnishadm",
				Destination: &c.VarnishadmPath,
			},
			&cli.StringFlag{
				Name:        "varnishstat-path",
				Category:    "Varnish paths:",
				Usage:       "Path to varnishstat binary",
				Value:       "varnishstat",
				Destination: &c.VarnishstatPath,
			},
			&cli.DurationFlag{
				Name:        "admin-timeout",
				Category:    "Varnish paths:",
				Usage:       "Max time to wait for the varnish admin CLI to become ready",
				Value:       30 * time.Second,
				Destination: &c.AdminTimeout,
			},

			// Broadcast
			&cli.StringFlag{
				Name:        "broadcast-addr",
				Category:    "Broadcast:",
				Usage:       `Listen address for the broadcast HTTP server ("none" to disable)`,
				Value:       ":8088",
				Destination: &c.BroadcastAddr,
			},
			&cli.StringFlag{
				Name:        "broadcast-target-listen-addr",
				Category:    "Broadcast:",
				Usage:       "Name of the --listen-addr to target for fan-out (default: first --listen-addr; only effective when broadcast is enabled)",
				Destination: &c.BroadcastTargetListenAddr,
			},
			&cli.DurationFlag{
				Name:        "broadcast-drain-timeout",
				Category:    "Broadcast:",
				Usage:       "Time to wait for broadcast connections to drain before shutting down (only effective when broadcast is enabled)",
				Value:       30 * time.Second,
				Destination: &c.BroadcastDrainTimeout,
			},
			&cli.DurationFlag{
				Name:        "broadcast-shutdown-timeout",
				Category:    "Broadcast:",
				Usage:       "Time to wait for in-flight broadcast requests to finish after draining (only effective when broadcast is enabled)",
				Value:       5 * time.Second,
				Destination: &c.BroadcastShutdownTimeout,
			},
			&cli.DurationFlag{
				Name:        "broadcast-server-idle-timeout",
				Category:    "Broadcast:",
				Usage:       "Max time a client keep-alive connection to the broadcast server can stay idle (only effective when broadcast is enabled)",
				Value:       120 * time.Second,
				Destination: &c.BroadcastServerIdleTimeout,
			},
			&cli.DurationFlag{
				Name:        "broadcast-read-header-timeout",
				Category:    "Broadcast:",
				Usage:       "Max time to read request headers on the broadcast server (only effective when broadcast is enabled)",
				Value:       10 * time.Second,
				Destination: &c.BroadcastReadHeaderTimeout,
			},
			&cli.DurationFlag{
				Name:        "broadcast-client-idle-timeout",
				Category:    "Broadcast:",
				Usage:       "Max time an idle connection to a Varnish pod is kept in the broadcast client pool (only effective when broadcast is enabled)",
				Value:       4 * time.Second,
				Destination: &c.BroadcastClientIdleTimeout,
			},
			&cli.DurationFlag{
				Name:        "broadcast-client-timeout",
				Category:    "Broadcast:",
				Usage:       "Timeout for each fan-out request to a Varnish pod (only effective when broadcast is enabled)",
				Value:       3 * time.Second,
				Destination: &c.BroadcastClientTimeout,
			},

			// Metrics
			&cli.StringFlag{
				Name:        "metrics-addr",
				Category:    "Metrics:",
				Usage:       `Listen address for Prometheus metrics ("none" to disable)`,
				Value:       ":9101",
				Destination: &c.MetricsAddr,
			},
			&cli.DurationFlag{
				Name:        "metrics-read-header-timeout",
				Category:    "Metrics:",
				Usage:       "Max time to read request headers on the metrics server",
				Value:       10 * time.Second,
				Destination: &c.MetricsReadHeaderTimeout,
			},

			// Drain
			&cli.BoolFlag{
				Name:        "drain",
				Category:    "Drain:",
				Usage:       "Enable graceful connection draining on shutdown",
				Destination: &c.Drain,
			},
			&cli.DurationFlag{
				Name:        "drain-delay",
				Category:    "Drain:",
				Usage:       "Delay after marking backend sick before polling for active sessions (only effective when --drain is enabled)",
				Value:       15 * time.Second,
				Destination: &c.DrainDelay,
			},
			&cli.DurationFlag{
				Name:        "drain-poll-interval",
				Category:    "Drain:",
				Usage:       "Poll interval for active sessions during graceful drain (only effective when --drain is enabled)",
				Value:       1 * time.Second,
				Destination: &c.DrainPollInterval,
			},
			&cli.DurationFlag{
				Name:        "drain-timeout",
				Category:    "Drain:",
				Usage:       "Max time to wait for active sessions to reach 0 (0 to skip session polling; only effective when --drain is enabled)",
				Destination: &c.DrainTimeout,
			},

			// Template
			&cli.StringFlag{
				Name:        "template-delims",
				Category:    "Template:",
				Usage:       `Template delimiters as a space-separated pair (e.g. "<< >>" or "{{ }}")`,
				Value:       "<< >>",
				Destination: &templateDelims,
			},

			// Timing and logging
			&cli.DurationFlag{
				Name:        "debounce",
				Category:    "Timing and logging:",
				Usage:       "Debounce duration for endpoint changes",
				Value:       2 * time.Second,
				Destination: &c.Debounce,
			},
			&cli.DurationFlag{
				Name:        "debounce-max",
				Category:    "Timing and logging:",
				Usage:       "Maximum debounce duration before a reload is forced (0 disables; only effective when events arrive within the --debounce window)",
				Destination: &c.DebounceMax,
			},
			&cli.DurationFlag{
				Name:        "shutdown-timeout",
				Category:    "Timing and logging:",
				Usage:       "Time to wait for varnishd to exit before sending SIGKILL",
				Value:       30 * time.Second,
				Destination: &c.ShutdownTimeout,
			},
			&cli.DurationFlag{
				Name:        "vcl-template-watch-interval",
				Category:    "Timing and logging:",
				Usage:       "Poll interval for VCL template file changes (only effective when --file-watch is enabled)",
				Value:       5 * time.Second,
				Destination: &c.VCLTemplateWatchInterval,
			},
			&cli.BoolFlag{
				Name:        "file-watch",
				Category:    "Timing and logging:",
				Usage:       "Watch VCL template and --values-dir paths for changes (disable with --file-watch=false)",
				Value:       true,
				Destination: &c.FileWatch,
			},
			&cli.IntFlag{
				Name:        "vcl-reload-retries",
				Category:    "Timing and logging:",
				Usage:       "Max retry attempts for vcl.load failures (0 disables retries)",
				Value:       3,
				Destination: &c.VCLReloadRetries,
			},
			&cli.DurationFlag{
				Name:        "vcl-reload-retry-interval",
				Category:    "Timing and logging:",
				Usage:       "Wait between vcl.load retry attempts",
				Value:       2 * time.Second,
				Destination: &c.VCLReloadRetryInterval,
			},
			&cli.IntFlag{
				Name:        "vcl-kept",
				Category:    "Timing and logging:",
				Usage:       "Number of old VCL objects to retain after reload (0 discards all)",
				Value:       0,
				Destination: &c.VCLKept,
			},
			&cli.StringFlag{
				Name:        "log-level",
				Category:    "Timing and logging:",
				Usage:       "Log level (DEBUG, INFO, WARN, ERROR)",
				Value:       "INFO",
				Destination: &logLevel,
			},
			&cli.StringFlag{
				Name:        "log-format",
				Category:    "Timing and logging:",
				Usage:       "Log format (text, json)",
				Value:       "text",
				Destination: &c.LogFormat,
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			parsed = true

			// Check required flags.
			if c.ServiceName == "" {
				actionErr = validationError(cmd, "--service-name is required")
				return nil
			}
			if c.Namespace == "" {
				actionErr = validationError(cmd, "--namespace is required")
				return nil
			}
			if c.VCLTemplate == "" {
				actionErr = validationError(cmd, "--vcl-template is required")
				return nil
			}

			// Parse template delimiters.
			parts := strings.Fields(templateDelims)
			if len(parts) != 2 {
				actionErr = validationError(cmd, "--template-delims must be exactly two tokens, got %d in %q", len(parts), templateDelims)
				return nil
			}
			c.TemplateDelimLeft = parts[0]
			c.TemplateDelimRight = parts[1]

			// Validate log level.
			if err := c.LogLevel.UnmarshalText([]byte(logLevel)); err != nil {
				actionErr = validationError(cmd, "--log-level: %v", err)
				return nil
			}

			// Validate log format.
			switch c.LogFormat {
			case "text", "json":
			default:
				actionErr = validationError(cmd, "--log-format must be \"text\" or \"json\", got %q", c.LogFormat)
				return nil
			}

			// Validate debounce-max.
			if c.DebounceMax < 0 {
				actionErr = validationError(cmd, "--debounce-max must be >= 0, got %v", c.DebounceMax)
				return nil
			}
			if c.DebounceMax > 0 && c.DebounceMax < c.Debounce {
				actionErr = validationError(cmd, "--debounce-max (%v) must be >= --debounce (%v)", c.DebounceMax, c.Debounce)
				return nil
			}

			// Validate VCL reload retries.
			if c.VCLReloadRetries < 0 {
				actionErr = validationError(cmd, "--vcl-reload-retries must be >= 0, got %d", c.VCLReloadRetries)
				return nil
			}
			if c.VCLReloadRetryInterval < 0 {
				actionErr = validationError(cmd, "--vcl-reload-retry-interval must be >= 0, got %v", c.VCLReloadRetryInterval)
				return nil
			}

			// Validate VCL kept.
			if c.VCLKept < 0 {
				actionErr = validationError(cmd, "--vcl-kept must be >= 0, got %d", c.VCLKept)
				return nil
			}

			// Validate VCL template file exists.
			if _, err := os.Stat(c.VCLTemplate); err != nil {
				actionErr = validationError(cmd, "vcl-template file %q: %v", c.VCLTemplate, err)
				return nil
			}

			// Parse listen addresses.
			var listenAddrs listenAddrFlags
			for _, raw := range rawListenAddrs {
				if err := listenAddrs.Set(raw); err != nil {
					actionErr = validationError(cmd, "%v", err)
					return nil
				}
			}

			// Default listen address if none provided.
			if len(listenAddrs) == 0 {
				if err := listenAddrs.Set("http=:8080,HTTP"); err != nil {
					actionErr = fmt.Errorf("default --listen-addr: %w", err)
					return nil
				}
			}

			// Validate listen address name uniqueness.
			seenLA := make(map[string]bool, len(listenAddrs))
			for _, la := range listenAddrs {
				if la.Name != "" {
					if seenLA[la.Name] {
						actionErr = validationError(cmd, "duplicate --listen-addr name %q", la.Name)
						return nil
					}
					seenLA[la.Name] = true
				}
			}
			c.ListenAddrs = []ListenAddrSpec(listenAddrs)

			// Normalize "none" to empty string to disable optional servers.
			if c.BroadcastAddr == "none" {
				c.BroadcastAddr = ""
			}
			if c.MetricsAddr == "none" {
				c.MetricsAddr = ""
			}

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
						actionErr = validationError(cmd, "--broadcast-target-listen-addr %q does not match any --listen-addr name", c.BroadcastTargetListenAddr)
						return nil
					}
				}
			}

			// Resolve frontend service namespace.
			ns, svc, err := parseNamespacedService(c.ServiceName, c.Namespace)
			if err != nil {
				actionErr = validationError(cmd, "--service-name: %v", err)
				return nil
			}
			c.ServiceNamespace = ns
			c.ServiceName = svc

			// Parse and validate backends.
			var backends backendFlags
			for _, raw := range rawBackends {
				if err := backends.Set(raw); err != nil {
					actionErr = validationError(cmd, "%v", err)
					return nil
				}
			}
			seen := make(map[string]bool, len(backends))
			for i, b := range backends {
				if seen[b.Name] {
					actionErr = validationError(cmd, "duplicate --backend name %q", b.Name)
					return nil
				}
				seen[b.Name] = true

				ns, svc, err := parseNamespacedService(b.ServiceName, c.Namespace)
				if err != nil {
					actionErr = validationError(cmd, "--backend %q: %v", b.Name, err)
					return nil
				}
				backends[i].Namespace = ns
				backends[i].ServiceName = svc
			}
			c.Backends = []BackendSpec(backends)

			// Parse and validate values.
			var values valuesFlags
			for _, raw := range rawValues {
				if err := values.Set(raw); err != nil {
					actionErr = validationError(cmd, "%v", err)
					return nil
				}
			}
			seenValues := make(map[string]bool, len(values)+len(rawValuesDirs))
			for i, v := range values {
				if seenValues[v.Name] {
					actionErr = validationError(cmd, "duplicate --values name %q", v.Name)
					return nil
				}
				seenValues[v.Name] = true

				ns, cm, err := parseNamespacedService(v.ConfigMapName, c.Namespace)
				if err != nil {
					actionErr = validationError(cmd, "--values %q: %v", v.Name, err)
					return nil
				}
				values[i].Namespace = ns
				values[i].ConfigMapName = cm
			}
			c.Values = []ValuesSpec(values)

			// Parse and validate values-dir.
			var valuesDirs valuesDirFlags
			for _, raw := range rawValuesDirs {
				if err := valuesDirs.Set(raw); err != nil {
					actionErr = validationError(cmd, "%v", err)
					return nil
				}
			}
			for _, vd := range valuesDirs {
				if seenValues[vd.Name] {
					actionErr = validationError(cmd, "duplicate values name %q (across --values and --values-dir)", vd.Name)
					return nil
				}
				seenValues[vd.Name] = true

				info, err := os.Stat(vd.Path)
				if err != nil {
					actionErr = validationError(cmd, "--values-dir %q: %v", vd.Name, err)
					return nil
				}
				if !info.IsDir() {
					actionErr = validationError(cmd, "--values-dir %q: path %q is not a directory", vd.Name, vd.Path)
					return nil
				}
			}
			c.ValuesDirs = []ValuesDirSpec(valuesDirs)

			// Extra varnishd args (after --).
			c.ExtraVarnishd = cmd.Args().Slice()

			return nil
		},
	}

	if err := cmd.Run(context.Background(), args); err != nil {
		return nil, err
	}
	if actionErr != nil {
		return nil, actionErr
	}
	if !parsed {
		return nil, ErrHelp
	}
	return c, nil
}
