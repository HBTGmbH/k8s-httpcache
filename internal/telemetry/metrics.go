// Package telemetry provides Prometheus metrics for k8s-httpcache.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "k8s_httpcache"

var (
	// VCLReloadsTotal counts VCL reload attempts, labelled by result (success/error).
	VCLReloadsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "vcl_reloads_total",
		Help:      "Total number of VCL reload attempts.",
	}, []string{"result"})

	// VCLRenderErrorsTotal counts VCL template render failures.
	VCLRenderErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "vcl_render_errors_total",
		Help:      "Total number of VCL template render failures.",
	})

	// EndpointUpdatesTotal counts EndpointSlice updates received by the event loop.
	EndpointUpdatesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "endpoint_updates_total",
		Help:      "Total number of EndpointSlice updates received.",
	}, []string{"role", "service"})

	// Endpoints tracks the current ready endpoint count per service.
	Endpoints = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "endpoints",
		Help:      "Current number of ready endpoints per service.",
	}, []string{"role", "service"})

	// VCLTemplateChangesTotal counts VCL template file changes detected on disk.
	VCLTemplateChangesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "vcl_template_changes_total",
		Help:      "Total number of VCL template file changes detected on disk.",
	})

	// VCLTemplateParseErrorsTotal counts VCL template parse failures.
	VCLTemplateParseErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "vcl_template_parse_errors_total",
		Help:      "Total number of VCL template parse failures.",
	})

	// VCLRollbacksTotal counts template rollbacks to the previous known-good version.
	VCLRollbacksTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "vcl_rollbacks_total",
		Help:      "Total number of VCL template rollbacks to the previous version.",
	})

	// VarnishdUp indicates whether the varnishd child process is running.
	VarnishdUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "varnishd_up",
		Help:      "Whether the varnishd process is running (1 = up, 0 = down).",
	})

	// BroadcastRequestsTotal counts broadcast HTTP requests by method and status.
	BroadcastRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "broadcast_requests_total",
		Help:      "Total number of broadcast HTTP requests.",
	}, []string{"method", "status"})

	// BroadcastFanoutTargets tracks the number of frontends each broadcast hits.
	BroadcastFanoutTargets = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "broadcast_fanout_targets",
		Help:      "Number of frontend pods targeted by the last broadcast fan-out.",
	})

	// VCLReloadRetriesTotal counts VCL reload retry attempts (each retry increments this counter).
	VCLReloadRetriesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "vcl_reload_retries_total",
		Help:      "Total number of VCL reload retry attempts.",
	})

	// ValuesUpdatesTotal counts ConfigMap value updates received by the event loop.
	ValuesUpdatesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "values_updates_total",
		Help:      "Total number of ConfigMap value updates received.",
	}, []string{"configmap"})

	// BuildInfo is a gauge that always has value 1 and carries build metadata as labels.
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build information. Always 1.",
	}, []string{"version", "goversion"})

	// DebounceEventsTotal counts events received per debounce group.
	// Each call to resetDebounce (i.e. each event that resets/starts the
	// timer) increments this counter. Compare with DebounceFiresTotal to
	// see the coalescing ratio.
	DebounceEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "debounce_events_total",
		Help:      "Total number of events received per debounce group.",
	}, []string{"group"})

	// DebounceFiresTotal counts debounce timer fires per group — the group
	// whose timer triggered the reload.
	DebounceFiresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "debounce_fires_total",
		Help:      "Total number of debounce timer fires per group.",
	}, []string{"group"})

	// DebounceMaxEnforcementsTotal counts reloads where the debounce-max
	// deadline forced the timer to fire earlier than the normal debounce
	// window.
	DebounceMaxEnforcementsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "debounce_max_enforcements_total",
		Help:      "Total number of reloads forced by the debounce-max deadline.",
	}, []string{"group"})

	// VCLRenderDurationSeconds observes the time to execute the VCL template
	// and write the rendered output to a temporary file.
	VCLRenderDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "vcl_render_duration_seconds",
		Help:      "Time to render the VCL template to a temporary file.",
		Buckets:   prometheus.DefBuckets,
	})

	// VCLReloadDurationSeconds observes the time for varnishd vcl.load + vcl.use,
	// including any retries.
	VCLReloadDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "vcl_reload_duration_seconds",
		Help:      "Time for varnishd VCL reload (vcl.load + vcl.use), including retries.",
		Buckets:   prometheus.DefBuckets,
	})

	// BroadcastDurationSeconds observes the total wall-clock time for
	// broadcast fan-out to all frontend pods.
	BroadcastDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "broadcast_duration_seconds",
		Help:      "Total wall-clock time for broadcast fan-out to all frontend pods.",
		Buckets:   prometheus.DefBuckets,
	})

	// DebounceLatencySeconds observes wall-clock time from the first event
	// in a debounce burst to the reload, per group. It is registered via
	// RegisterDebounceLatency after CLI parsing so bucket boundaries can
	// be configured at runtime.
	DebounceLatencySeconds *prometheus.HistogramVec
)

func init() {
	prometheus.MustRegister(
		VCLReloadsTotal,
		VCLRenderErrorsTotal,
		VCLReloadRetriesTotal,
		VCLTemplateChangesTotal,
		VCLTemplateParseErrorsTotal,
		VCLRollbacksTotal,
		EndpointUpdatesTotal,
		Endpoints,
		VarnishdUp,
		ValuesUpdatesTotal,
		BroadcastRequestsTotal,
		BroadcastFanoutTargets,
		BuildInfo,
		DebounceEventsTotal,
		DebounceFiresTotal,
		DebounceMaxEnforcementsTotal,
		VCLRenderDurationSeconds,
		VCLReloadDurationSeconds,
		BroadcastDurationSeconds,
	)
}

// RegisterDebounceLatency creates and registers the DebounceLatencySeconds
// histogram with the given bucket boundaries. It must be called once after
// CLI parsing and before any metrics are observed.
func RegisterDebounceLatency(buckets []float64) {
	DebounceLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "debounce_latency_seconds",
		Help:      "Wall-clock time from first event in a debounce burst to the reload.",
		Buckets:   buckets,
	}, []string{"group"})
	prometheus.MustRegister(DebounceLatencySeconds)
}
