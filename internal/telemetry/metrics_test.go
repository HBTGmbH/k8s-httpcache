package telemetry

import (
	"runtime"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

var testBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

func TestVCLReloadsTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLReloadsTotal.WithLabelValues("success").Inc()
	m.VCLReloadsTotal.WithLabelValues("error").Inc()
	m.VCLReloadsTotal.WithLabelValues("error").Inc()

	assertCounterValue(t, m.VCLReloadsTotal.WithLabelValues("success"), 1)
	assertCounterValue(t, m.VCLReloadsTotal.WithLabelValues("error"), 2)
}

func TestVCLRenderErrorsTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLRenderErrorsTotal.Inc()
	assertCounterValue(t, m.VCLRenderErrorsTotal, 1)
}

func TestVCLTemplateChangesTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLTemplateChangesTotal.Inc()
	assertCounterValue(t, m.VCLTemplateChangesTotal, 1)
}

func TestVCLTemplateParseErrorsTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLTemplateParseErrorsTotal.Inc()
	assertCounterValue(t, m.VCLTemplateParseErrorsTotal, 1)
}

func TestVCLRollbacksTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLRollbacksTotal.Inc()
	assertCounterValue(t, m.VCLRollbacksTotal, 1)
}

func TestEndpointUpdatesTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.EndpointUpdatesTotal.WithLabelValues("frontend", "my-svc").Inc()
	assertCounterValue(t, m.EndpointUpdatesTotal.WithLabelValues("frontend", "my-svc"), 1)
}

func TestEndpoints(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.Endpoints.WithLabelValues("frontend", "my-svc").Set(5)
	assertGaugeValue(t, m.Endpoints.WithLabelValues("frontend", "my-svc"), 5)
}

func TestVarnishdUp(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VarnishdUp.Set(1)
	assertGaugeValue(t, m.VarnishdUp, 1)

	m.VarnishdUp.Set(0)
	assertGaugeValue(t, m.VarnishdUp, 0)
}

func TestBroadcastRequestsTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.BroadcastRequestsTotal.WithLabelValues("PURGE", "200").Inc()
	assertCounterValue(t, m.BroadcastRequestsTotal.WithLabelValues("PURGE", "200"), 1)
}

func TestBroadcastFanoutTargets(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.BroadcastFanoutTargets.Set(3)
	assertGaugeValue(t, m.BroadcastFanoutTargets, 3)
}

func TestBuildInfo(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.BuildInfo.WithLabelValues("v1.0.0", runtime.Version()).Set(1)
	assertGaugeValue(t, m.BuildInfo.WithLabelValues("v1.0.0", runtime.Version()), 1)
}

func TestVCLReloadRetriesTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLReloadRetriesTotal.Inc()
	assertCounterValue(t, m.VCLReloadRetriesTotal, 1)
}

func TestValuesUpdatesTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.ValuesUpdatesTotal.WithLabelValues("my-configmap").Inc()
	assertCounterValue(t, m.ValuesUpdatesTotal.WithLabelValues("my-configmap"), 1)
}

func TestSecretsUpdatesTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.SecretsUpdatesTotal.WithLabelValues("my-secret").Inc()
	assertCounterValue(t, m.SecretsUpdatesTotal.WithLabelValues("my-secret"), 1)
}

func TestDebounceEventsTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.DebounceEventsTotal.WithLabelValues("frontend").Inc()
	m.DebounceEventsTotal.WithLabelValues("backend").Inc()
	m.DebounceEventsTotal.WithLabelValues("backend").Inc()

	assertCounterValue(t, m.DebounceEventsTotal.WithLabelValues("frontend"), 1)
	assertCounterValue(t, m.DebounceEventsTotal.WithLabelValues("backend"), 2)
}

func TestDebounceFiresTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.DebounceFiresTotal.WithLabelValues("frontend").Inc()
	assertCounterValue(t, m.DebounceFiresTotal.WithLabelValues("frontend"), 1)
}

func TestDebounceMaxEnforcementsTotal(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.DebounceMaxEnforcementsTotal.WithLabelValues("backend").Inc()
	assertCounterValue(t, m.DebounceMaxEnforcementsTotal.WithLabelValues("backend"), 1)
}

