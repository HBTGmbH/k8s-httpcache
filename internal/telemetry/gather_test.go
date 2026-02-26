package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gatherFiltered is a test helper that gathers through a ZeroCounterFilter.
func gatherFiltered(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()

	filter := &ZeroCounterFilter{Inner: reg}
	families, err := filter.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	return families
}

// gatherFamilyMap is a test helper that returns gathered families keyed by name.
func gatherFamilyMap(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()

	families := gatherFiltered(t, reg)
	m := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		m[f.GetName()] = f
	}

	return m
}

func TestZeroCounterFilterDropsZeroCounter(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "idle_events_total", Help: "never fired"})
	reg.MustRegister(c)

	fams := gatherFamilyMap(t, reg)
	if _, ok := fams["idle_events_total"]; ok {
		t.Error("zero-value counter should be filtered out")
	}
}

func TestZeroCounterFilterRetainsNonZeroCounter(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "active_events_total", Help: "fired"})
	reg.MustRegister(c)
	c.Add(42)

	fams := gatherFamilyMap(t, reg)
	fam, ok := fams["active_events_total"]
	if !ok {
		t.Fatal("non-zero counter should be present")
	}
	if got := fam.GetMetric()[0].GetCounter().GetValue(); got != 42 {
		t.Errorf("counter value = %v, want 42", got)
	}
}

func TestZeroCounterFilterRetainsZeroGauge(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "current_items", Help: "items"})
	reg.MustRegister(g)
	// gauge stays at 0

	fams := gatherFamilyMap(t, reg)
	fam, ok := fams["current_items"]
	if !ok {
		t.Fatal("zero-value gauge should be present")
	}
	if got := fam.GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Errorf("gauge value = %v, want 0", got)
	}
}

func TestZeroCounterFilterRetainsZeroHistogram(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	h := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "request_duration_seconds",
		Help:    "latency",
		Buckets: []float64{0.1, 0.5, 1},
	})
	reg.MustRegister(h)
	// no observations — sample count is 0

	fams := gatherFamilyMap(t, reg)
	fam, ok := fams["request_duration_seconds"]
	if !ok {
		t.Fatal("zero-observation histogram should be present")
	}
	if got := fam.GetMetric()[0].GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("histogram sample count = %v, want 0", got)
	}
}

func TestZeroCounterFilterRetainsZeroSummary(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	s := prometheus.NewSummary(prometheus.SummaryOpts{Name: "response_size_bytes", Help: "sizes"})
	reg.MustRegister(s)
	// no observations

	fams := gatherFamilyMap(t, reg)
	fam, ok := fams["response_size_bytes"]
	if !ok {
		t.Fatal("zero-observation summary should be present")
	}
	if got := fam.GetMetric()[0].GetSummary().GetSampleCount(); got != 0 {
		t.Errorf("summary sample count = %v, want 0", got)
	}
}

func TestZeroCounterFilterCounterVecPartialDrop(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cv := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "requests_total", Help: "reqs"}, []string{"status"})
	reg.MustRegister(cv)

	cv.WithLabelValues("200").Add(5)
	cv.WithLabelValues("500") // initialize at 0

	fams := gatherFamilyMap(t, reg)
	fam, ok := fams["requests_total"]
	if !ok {
		t.Fatal("requests_total family should be present (has non-zero members)")
	}
	if len(fam.GetMetric()) != 1 {
		t.Fatalf("expected 1 metric (status=200 only), got %d", len(fam.GetMetric()))
	}

	m := fam.GetMetric()[0]
	if got := m.GetCounter().GetValue(); got != 5 {
		t.Errorf("surviving counter value = %v, want 5", got)
	}

	var statusLabel string
	for _, lp := range m.GetLabel() {
		if lp.GetName() == "status" {
			statusLabel = lp.GetValue()
		}
	}
	if statusLabel != "200" {
		t.Errorf("surviving label status = %q, want 200", statusLabel)
	}
}

func TestZeroCounterFilterAllZeroDropsFamily(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cv := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "all_zero_total", Help: "all zero"}, []string{"code"})
	reg.MustRegister(cv)

	cv.WithLabelValues("200") // 0
	cv.WithLabelValues("500") // 0

	fams := gatherFamilyMap(t, reg)
	if _, ok := fams["all_zero_total"]; ok {
		t.Error("counter family with all zero values should be dropped entirely")
	}
}

func TestZeroCounterFilterEmptyRegistry(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	families := gatherFiltered(t, reg)
	if len(families) != 0 {
		t.Errorf("expected 0 families from empty registry, got %d", len(families))
	}
}
