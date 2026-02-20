package telemetry

import (
	"runtime"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestVCLReloadsTotal(t *testing.T) {
	VCLReloadsTotal.WithLabelValues("success").Inc()
	VCLReloadsTotal.WithLabelValues("error").Inc()
	VCLReloadsTotal.WithLabelValues("error").Inc()

	assertCounterValue(t, VCLReloadsTotal.WithLabelValues("success"), 1)
	assertCounterValue(t, VCLReloadsTotal.WithLabelValues("error"), 2)
}

func TestVCLRenderErrorsTotal(t *testing.T) {
	VCLRenderErrorsTotal.Inc()
	assertCounterValue(t, VCLRenderErrorsTotal, 1)
}

func TestVCLTemplateChangesTotal(t *testing.T) {
	VCLTemplateChangesTotal.Inc()
	assertCounterValue(t, VCLTemplateChangesTotal, 1)
}

func TestVCLTemplateParseErrorsTotal(t *testing.T) {
	VCLTemplateParseErrorsTotal.Inc()
	assertCounterValue(t, VCLTemplateParseErrorsTotal, 1)
}

func TestVCLRollbacksTotal(t *testing.T) {
	VCLRollbacksTotal.Inc()
	assertCounterValue(t, VCLRollbacksTotal, 1)
}

func TestEndpointUpdatesTotal(t *testing.T) {
	EndpointUpdatesTotal.WithLabelValues("frontend", "my-svc").Inc()
	assertCounterValue(t, EndpointUpdatesTotal.WithLabelValues("frontend", "my-svc"), 1)
}

func TestEndpoints(t *testing.T) {
	Endpoints.WithLabelValues("frontend", "my-svc").Set(5)
	assertGaugeValue(t, Endpoints.WithLabelValues("frontend", "my-svc"), 5)
}

func TestVarnishdUp(t *testing.T) {
	VarnishdUp.Set(1)
	assertGaugeValue(t, VarnishdUp, 1)

	VarnishdUp.Set(0)
	assertGaugeValue(t, VarnishdUp, 0)
}

func TestBroadcastRequestsTotal(t *testing.T) {
	BroadcastRequestsTotal.WithLabelValues("PURGE", "200").Inc()
	assertCounterValue(t, BroadcastRequestsTotal.WithLabelValues("PURGE", "200"), 1)
}

func TestBroadcastFanoutTargets(t *testing.T) {
	BroadcastFanoutTargets.Set(3)
	assertGaugeValue(t, BroadcastFanoutTargets, 3)
}

func TestBuildInfo(t *testing.T) {
	BuildInfo.WithLabelValues("v1.0.0", runtime.Version()).Set(1)
	assertGaugeValue(t, BuildInfo.WithLabelValues("v1.0.0", runtime.Version()), 1)
}

func TestMetricsRegistered(t *testing.T) {
	// Verify all metrics are present in the default registry by gathering them.
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"k8s_httpcache_vcl_reloads_total":               false,
		"k8s_httpcache_vcl_render_errors_total":         false,
		"k8s_httpcache_vcl_template_changes_total":      false,
		"k8s_httpcache_vcl_template_parse_errors_total": false,
		"k8s_httpcache_vcl_rollbacks_total":             false,
		"k8s_httpcache_endpoint_updates_total":          false,
		"k8s_httpcache_endpoints":                       false,
		"k8s_httpcache_varnishd_up":                     false,
		"k8s_httpcache_broadcast_requests_total":        false,
		"k8s_httpcache_broadcast_fanout_targets":        false,
		"k8s_httpcache_build_info":                      false,
	}

	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}

	for name, found := range want {
		if !found {
			t.Errorf("metric %q not found in default registry", name)
		}
	}
}

func assertCounterValue(t *testing.T, c prometheus.Counter, expected float64) {
	t.Helper()
	var m dto.Metric
	pm, ok := c.(prometheus.Metric)
	if !ok {
		t.Fatal("counter does not implement prometheus.Metric")
	}
	if err := pm.Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCounter().GetValue(); got < expected {
		t.Errorf("counter value = %v, want >= %v", got, expected)
	}
}

func assertGaugeValue(t *testing.T, g prometheus.Gauge, expected float64) {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetGauge().GetValue(); got != expected {
		t.Errorf("gauge value = %v, want %v", got, expected)
	}
}