func TestVCLRenderDurationSeconds(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLRenderDurationSeconds.Observe(0.05)
	m.VCLRenderDurationSeconds.Observe(0.15)

	var dm dto.Metric

	err := m.VCLRenderDurationSeconds.Write(&dm)
	if err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("histogram sample count = %d, want 2", got)
	}
}

func TestVCLReloadDurationSeconds(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.VCLReloadDurationSeconds.Observe(0.1)
	m.VCLReloadDurationSeconds.Observe(0.2)

	var dm dto.Metric

	err := m.VCLReloadDurationSeconds.Write(&dm)
	if err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("histogram sample count = %d, want 2", got)
	}
}

func TestBroadcastDurationSeconds(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.BroadcastDurationSeconds.Observe(0.01)
	m.BroadcastDurationSeconds.Observe(0.02)

	var dm dto.Metric

	err := m.BroadcastDurationSeconds.Write(&dm)
	if err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("histogram sample count = %d, want 2", got)
	}
}

func TestDebounceLatencySeconds(t *testing.T) {
	t.Parallel()
	m := NewMetrics(prometheus.NewRegistry(), testBuckets)
	m.DebounceLatencySeconds.WithLabelValues("frontend").Observe(0.05)
	m.DebounceLatencySeconds.WithLabelValues("frontend").Observe(0.15)

	var dm dto.Metric
	observer := m.DebounceLatencySeconds.WithLabelValues("frontend")
	pm, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatal("histogram does not implement prometheus.Metric")
	}
	err := pm.Write(&dm)
	if err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("histogram sample count = %d, want 2", got)
	}
}

func TestMetricsRegistered(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	_ = NewMetrics(reg, testBuckets)

	ch := make(chan *prometheus.Desc, 64)
	go func() {
		reg.Describe(ch)
		close(ch)
	}()

	found := map[string]bool{}
	for d := range ch {
		found[d.String()] = true
	}

	want := []string{
		"k8s_httpcache_vcl_reloads_total",
		"k8s_httpcache_vcl_render_errors_total",
		"k8s_httpcache_vcl_reload_retries_total",
		"k8s_httpcache_vcl_template_changes_total",
		"k8s_httpcache_vcl_template_parse_errors_total",
		"k8s_httpcache_vcl_rollbacks_total",
		"k8s_httpcache_endpoint_updates_total",
		"k8s_httpcache_endpoints",
		"k8s_httpcache_varnishd_up",
		"k8s_httpcache_values_updates_total",
		"k8s_httpcache_secrets_updates_total",
		"k8s_httpcache_broadcast_requests_total",
		"k8s_httpcache_broadcast_fanout_targets",
		"k8s_httpcache_build_info",
		"k8s_httpcache_debounce_events_total",
		"k8s_httpcache_debounce_fires_total",
		"k8s_httpcache_debounce_max_enforcements_total",
		"k8s_httpcache_debounce_latency_seconds",
		"k8s_httpcache_vcl_render_duration_seconds",
		"k8s_httpcache_vcl_reload_duration_seconds",
		"k8s_httpcache_broadcast_duration_seconds",
	}

	if len(found) != len(want) {
		t.Errorf("registry has %d metrics, want %d", len(found), len(want))
	}

	for _, name := range want {
		seen := false
		for desc := range found {
			if strings.Contains(desc, "\""+name+"\"") {
				seen = true

				break
			}
		}
		if !seen {
			t.Errorf("metric %q not found in registry", name)
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
	err := pm.Write(&m)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.GetCounter().GetValue(); got != expected {
		t.Errorf("counter value = %v, want %v", got, expected)
	}
}

func TestNewMetrics_NilBucketsUsesDefaults(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)

	// Observe a value and verify the histogram works (proves DefBuckets was used).
	m.DebounceLatencySeconds.WithLabelValues("frontend").Observe(0.5)

	var dm dto.Metric
	observer := m.DebounceLatencySeconds.WithLabelValues("frontend")
	pm, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatal("histogram does not implement prometheus.Metric")
	}
	err := pm.Write(&dm)
	if err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 1 {
		t.Errorf("histogram sample count = %d, want 1", got)
	}
}

func assertGaugeValue(t *testing.T, g prometheus.Gauge, expected float64) {
	t.Helper()
	var m dto.Metric
	err := g.Write(&m)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.GetGauge().GetValue(); got != expected {
		t.Errorf("gauge value = %v, want %v", got, expected)
	}
}
