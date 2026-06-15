// Package telemetry provides Prometheus metrics for k8s-httpcache.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "k8s_httpcache"

	// Prometheus metric label keys.
	labelStatus = "status"
	labelGroup  = "group"
	labelResult = "result"
)

// Metrics holds all Prometheus metrics for k8s-httpcache. Create one per
// registry with NewMetrics; production uses prometheus.DefaultRegisterer,
// tests use prometheus.NewRegistry() for isolation.
type Metrics struct {
	VCLReloadsTotal              *prometheus.CounterVec
	VCLRenderErrorsTotal         prometheus.Counter
	EndpointUpdatesTotal         *prometheus.CounterVec
	Endpoints                    *prometheus.GaugeVec
	VCLTemplateChangesTotal      prometheus.Counter
	VCLTemplateParseErrorsTotal  prometheus.Counter
	VCLRollbacksTotal            prometheus.Counter
	VarnishdUp                   prometheus.Gauge
	BroadcastRequestsTotal       *prometheus.CounterVec
	BroadcastFanoutTargets       prometheus.Gauge
	VCLReloadRetriesTotal        prometheus.Counter
	ValuesUpdatesTotal           *prometheus.CounterVec
	SecretsUpdatesTotal          *prometheus.CounterVec
	BuildInfo                    *prometheus.GaugeVec
	DebounceEventsTotal          *prometheus.CounterVec
	DebounceFiresTotal           *prometheus.CounterVec
	DebounceMaxEnforcementsTotal *prometheus.CounterVec
	VCLRenderDurationSeconds     prometheus.Histogram
	VCLReloadDurationSeconds     prometheus.Histogram
	BroadcastDurationSeconds     prometheus.Histogram
	DebounceLatencySeconds       *prometheus.HistogramVec
	TLSCertUpdatesTotal          *prometheus.CounterVec
	TLSCertReloadsTotal          *prometheus.CounterVec
	TLSCertOperationsTotal       *prometheus.CounterVec
	TLSCertsActive               prometheus.Gauge
}

// NewMetrics creates and registers all Prometheus metrics on reg.
// If debounceBuckets is nil, prometheus.DefBuckets is used for DebounceLatencySeconds.
func NewMetrics(reg prometheus.Registerer, debounceBuckets []float64) *Metrics {
	if debounceBuckets == nil {
		debounceBuckets = prometheus.DefBuckets
	}

	m := &Metrics{
		VCLReloadsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vcl_reloads_total",
			Help:      "Total number of VCL reload attempts.",
		}, []string{labelResult}),

		VCLRenderErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vcl_render_errors_total",
			Help:      "Total number of VCL template render failures.",
		}),

		EndpointUpdatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "endpoint_updates_total",
			Help:      "Total number of EndpointSlice updates received.",
		}, []string{"role", "service"}),

		Endpoints: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "endpoints",
			Help:      "Current number of ready endpoints per service.",
		}, []string{"role", "service"}),

		VCLTemplateChangesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vcl_template_changes_total",
			Help:      "Total number of VCL template file changes detected on disk.",
		}),

		VCLTemplateParseErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vcl_template_parse_errors_total",
			Help:      "Total number of VCL template parse failures.",
		}),

		VCLRollbacksTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vcl_rollbacks_total",
			Help:      "Total number of VCL template rollbacks to the previous version.",
		}),

		VarnishdUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "varnishd_up",
			Help:      "Whether the varnishd process is running (1 = up, 0 = down).",
		}),

		BroadcastRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "broadcast_requests_total",
			Help:      "Total number of broadcast HTTP requests.",
		}, []string{"method", labelStatus}),

		BroadcastFanoutTargets: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "broadcast_fanout_targets",
			Help:      "Number of frontend pods targeted by the last broadcast fan-out.",
		}),

		VCLReloadRetriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vcl_reload_retries_total",
			Help:      "Total number of VCL reload retry attempts.",
		}),

		ValuesUpdatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "values_updates_total",
			Help:      "Total number of ConfigMap value updates received.",
		}, []string{"configmap"}),

		SecretsUpdatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "secrets_updates_total",
			Help:      "Total number of Secret value updates received.",
		}, []string{"secret"}),

		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Build information. Always 1.",
		}, []string{"version", "goversion"}),

		DebounceEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "debounce_events_total",
			Help:      "Total number of events received per debounce group.",
		}, []string{labelGroup}),

		DebounceFiresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "debounce_fires_total",
			Help:      "Total number of debounce timer fires per group.",
		}, []string{labelGroup}),

		DebounceMaxEnforcementsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "debounce_max_enforcements_total",
			Help:      "Total number of reloads forced by the debounce-max deadline.",
		}, []string{labelGroup}),

		VCLRenderDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "vcl_render_duration_seconds",
			Help:      "Time to render the VCL template to a temporary file.",
			Buckets:   prometheus.DefBuckets,
		}),

		VCLReloadDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "vcl_reload_duration_seconds",
			Help:      "Time for varnishd VCL reload (vcl.load + vcl.use), including retries.",
			Buckets:   prometheus.DefBuckets,
		}),

		BroadcastDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "broadcast_duration_seconds",
			Help:      "Total wall-clock time for broadcast fan-out to all frontend pods.",
			Buckets:   prometheus.DefBuckets,
		}),

		DebounceLatencySeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "debounce_latency_seconds",
			Help:      "Wall-clock time from first event in a debounce burst to the reload.",
			Buckets:   debounceBuckets,
		}, []string{labelGroup}),

		TLSCertUpdatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "tls_cert_updates_total",
			Help:      "Total number of TLS certificate Secret updates received.",
		}, []string{"cert"}),

		TLSCertReloadsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "tls_cert_reloads_total",
			Help:      "Total number of TLS certificate (re)load attempts, by result (success, error, noop).",
		}, []string{"cert", labelResult}),

		TLSCertOperationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "tls_cert_operations_total",
			Help:      "Total number of varnishadm TLS certificate operations (load, commit, discard, rollback), by result.",
		}, []string{"operation", labelResult}),

		TLSCertsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "tls_certs_active",
			Help:      "Number of TLS certificates currently active in the cache.",
		}),
	}

	reg.MustRegister(
		m.VCLReloadsTotal,
		m.VCLRenderErrorsTotal,
		m.VCLReloadRetriesTotal,
		m.VCLTemplateChangesTotal,
		m.VCLTemplateParseErrorsTotal,
		m.VCLRollbacksTotal,
		m.EndpointUpdatesTotal,
		m.Endpoints,
		m.VarnishdUp,
		m.ValuesUpdatesTotal,
		m.SecretsUpdatesTotal,
		m.BroadcastRequestsTotal,
		m.BroadcastFanoutTargets,
		m.BuildInfo,
		m.DebounceEventsTotal,
		m.DebounceFiresTotal,
		m.DebounceMaxEnforcementsTotal,
		m.VCLRenderDurationSeconds,
		m.VCLReloadDurationSeconds,
		m.BroadcastDurationSeconds,
		m.DebounceLatencySeconds,
		m.TLSCertUpdatesTotal,
		m.TLSCertReloadsTotal,
		m.TLSCertOperationsTotal,
		m.TLSCertsActive,
	)

	return m
}
