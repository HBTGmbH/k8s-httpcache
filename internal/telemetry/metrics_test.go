package telemetry

import (
	"os"
	"runtime"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestMain(m *testing.M) {
	RegisterDebounceLatency([]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})
	os.Exit(m.Run())
}

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

func TestVCLReloadRetriesTotal(t *testing.T) {
	VCLReloadRetriesTotal.Inc()
	assertCounterValue(t, VCLReloadRetriesTotal, 1)
}

func TestValuesUpdatesTotal(t *testing.T) {
	ValuesUpdatesTotal.WithLabelValues("my-configmap").Inc()
	assertCounterValue(t, ValuesUpdatesTotal.WithLabelValues("my-configmap"), 1)
}

func TestDebounceEventsTotal(t *testing.T) {
	DebounceEventsTotal.WithLabelValues("frontend").Inc()
	DebounceEventsTotal.WithLabelValues("backend").Inc()
	DebounceEventsTotal.WithLabelValues("backend").Inc()

	assertCounterValue(t, DebounceEventsTotal.WithLabelValues("frontend"), 1)
	assertCounterValue(t, DebounceEventsTotal.WithLabelValues("backend"), 2)
}

func TestDebounceFiresTotal(t *testing.T) {
	DebounceFiresTotal.WithLabelValues("frontend").Inc()
	assertCounterValue(t, DebounceFiresTotal.WithLabelValues("frontend"), 1)
}

func TestDebounceMaxEnforcementsTotal(t *testing.T) {
	DebounceMaxEnforcementsTotal.WithLabelValues("backend").Inc()
	assertCounterValue(t, DebounceMaxEnforcementsTotal.WithLabelValues("backend"), 1)
}

func TestDebounceLatencySeconds(t *testing.T) {
	DebounceLatencySeconds.WithLabelValues("frontend").Observe(0.05)
	DebounceLatencySeconds.WithLabelValues("frontend").Observe(0.15)

	var m dto.Metric
	observer := DebounceLatencySeconds.WithLabelValues("frontend")
	pm, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatal("histogram does not implement prometheus.Metric")
	}
	if err := pm.Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetHistogram().GetSampleCount(); got < 2 {
		t.Errorf("histogram sample count = %d, want >= 2", got)
	}
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
		"k8s_httpcache_debounce_events_total":           false,
		"k8s_httpcache_debounce_fires_total":            false,
		"k8s_httpcache_debounce_max_enforcements_total": false,
		"k8s_httpcache_debounce_latency_seconds":        false,
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
