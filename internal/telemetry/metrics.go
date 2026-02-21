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
)

func init() {
	prometheus.MustRegister(
		VCLReloadsTotal,
		VCLRenderErrorsTotal,
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
	)
}
