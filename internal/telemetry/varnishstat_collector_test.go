package telemetry

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// collectMetrics is a test helper that gathers all metrics from a collector.
// Uses a regular registry (not pedantic) because the collector uses the
// unchecked collector pattern where dynamic metrics are not pre-registered
// via Describe.
func collectMetrics(t *testing.T, c prometheus.Collector) []*dto.MetricFamily {
	t.Helper()

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	return families
}

// findFamily returns the MetricFamily with the given name, or nil.
func findFamily(families []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}

	return nil
}

// labelValue returns the value of the named label on the first metric, or "".
func labelValue(mf *dto.MetricFamily, labelName string) string {
	if mf == nil || len(mf.GetMetric()) == 0 {
		return ""
	}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		if lp.GetName() == labelName {
			return lp.GetValue()
		}
	}

	return ""
}

func TestVarnishstatCollectorV7BasicCounters(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit": {"value": 42, "flag": "c", "description": "Cache hits"},
			"MAIN.n_object": {"value": 10, "flag": "g", "description": "Object count"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	// Check varnish_up = 1.
	up := findFamily(families, "varnish_up")
	if up == nil {
		t.Fatal("missing varnish_up metric")
	}
	if got := up.GetMetric()[0].GetGauge().GetValue(); got != 1 {
		t.Errorf("varnish_up = %v, want 1", got)
	}

	// Check counter type for cache_hit.
	hit := findFamily(families, "varnish_main_cache_hit")
	if hit == nil {
		t.Fatal("missing varnish_main_cache_hit metric")
	}
	if got := hit.GetType(); got != dto.MetricType_COUNTER {
		t.Errorf("varnish_main_cache_hit type = %v, want COUNTER", got)
	}
	if got := hit.GetMetric()[0].GetCounter().GetValue(); got != 42 {
		t.Errorf("varnish_main_cache_hit = %v, want 42", got)
	}

	// Check gauge type for n_object.
	obj := findFamily(families, "varnish_main_n_object")
	if obj == nil {
		t.Fatal("missing varnish_main_n_object metric")
	}
	if got := obj.GetType(); got != dto.MetricType_GAUGE {
		t.Errorf("varnish_main_n_object type = %v, want GAUGE", got)
	}
	if got := obj.GetMetric()[0].GetGauge().GetValue(); got != 10 {
		t.Errorf("varnish_main_n_object = %v, want 10", got)
	}
}

func TestVarnishstatCollectorV6BasicCounters(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.cache_hit": {"value": 100, "flag": "c", "description": "Cache hits"},
		"MAIN.n_object": {"value": 5, "flag": "g", "description": "Object count"}
	}`

	fn := func() (string, int, error) { return jsonOutput, 6, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	hit := findFamily(families, "varnish_main_cache_hit")
	if hit == nil {
		t.Fatal("missing varnish_main_cache_hit metric")
	}
	if got := hit.GetMetric()[0].GetCounter().GetValue(); got != 100 {
		t.Errorf("varnish_main_cache_hit = %v, want 100", got)
	}
}

func TestVarnishstatCollectorErrorSetsUpZero(t *testing.T) {
	t.Parallel()

	fn := func() (string, int, error) { return "", 7, errors.New("varnishstat failed") }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	up := findFamily(families, "varnish_up")
	if up == nil {
		t.Fatal("missing varnish_up metric")
	}
	if got := up.GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Errorf("varnish_up = %v, want 0", got)
	}

	dur := findFamily(families, "varnish_scrape_duration_seconds")
	if dur == nil {
		t.Fatal("missing varnish_scrape_duration_seconds metric")
	}
}

func TestVarnishstatCollectorGroupFilterIncludesOnly(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit": {"value": 42, "flag": "c", "description": "Cache hits"},
			"SMA.s0.g_alloc": {"value": 5, "flag": "g", "description": "Allocations", "ident": "s0"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, []string{"MAIN"})

	families := collectMetrics(t, c)

	if findFamily(families, "varnish_main_cache_hit") == nil {
		t.Error("expected varnish_main_cache_hit to be present")
	}
	if findFamily(families, "varnish_sma_g_alloc") != nil {
		t.Error("expected varnish_sma_g_alloc to be filtered out")
	}
}

func TestVarnishstatCollectorEmptyFilterExportsAll(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit": {"value": 1, "flag": "c", "description": ""},
			"SMA.s0.g_alloc": {"value": 2, "flag": "g", "description": "", "ident": "s0"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	if findFamily(families, "varnish_main_cache_hit") == nil {
		t.Error("expected varnish_main_cache_hit to be present")
	}
	if findFamily(families, "varnish_sma_g_alloc") == nil {
		t.Error("expected varnish_sma_g_alloc to be present")
	}
}

func TestVarnishstatCollectorDescribeEmitsFixedDescriptors(t *testing.T) {
	t.Parallel()

	fn := func() (string, int, error) { return "", 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	ch := make(chan *prometheus.Desc, 10)
	c.Describe(ch)
	close(ch)

	var descs []*prometheus.Desc
	for d := range ch {
		descs = append(descs, d)
	}
	if len(descs) != 4 {
		t.Errorf("Describe sent %d descriptors, want 4", len(descs))
	}

	var names []string
	for _, d := range descs {
		s := d.String()
		switch {
		case strings.Contains(s, "varnish_up"):
			names = append(names, "varnish_up")
		case strings.Contains(s, "varnish_scrape_duration_seconds"):
			names = append(names, "varnish_scrape_duration_seconds")
		case strings.Contains(s, "varnish_exporter_total_scrapes"):
			names = append(names, "varnish_exporter_total_scrapes")
		case strings.Contains(s, "varnish_exporter_json_parse_failures_total"):
			names = append(names, "varnish_exporter_json_parse_failures_total")
		}
	}
	slices.Sort(names)
	want := []string{
		"varnish_exporter_json_parse_failures_total",
		"varnish_exporter_total_scrapes",
		"varnish_scrape_duration_seconds",
		"varnish_up",
	}
	if !slices.Equal(names, want) {
		t.Errorf("descriptor names = %v, want %v", names, want)
	}
}

func TestVarnishstatCollectorGroupFilterCaseInsensitive(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit": {"value": 1, "flag": "c", "description": ""}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, []string{"main"})

	families := collectMetrics(t, c)
	if findFamily(families, "varnish_main_cache_hit") == nil {
		t.Error("expected varnish_main_cache_hit with lowercase filter")
	}
}

func TestVarnishstatCollectorScrapeDurationPresent(t *testing.T) {
	t.Parallel()

	jsonOutput := `{"version": 1, "counters": {}}`
	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	dur := findFamily(families, "varnish_scrape_duration_seconds")
	if dur == nil {
		t.Fatal("missing varnish_scrape_duration_seconds metric")
	}
	if got := dur.GetMetric()[0].GetGauge().GetValue(); got < 0 {
		t.Errorf("scrape duration = %v, want >= 0", got)
	}
}

func TestVarnishstatCollectorTotalScrapes(t *testing.T) {
	t.Parallel()

	jsonOutput := `{"version": 1, "counters": {}}`
	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	scrapes := findFamily(families, "varnish_exporter_total_scrapes")
	if scrapes == nil {
		t.Fatal("missing varnish_exporter_total_scrapes metric")
	}
	if got := scrapes.GetMetric()[0].GetCounter().GetValue(); got != 1 {
		t.Errorf("total_scrapes after 1 scrape = %v, want 1", got)
	}
}

func TestVarnishstatCollectorTotalScrapesOnError(t *testing.T) {
	t.Parallel()

	fn := func() (string, int, error) { return "", 7, errors.New("fail") }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	scrapes := findFamily(families, "varnish_exporter_total_scrapes")
	if scrapes == nil {
		t.Fatal("missing varnish_exporter_total_scrapes on error")
	}
	if got := scrapes.GetMetric()[0].GetCounter().GetValue(); got != 1 {
		t.Errorf("total_scrapes on error = %v, want 1", got)
	}
}

func TestVarnishstatCollectorParseFailuresZeroOnSuccess(t *testing.T) {
	t.Parallel()

	jsonOutput := `{"version": 1, "counters": {}}`
	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	pf := findFamily(families, "varnish_exporter_json_parse_failures_total")
	if pf == nil {
		t.Fatal("missing varnish_exporter_json_parse_failures_total metric")
	}
	if got := pf.GetMetric()[0].GetCounter().GetValue(); got != 0 {
		t.Errorf("parse_failures on success = %v, want 0", got)
	}
}

func TestVarnishstatCollectorParseFailuresIncrementsOnBadJSON(t *testing.T) {
	t.Parallel()

	fn := func() (string, int, error) { return "not valid json", 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	up := findFamily(families, "varnish_up")
	if up == nil {
		t.Fatal("missing varnish_up metric")
	}
	if got := up.GetMetric()[0].GetGauge().GetValue(); got != 1 {
		t.Errorf("varnish_up = %v, want 1", got)
	}

	pf := findFamily(families, "varnish_exporter_json_parse_failures_total")
	if pf == nil {
		t.Fatal("missing varnish_exporter_json_parse_failures_total metric")
	}
	if got := pf.GetMetric()[0].GetCounter().GetValue(); got != 1 {
		t.Errorf("parse_failures on bad JSON = %v, want 1", got)
	}
}

func TestVarnishstatCollectorParseFailuresOnError(t *testing.T) {
	t.Parallel()

	fn := func() (string, int, error) { return "", 7, errors.New("exec fail") }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	pf := findFamily(families, "varnish_exporter_json_parse_failures_total")
	if pf == nil {
		t.Fatal("missing varnish_exporter_json_parse_failures_total metric")
	}
	if got := pf.GetMetric()[0].GetCounter().GetValue(); got != 0 {
		t.Errorf("parse_failures on exec error = %v, want 0", got)
	}
}

func TestVarnishstatCollectorAccumulatorFlagIsCounter(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.s_req": {"value": 999, "flag": "a", "description": "Total requests"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	req := findFamily(families, "varnish_main_s_req")
	if req == nil {
		t.Fatal("missing varnish_main_s_req metric")
	}
	if got := req.GetType(); got != dto.MetricType_COUNTER {
		t.Errorf("flag=a metric type = %v, want COUNTER", got)
	}
}

// --- VBE label extraction ---

func TestVarnishstatCollectorVBELabelsParenthesized(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.my_backend(127.0.0.1,,8080).happy": {
				"value": 18446744073709551615,
				"flag": "b",
				"description": "Happy health probes",
				"ident": "boot.my_backend(127.0.0.1,,8080)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}
	if got := labelValue(up, "backend"); got != "my_backend" {
		t.Errorf("backend label = %q, want my_backend", got)
	}
	if got := labelValue(up, "server"); got != "127.0.0.1:8080" {
		t.Errorf("server label = %q, want 127.0.0.1:8080", got)
	}
}

func TestVarnishstatCollectorVBELabelsReloadPrefix(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.kv_reload_20240101_120000_123.my_backend(10.0.0.1,,80).happy": {
				"value": 18446744073709551615,
				"flag": "b",
				"description": "Happy health probes",
				"ident": "kv_reload_20240101_120000_123.my_backend(10.0.0.1,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}
	if got := labelValue(up, "backend"); got != "my_backend" {
		t.Errorf("backend label = %q, want my_backend", got)
	}
}

func TestVarnishstatCollectorVBELabelsPlain(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.default.happy": {
				"value": 18446744073709551615,
				"flag": "b",
				"description": "Happy health probes",
				"ident": "boot.default"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}
	if got := labelValue(up, "backend"); got != "default" {
		t.Errorf("backend label = %q, want default", got)
	}
	if got := labelValue(up, "server"); got != "unknown" {
		t.Errorf("server label = %q, want unknown", got)
	}
}

// --- VBE reload filtering ---

func TestVarnishstatCollectorVBEReloadFiltering(t *testing.T) {
	t.Parallel()

	// kv_reload_2 is newer than kv_reload_1; only kv_reload_2 counters should appear.
	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.kv_reload_1.old_backend(10.0.0.1,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "",
				"ident": "kv_reload_1.old_backend(10.0.0.1,,80)"
			},
			"VBE.kv_reload_2.new_backend(10.0.0.2,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "",
				"ident": "kv_reload_2.new_backend(10.0.0.2,,80)"
			},
			"VBE.kv_reload_1.old_backend(10.0.0.1,,80).fail": {
				"value": 0, "flag": "c", "description": "",
				"ident": "kv_reload_1.old_backend(10.0.0.1,,80)"
			},
			"VBE.kv_reload_2.new_backend(10.0.0.2,,80).fail": {
				"value": 3, "flag": "c", "description": "",
				"ident": "kv_reload_2.new_backend(10.0.0.2,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	// The new_backend should be present.
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}
	if got := labelValue(up, "backend"); got != "new_backend" {
		t.Errorf("backend label = %q, want new_backend", got)
	}

	// There should be exactly one backend_up and one backend_fail (from kv_reload_2 only).
	fail := findFamily(families, "varnish_backend_fail")
	if fail == nil {
		t.Fatal("missing varnish_backend_fail metric")
	}
	if len(fail.GetMetric()) != 1 {
		t.Errorf("expected 1 backend_fail metric, got %d", len(fail.GetMetric()))
	}
}

func TestVarnishstatCollectorVBEBootNotFilteredWhenNoReload(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.default(127.0.0.1,,8080).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "",
				"ident": "boot.default(127.0.0.1,,8080)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("boot VBE should not be filtered when no reload_ prefix exists")
	}
}

// --- Backend up derivation ---

func TestVarnishstatCollectorBackendUp(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.healthy(10.0.0.1,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "",
				"ident": "boot.healthy(10.0.0.1,,80)"
			},
			"VBE.boot.sick(10.0.0.2,,80).happy": {
				"value": 0, "flag": "b", "description": "",
				"ident": "boot.sick(10.0.0.2,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}
	if len(up.GetMetric()) != 2 {
		t.Fatalf("expected 2 backend_up metrics, got %d", len(up.GetMetric()))
	}

	for _, m := range up.GetMetric() {
		var backend string
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "backend" {
				backend = lp.GetValue()
			}
		}
		val := m.GetGauge().GetValue()
		switch backend {
		case "healthy":
			if val != 1 {
				t.Errorf("backend_up{backend=healthy} = %v, want 1", val)
			}
		case "sick":
			if val != 0 {
				t.Errorf("backend_up{backend=sick} = %v, want 0", val)
			}
		default:
			t.Errorf("unexpected backend label: %q", backend)
		}
	}
}

// --- SMA/LCK/MEMPOOL label extraction ---

func TestVarnishstatCollectorSMALabels(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"SMA.s0.g_alloc": {"value": 42, "flag": "g", "description": "Allocations outstanding", "ident": "s0"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	alloc := findFamily(families, "varnish_sma_g_alloc")
	if alloc == nil {
		t.Fatal("missing varnish_sma_g_alloc metric")
	}
	if got := labelValue(alloc, "type"); got != "s0" {
		t.Errorf("type label = %q, want s0", got)
	}
}

func TestVarnishstatCollectorLCKLabelsAndRename(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"LCK.sms.creat": {"value": 1, "flag": "c", "description": "Created", "ident": "sms"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	// varnish_lck_creat → varnish_lock_created (renamed).
	created := findFamily(families, "varnish_lock_created")
	if created == nil {
		t.Fatal("missing varnish_lock_created metric (expected rename from varnish_lck_creat)")
	}
	if got := labelValue(created, "target"); got != "sms" {
		t.Errorf("target label = %q, want sms", got)
	}
}

func TestVarnishstatCollectorMEMPOOLLabels(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MEMPOOL.sess0.live": {"value": 5, "flag": "g", "description": "In use", "ident": "sess0"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	live := findFamily(families, "varnish_mempool_live")
	if live == nil {
		t.Fatal("missing varnish_mempool_live metric")
	}
	if got := labelValue(live, "id"); got != "sess0" {
		t.Errorf("id label = %q, want sess0", got)
	}
}

// --- Metric grouping ---

func TestVarnishstatCollectorFetchGrouping(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.fetch_head": {"value": 10, "flag": "c", "description": "Fetch head"},
			"MAIN.fetch_204": {"value": 5, "flag": "c", "description": "Fetch 204"},
			"MAIN.s_fetch": {"value": 100, "flag": "c", "description": "Total fetches"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	// fetch_head and fetch_204 should be grouped under varnish_main_fetch{type=...}.
	fetch := findFamily(families, "varnish_main_fetch")
	if fetch == nil {
		t.Fatal("missing varnish_main_fetch grouped metric")
	}
	if len(fetch.GetMetric()) != 2 {
		t.Fatalf("expected 2 fetch metrics, got %d", len(fetch.GetMetric()))
	}

	// s_fetch should be the total.
	total := findFamily(families, "varnish_main_fetch_total")
	if total == nil {
		t.Fatal("missing varnish_main_fetch_total metric")
	}
}

func TestVarnishstatCollectorSessionGrouping(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.sess_conn": {"value": 50, "flag": "c", "description": "Sessions accepted"},
			"MAIN.s_sess": {"value": 200, "flag": "c", "description": "Total sessions"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	sess := findFamily(families, "varnish_main_sessions")
	if sess == nil {
		t.Fatal("missing varnish_main_sessions grouped metric")
	}

	total := findFamily(families, "varnish_main_sessions_total")
	if total == nil {
		t.Fatal("missing varnish_main_sessions_total metric")
	}
}

// --- Unit tests for internal functions ---

func TestNormalizeBackendName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"boot.my_backend", "my_backend"},
		{"root.my_backend", "my_backend"},
		{"kv_reload_3.my_backend", "my_backend"},
		{"reload_20240101_120000_123.my_backend", "my_backend"},
		{"my_backend", "my_backend"},
		{".my_backend.", "my_backend"},
		{"boot.reload_1.nested", "reload_1.nested"},
	}

	for _, tt := range tests {
		if got := normalizeBackendName(tt.input); got != tt.want {
			t.Errorf("normalizeBackendName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMapGroupName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"MAIN.cache_hit", "main"},
		{"VBE.boot.default.happy", "backend"},
		{"SMA.s0.g_alloc", "sma"},
		{"LCK.sms.creat", "lck"},
		{"MEMPOOL.sess0.live", "mempool"},
		{"MGT.uptime", "mgt"},
	}

	for _, tt := range tests {
		if got := mapGroupName(tt.input); got != tt.want {
			t.Errorf("mapGroupName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNewestVBEReloadTag(t *testing.T) {
	t.Parallel()

	counters := map[string]varnishstatCounter{
		"VBE.kv_reload_1.a(1.2.3.4,,80).happy": {},
		"VBE.kv_reload_2.b(1.2.3.5,,80).happy": {},
		"VBE.kv_reload_2.b(1.2.3.5,,80).fail":  {},
		"MAIN.uptime":                          {},
	}

	got := newestVBEReloadTag(counters)
	if got != "VBE.kv_reload_2" {
		t.Errorf("newestVBEReloadTag() = %q, want VBE.kv_reload_2", got)
	}
}

func TestNewestVBEReloadTagNoReload(t *testing.T) {
	t.Parallel()

	counters := map[string]varnishstatCounter{
		"VBE.boot.default(127.0.0.1,,8080).happy": {},
		"MAIN.uptime": {},
	}

	got := newestVBEReloadTag(counters)
	if got != "" {
		t.Errorf("newestVBEReloadTag() = %q, want empty", got)
	}
}

func TestIsStaleBackendCounter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		latestReload string
		want         bool
	}{
		{"VBE.kv_reload_1.foo.happy", "VBE.kv_reload_2", true},
		{"VBE.kv_reload_2.foo.happy", "VBE.kv_reload_2", false},
		{"VBE.boot.foo.happy", "VBE.kv_reload_2", true},
		{"MAIN.uptime", "VBE.kv_reload_2", false},
		{"VBE.boot.foo.happy", "", false},
	}

	for _, tt := range tests {
		if got := isStaleBackendCounter(tt.name, tt.latestReload); got != tt.want {
			t.Errorf("isStaleBackendCounter(%q, %q) = %v, want %v", tt.name, tt.latestReload, got, tt.want)
		}
	}
}

func TestBackendLabelsFromIdentParenthesized(t *testing.T) {
	t.Parallel()

	keys, values := backendLabelsFromIdent("boot.my_backend(127.0.0.1,,8080)")
	if !slices.Equal(keys, []string{"backend", "server"}) {
		t.Errorf("keys = %v, want [backend server]", keys)
	}
	if values[0] != "my_backend" {
		t.Errorf("backend = %q, want my_backend", values[0])
	}
	if values[1] != "127.0.0.1:8080" {
		t.Errorf("server = %q, want 127.0.0.1:8080", values[1])
	}
}

func TestBackendLabelsFromIdentPlain(t *testing.T) {
	t.Parallel()

	keys, values := backendLabelsFromIdent("boot.default")
	if values[0] != "default" {
		t.Errorf("backend = %q, want default", values[0])
	}
	if values[1] != "unknown" {
		t.Errorf("server = %q, want unknown", values[1])
	}
	if !slices.Equal(keys, []string{"backend", "server"}) {
		t.Errorf("keys = %v, want [backend server]", keys)
	}
}

func TestResolveMetricMAIN(t *testing.T) {
	t.Parallel()

	name, _, keys, values := resolveMetric("MAIN.cache_hit", "main", "", "Cache hits")
	if name != "varnish_main_cache_hit" {
		t.Errorf("name = %q, want varnish_main_cache_hit", name)
	}
	if len(keys) != 0 {
		t.Errorf("expected no labels for MAIN counter, got keys=%v values=%v", keys, values)
	}
}

func TestResolveMetricSMA(t *testing.T) {
	t.Parallel()

	name, _, keys, values := resolveMetric("SMA.s0.g_alloc", "sma", "s0", "Allocations")
	if name != "varnish_sma_g_alloc" {
		t.Errorf("name = %q, want varnish_sma_g_alloc", name)
	}
	if !slices.Equal(keys, []string{"type"}) {
		t.Errorf("keys = %v, want [type]", keys)
	}
	if !slices.Equal(values, []string{"s0"}) {
		t.Errorf("values = %v, want [s0]", values)
	}
}

func TestResolveMetricLCKRename(t *testing.T) {
	t.Parallel()

	name, _, keys, values := resolveMetric("LCK.sms.creat", "lck", "sms", "Created")
	if name != "varnish_lock_created" {
		t.Errorf("name = %q, want varnish_lock_created", name)
	}
	if !slices.Equal(keys, []string{"target"}) {
		t.Errorf("keys = %v, want [target]", keys)
	}
	if !slices.Equal(values, []string{"sms"}) {
		t.Errorf("values = %v, want [sms]", values)
	}
}

func TestResolveMetricVBE(t *testing.T) {
	t.Parallel()

	name, _, keys, values := resolveMetric(
		"VBE.boot.my_backend(10.0.0.1,,80).happy",
		"backend",
		"boot.my_backend(10.0.0.1,,80)",
		"Happy health probes",
	)
	if name != "varnish_backend_happy" {
		t.Errorf("name = %q, want varnish_backend_happy", name)
	}
	if !slices.Equal(keys, []string{"backend", "server"}) {
		t.Errorf("keys = %v, want [backend server]", keys)
	}
	if values[0] != "my_backend" {
		t.Errorf("backend = %q, want my_backend", values[0])
	}
	if values[1] != "10.0.0.1:80" {
		t.Errorf("server = %q, want 10.0.0.1:80", values[1])
	}
}

func TestParseVarnishstatV7(t *testing.T) {
	t.Parallel()

	data := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit": {"value": 42, "flag": "c", "description": "Cache hits"},
			"SMA.s0.g_alloc": {"value": 10, "flag": "g", "description": "Allocs", "ident": "s0"}
		}
	}`

	counters, err := parseVarnishstatV7(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV7() error: %v", err)
	}
	if len(counters) != 2 {
		t.Fatalf("len(counters) = %d, want 2", len(counters))
	}
	if counters["MAIN.cache_hit"].Value != 42 {
		t.Errorf("cache_hit value = %v, want 42", counters["MAIN.cache_hit"].Value)
	}
	if counters["SMA.s0.g_alloc"].Ident != "s0" {
		t.Errorf("SMA ident = %q, want s0", counters["SMA.s0.g_alloc"].Ident)
	}
}

func TestParseVarnishstatV7InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := parseVarnishstatV7("not json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseVarnishstatV7SkipsMalformedValue(t *testing.T) {
	t.Parallel()

	// Counter with a non-numeric value should be skipped (Float64 error),
	// while the valid counter is still returned.
	data := `{
		"counters": {
			"MAIN.bad_value": {"value": "not_a_number", "flag": "c", "description": "bad"},
			"MAIN.good_value": {"value": 42, "flag": "c", "description": "good"}
		}
	}`

	counters, err := parseVarnishstatV7(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV7() error: %v", err)
	}

	if _, ok := counters["MAIN.bad_value"]; ok {
		t.Error("expected malformed counter to be skipped")
	}

	if _, ok := counters["MAIN.good_value"]; !ok {
		t.Error("expected valid counter to be present")
	}
}

func TestParseVarnishstatV6(t *testing.T) {
	t.Parallel()

	data := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.cache_hit": {"value": 100, "flag": "c", "description": "Cache hits"},
		"SMA.s0.g_alloc": {"value": 5, "flag": "g", "description": "Allocs", "ident": "s0"}
	}`

	counters, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV6() error: %v", err)
	}
	if len(counters) != 2 {
		t.Fatalf("len(counters) = %d, want 2", len(counters))
	}
	if counters["SMA.s0.g_alloc"].Ident != "s0" {
		t.Errorf("SMA ident = %q, want s0", counters["SMA.s0.g_alloc"].Ident)
	}
}

func TestParseVarnishstatV6SkipsNonCounterKeys(t *testing.T) {
	t.Parallel()

	data := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.cache_hit": {"value": 1, "flag": "c", "description": ""}
	}`

	counters, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV6() error: %v", err)
	}
	if _, ok := counters["timestamp"]; ok {
		t.Error("expected timestamp key to be skipped")
	}
	if len(counters) != 1 {
		t.Errorf("len(counters) = %d, want 1", len(counters))
	}
}

func TestParseVarnishstatV6InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := parseVarnishstatV6("not json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseVarnishstatV6SkipsMalformedEntry(t *testing.T) {
	t.Parallel()

	// An entry whose JSON structure doesn't match the expected schema
	// should be skipped, while valid entries are still returned.
	data := `{
		"MAIN.bad_entry": "not an object",
		"MAIN.good_entry": {"value": 10, "flag": "c", "description": "good"}
	}`

	counters, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV6() error: %v", err)
	}

	if _, ok := counters["MAIN.bad_entry"]; ok {
		t.Error("expected malformed entry to be skipped")
	}

	if _, ok := counters["MAIN.good_entry"]; !ok {
		t.Error("expected valid entry to be present")
	}
}

func TestParseVarnishstatV6SkipsMalformedValue(t *testing.T) {
	t.Parallel()

	// An entry with a non-numeric value should be skipped (Float64 error).
	data := `{
		"MAIN.bad_value": {"value": "not_a_number", "flag": "c", "description": "bad"},
		"MAIN.good_value": {"value": 7, "flag": "c", "description": "good"}
	}`

	counters, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV6() error: %v", err)
	}

	if _, ok := counters["MAIN.bad_value"]; ok {
		t.Error("expected counter with malformed value to be skipped")
	}

	if _, ok := counters["MAIN.good_value"]; !ok {
		t.Error("expected valid counter to be present")
	}
}

func TestSanitizeMetricName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"MAIN.cache_hit", "varnish_main_cache_hit"},
		{"SMA.s0.g_alloc", "varnish_sma_s0_g_alloc"},
		{"VBE.default(127.0.0.1,,8080).happy", "varnish_vbe_default_127_0_0_1__8080__happy"},
	}

	for _, tt := range tests {
		if got := sanitizeMetricName(tt.input); got != tt.want {
			t.Errorf("sanitizeMetricName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveMetricIdentDerivedFromName(t *testing.T) {
	t.Parallel()

	// When ident is empty and name has >1 dot, ident is derived from the middle segments.
	name, _, keys, values := resolveMetric("SMA.s0.g_alloc", "sma", "", "Allocations")
	if name != "varnish_sma_g_alloc" {
		t.Errorf("name = %q, want varnish_sma_g_alloc", name)
	}
	if !slices.Equal(keys, []string{"type"}) {
		t.Errorf("keys = %v, want [type]", keys)
	}
	if !slices.Equal(values, []string{"s0"}) {
		t.Errorf("values = %v, want [s0]", values)
	}
}

func TestVarnishstatCollectorVBEFilterUsesUppercaseGroup(t *testing.T) {
	t.Parallel()

	// VBE group is reported as "backend" internally, but the filter
	// should match using the original uppercase group name "VBE".
	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.default(127.0.0.1,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "",
				"ident": "boot.default(127.0.0.1,,80)"
			},
			"MAIN.cache_hit": {"value": 1, "flag": "c", "description": ""}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, []string{"VBE"})

	families := collectMetrics(t, c)
	if findFamily(families, "varnish_backend_up") == nil {
		t.Error("expected varnish_backend_up to be present with VBE filter")
	}
	if findFamily(families, "varnish_main_cache_hit") != nil {
		t.Error("expected varnish_main_cache_hit to be filtered out with VBE filter")
	}
}

// --- V6 format end-to-end tests ---

func TestVarnishstatCollectorV6VBELabelsAndBackendUp(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"VBE.boot.my_backend(10.0.0.1,,80).happy": {
			"value": 18446744073709551615, "flag": "b", "description": "Happy probes",
			"ident": "boot.my_backend(10.0.0.1,,80)"
		},
		"VBE.boot.my_backend(10.0.0.1,,80).fail": {
			"value": 3, "flag": "c", "description": "Fail count",
			"ident": "boot.my_backend(10.0.0.1,,80)"
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 6, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up in V6 format")
	}
	if got := labelValue(up, "backend"); got != "my_backend" {
		t.Errorf("V6 backend label = %q, want my_backend", got)
	}
	if got := labelValue(up, "server"); got != "10.0.0.1:80" {
		t.Errorf("V6 server label = %q, want 10.0.0.1:80", got)
	}
	if got := up.GetMetric()[0].GetGauge().GetValue(); got != 1 {
		t.Errorf("V6 backend_up = %v, want 1", got)
	}

	fail := findFamily(families, "varnish_backend_fail")
	if fail == nil {
		t.Fatal("missing varnish_backend_fail in V6 format")
	}
	if got := fail.GetMetric()[0].GetCounter().GetValue(); got != 3 {
		t.Errorf("V6 backend_fail = %v, want 3", got)
	}
}

func TestVarnishstatCollectorV6VBEReloadFiltering(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"VBE.kv_reload_1.old(10.0.0.1,,80).happy": {
			"value": 18446744073709551615, "flag": "b", "description": "",
			"ident": "kv_reload_1.old(10.0.0.1,,80)"
		},
		"VBE.kv_reload_2.current(10.0.0.2,,80).happy": {
			"value": 18446744073709551615, "flag": "b", "description": "",
			"ident": "kv_reload_2.current(10.0.0.2,,80)"
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 6, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up in V6 reload filtering")
	}
	if len(up.GetMetric()) != 1 {
		t.Errorf("expected 1 backend_up metric after V6 reload filtering, got %d", len(up.GetMetric()))
	}
	if got := labelValue(up, "backend"); got != "current" {
		t.Errorf("V6 reload filter backend = %q, want current", got)
	}
}

func TestVarnishstatCollectorV6MetricGrouping(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.fetch_head": {"value": 10, "flag": "c", "description": "Fetch head"},
		"MAIN.s_fetch": {"value": 100, "flag": "c", "description": "Total fetches"},
		"MAIN.sess_conn": {"value": 50, "flag": "c", "description": "Sessions accepted"},
		"MAIN.s_sess": {"value": 200, "flag": "c", "description": "Total sessions"}
	}`

	fn := func() (string, int, error) { return jsonOutput, 6, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	if findFamily(families, "varnish_main_fetch") == nil {
		t.Error("missing varnish_main_fetch in V6 format")
	}
	if findFamily(families, "varnish_main_fetch_total") == nil {
		t.Error("missing varnish_main_fetch_total in V6 format")
	}
	if findFamily(families, "varnish_main_sessions") == nil {
		t.Error("missing varnish_main_sessions in V6 format")
	}
	if findFamily(families, "varnish_main_sessions_total") == nil {
		t.Error("missing varnish_main_sessions_total in V6 format")
	}
}

func TestVarnishstatCollectorV6SMAAndLCKLabels(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"SMA.s0.g_alloc": {"value": 42, "flag": "g", "description": "Allocs", "ident": "s0"},
		"LCK.sms.colls": {"value": 7, "flag": "c", "description": "Collisions", "ident": "sms"}
	}`

	fn := func() (string, int, error) { return jsonOutput, 6, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	alloc := findFamily(families, "varnish_sma_g_alloc")
	if alloc == nil {
		t.Fatal("missing varnish_sma_g_alloc in V6 format")
	}
	if got := labelValue(alloc, "type"); got != "s0" {
		t.Errorf("V6 SMA type label = %q, want s0", got)
	}

	colls := findFamily(families, "varnish_lock_collisions")
	if colls == nil {
		t.Fatal("missing varnish_lock_collisions (renamed from lck_colls) in V6 format")
	}
	if got := labelValue(colls, "target"); got != "sms" {
		t.Errorf("V6 LCK target label = %q, want sms", got)
	}
}

// --- Worker threads grouping ---

func TestVarnishstatCollectorWorkerThreadGrouping(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.n_wrk": {"value": 100, "flag": "g", "description": "Total worker threads"},
			"MAIN.n_wrk_create": {"value": 50, "flag": "c", "description": "Workers created"},
			"MAIN.n_wrk_failed": {"value": 2, "flag": "c", "description": "Worker creation failures"},
			"MAIN.n_wrk_queued": {"value": 0, "flag": "c", "description": "Queued work requests"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	// n_wrk_create, n_wrk_failed, n_wrk_queued → varnish_main_worker_threads{type=...}
	wt := findFamily(families, "varnish_main_worker_threads")
	if wt == nil {
		t.Fatal("missing varnish_main_worker_threads grouped metric")
	}
	// n_wrk_queued has value=0 and flag="c", so it is skipped (zero-value counter).
	if len(wt.GetMetric()) != 2 {
		t.Errorf("expected 2 worker_threads metrics (zero-value counter skipped), got %d", len(wt.GetMetric()))
	}

	// n_wrk (the total suffix) → varnish_main_worker_threads_total
	total := findFamily(families, "varnish_main_worker_threads_total")
	if total == nil {
		t.Fatal("missing varnish_main_worker_threads_total metric")
	}
}

// --- Zero-value counter skipping ---

func TestVarnishstatCollectorSkipsZeroCounters(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit":  {"value": 0, "flag": "c", "description": "Cache hits"},
			"MAIN.cache_miss": {"value": 5, "flag": "c", "description": "Cache misses"},
			"MAIN.s_req":      {"value": 0, "flag": "a", "description": "Total requests (accumulator, zero)"},
			"MAIN.s_pipe":     {"value": 3, "flag": "a", "description": "Total pipe (accumulator, non-zero)"},
			"MAIN.n_object":   {"value": 0, "flag": "g", "description": "Object count"},
			"VBE.boot.default(10.0.0.1,,80).happy": {
				"value": 0, "flag": "b", "description": "Happy bitmap",
				"ident": "boot.default(10.0.0.1,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	// Counter with value 0 must be skipped.
	if mf := findFamily(families, "varnish_main_cache_hit"); mf != nil {
		t.Error("zero-value counter varnish_main_cache_hit should be skipped")
	}

	// Counter with value > 0 must be emitted.
	if mf := findFamily(families, "varnish_main_cache_miss"); mf == nil {
		t.Error("non-zero counter varnish_main_cache_miss should be emitted")
	}

	// Accumulator (flag "a") with value 0 must be skipped, same as flag "c".
	if mf := findFamily(families, "varnish_main_s_req"); mf != nil {
		t.Error("zero-value accumulator varnish_main_s_req should be skipped")
	}

	// Accumulator (flag "a") with value > 0 must be emitted.
	if mf := findFamily(families, "varnish_main_s_pipe"); mf == nil {
		t.Error("non-zero accumulator varnish_main_s_pipe should be emitted")
	}

	// Gauge with value 0 must still be emitted.
	if mf := findFamily(families, "varnish_main_n_object"); mf == nil {
		t.Error("zero-value gauge varnish_main_n_object should still be emitted")
	}

	// backend_up should be synthesized from the bitmap (value=0 → bit 0 clear → up=0).
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("varnish_backend_up should be synthesized from zero-value bitmap")
	}
	if got := up.GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Errorf("varnish_backend_up = %v, want 0", got)
	}
}

// --- Bitmap flag type and backend_up edge values ---

func TestVarnishstatCollectorBitmapEmitsUpGauge(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.default(10.0.0.1,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "Happy bitmap",
				"ident": "boot.default(10.0.0.1,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	// The raw bitmap should not be emitted.
	if mf := findFamily(families, "varnish_backend_happy"); mf != nil {
		t.Error("varnish_backend_happy should not be emitted (replaced by backend_up)")
	}

	// Only the derived backend_up gauge should appear.
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}
	if got := up.GetType(); got != dto.MetricType_GAUGE {
		t.Errorf("backend_up metric type = %v, want GAUGE", got)
	}
}

func TestVarnishstatCollectorBackendUpEdgeValues(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.bit0_set(10.0.0.1,,80).happy": {
				"value": 1, "flag": "b", "description": "",
				"ident": "boot.bit0_set(10.0.0.1,,80)"
			},
			"VBE.boot.bit0_clear(10.0.0.2,,80).happy": {
				"value": 2, "flag": "b", "description": "",
				"ident": "boot.bit0_clear(10.0.0.2,,80)"
			},
			"VBE.boot.all_bits(10.0.0.3,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "",
				"ident": "boot.all_bits(10.0.0.3,,80)"
			},
			"VBE.boot.zero(10.0.0.4,,80).happy": {
				"value": 0, "flag": "b", "description": "",
				"ident": "boot.zero(10.0.0.4,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}
	if len(up.GetMetric()) != 4 {
		t.Fatalf("expected 4 backend_up metrics, got %d", len(up.GetMetric()))
	}

	want := map[string]float64{
		"bit0_set":   1, // value=1, bit 0 set → up
		"bit0_clear": 0, // value=2, bit 0 clear → down
		"all_bits":   1, // value=max uint64, bit 0 set → up
		"zero":       0, // value=0, bit 0 clear → down
	}

	for _, m := range up.GetMetric() {
		var backend string
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "backend" {
				backend = lp.GetValue()
			}
		}
		expected, ok := want[backend]
		if !ok {
			t.Errorf("unexpected backend label: %q", backend)

			continue
		}
		if got := m.GetGauge().GetValue(); got != expected {
			t.Errorf("backend_up{backend=%s} = %v, want %v", backend, got, expected)
		}
	}
}

func TestVarnishstatCollectorBackendUpMalformedBitmap(t *testing.T) {
	t.Parallel()

	// A happy bitmap with a floating-point RawValue passes Float64() but
	// fails ParseUint, so the code should default to up=0.
	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.boot.bad(10.0.0.1,,80).happy": {
				"value": 1.5, "flag": "b", "description": "",
				"ident": "boot.bad(10.0.0.1,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric")
	}

	m := up.GetMetric()[0]
	if got := m.GetGauge().GetValue(); got != 0 {
		t.Errorf("backend_up = %v, want 0 for malformed bitmap", got)
	}
}

// --- UUID backend format ---

func TestVarnishstatCollectorVBELabelsUUID(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"VBE.12345678-abcd-1234-9abc-123456789012.my_backend.happy": {
				"value": 18446744073709551615, "flag": "b", "description": "Happy probes",
				"ident": "12345678-abcd-1234-9abc-123456789012.my_backend"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	up := findFamily(families, "varnish_backend_up")
	if up == nil {
		t.Fatal("missing varnish_backend_up metric for UUID backend")
	}
	if got := labelValue(up, "backend"); got != "my_backend" {
		t.Errorf("UUID backend label = %q, want my_backend", got)
	}
	if got := labelValue(up, "server"); got != "12345678-abcd-1234-9abc-123456789012" {
		t.Errorf("UUID server label = %q, want 12345678-abcd-1234-9abc-123456789012", got)
	}
}

// --- SMF labels ---

func TestVarnishstatCollectorSMFLabels(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"SMF.s0.g_space": {"value": 1024, "flag": "g", "description": "Bytes available", "ident": "s0"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)
	space := findFamily(families, "varnish_smf_g_space")
	if space == nil {
		t.Fatal("missing varnish_smf_g_space metric")
	}
	if got := labelValue(space, "type"); got != "s0" {
		t.Errorf("SMF type label = %q, want s0", got)
	}
}

// --- MGT group ---

func TestVarnishstatCollectorMGTGroup(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"MGT.uptime": {"value": 3600, "flag": "c", "description": "Management process uptime"},
			"MGT.child_start": {"value": 1, "flag": "c", "description": "Child process started"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	uptime := findFamily(families, "varnish_mgt_uptime")
	if uptime == nil {
		t.Fatal("missing varnish_mgt_uptime metric")
	}
	if got := uptime.GetMetric()[0].GetCounter().GetValue(); got != 3600 {
		t.Errorf("varnish_mgt_uptime = %v, want 3600", got)
	}

	start := findFamily(families, "varnish_mgt_child_start")
	if start == nil {
		t.Fatal("missing varnish_mgt_child_start metric")
	}
}

// --- All LCK renames ---

func TestVarnishstatCollectorAllLCKRenames(t *testing.T) {
	t.Parallel()

	jsonOutput := `{
		"version": 1,
		"counters": {
			"LCK.sms.colls":   {"value": 5, "flag": "c", "description": "Collisions", "ident": "sms"},
			"LCK.sms.creat":   {"value": 1, "flag": "c", "description": "Created", "ident": "sms"},
			"LCK.sms.destroy": {"value": 7, "flag": "c", "description": "Destroyed", "ident": "sms"},
			"LCK.sms.locks":   {"value": 999, "flag": "c", "description": "Lock ops", "ident": "sms"}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	renames := map[string]float64{
		"varnish_lock_collisions": 5,
		"varnish_lock_created":    1,
		"varnish_lock_destroyed":  7,
		"varnish_lock_operations": 999,
	}

	for name, wantVal := range renames {
		mf := findFamily(families, name)
		if mf == nil {
			t.Errorf("missing %s metric", name)

			continue
		}
		if got := labelValue(mf, "target"); got != "sms" {
			t.Errorf("%s target label = %q, want sms", name, got)
		}
		if got := mf.GetMetric()[0].GetCounter().GetValue(); got != wantVal {
			t.Errorf("%s = %v, want %v", name, got, wantVal)
		}
	}
}

// --- Comprehensive multi-group test ---

func TestVarnishstatCollectorV7Comprehensive(t *testing.T) {
	t.Parallel()

	// Realistic output spanning all major groups.
	jsonOutput := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit": {"value": 5000, "flag": "c", "description": "Cache hits"},
			"MAIN.cache_miss": {"value": 200, "flag": "c", "description": "Cache misses"},
			"MAIN.n_object": {"value": 150, "flag": "g", "description": "Num objects"},
			"MAIN.uptime": {"value": 86400, "flag": "c", "description": "Uptime in seconds"},
			"MAIN.fetch_head": {"value": 10, "flag": "c", "description": "Fetch head"},
			"MAIN.s_fetch": {"value": 300, "flag": "c", "description": "Total fetches"},
			"MAIN.sess_conn": {"value": 500, "flag": "c", "description": "Sessions accepted"},
			"MAIN.s_sess": {"value": 500, "flag": "c", "description": "Total sessions"},
			"MAIN.n_wrk": {"value": 4, "flag": "g", "description": "Worker threads"},
			"MAIN.n_wrk_create": {"value": 4, "flag": "c", "description": "Workers created"},
			"MGT.uptime": {"value": 86410, "flag": "c", "description": "MGT uptime"},
			"SMA.s0.g_alloc": {"value": 42, "flag": "g", "description": "Allocs", "ident": "s0"},
			"SMA.Transient.g_alloc": {"value": 100, "flag": "g", "description": "Allocs", "ident": "Transient"},
			"SMF.s0.g_space": {"value": 1024, "flag": "g", "description": "Space", "ident": "s0"},
			"LCK.sms.colls": {"value": 3, "flag": "c", "description": "Collisions", "ident": "sms"},
			"MEMPOOL.sess0.live": {"value": 5, "flag": "g", "description": "Live", "ident": "sess0"},
			"VBE.kv_reload_2.backend_a(10.0.0.1,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "Happy",
				"ident": "kv_reload_2.backend_a(10.0.0.1,,80)"
			},
			"VBE.kv_reload_2.backend_b(10.0.0.2,,80).happy": {
				"value": 0, "flag": "b", "description": "Happy",
				"ident": "kv_reload_2.backend_b(10.0.0.2,,80)"
			},
			"VBE.kv_reload_1.old_backend(10.0.0.3,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "Happy",
				"ident": "kv_reload_1.old_backend(10.0.0.3,,80)"
			}
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 7, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	// Meta-metrics.
	if findFamily(families, "varnish_up") == nil {
		t.Error("missing varnish_up")
	}
	if findFamily(families, "varnish_scrape_duration_seconds") == nil {
		t.Error("missing varnish_scrape_duration_seconds")
	}

	// MAIN counters.
	if findFamily(families, "varnish_main_cache_hit") == nil {
		t.Error("missing varnish_main_cache_hit")
	}
	if findFamily(families, "varnish_main_cache_miss") == nil {
		t.Error("missing varnish_main_cache_miss")
	}
	if findFamily(families, "varnish_main_n_object") == nil {
		t.Error("missing varnish_main_n_object")
	}

	// Groupings.
	if findFamily(families, "varnish_main_fetch") == nil {
		t.Error("missing varnish_main_fetch (grouped)")
	}
	if findFamily(families, "varnish_main_fetch_total") == nil {
		t.Error("missing varnish_main_fetch_total")
	}
	if findFamily(families, "varnish_main_sessions") == nil {
		t.Error("missing varnish_main_sessions (grouped)")
	}
	if findFamily(families, "varnish_main_worker_threads") == nil {
		t.Error("missing varnish_main_worker_threads (grouped)")
	}
	if findFamily(families, "varnish_main_worker_threads_total") == nil {
		t.Error("missing varnish_main_worker_threads_total")
	}

	// MGT.
	if findFamily(families, "varnish_mgt_uptime") == nil {
		t.Error("missing varnish_mgt_uptime")
	}

	// SMA with multiple idents.
	sma := findFamily(families, "varnish_sma_g_alloc")
	if sma == nil {
		t.Error("missing varnish_sma_g_alloc")
	} else if len(sma.GetMetric()) != 2 {
		t.Errorf("expected 2 sma_g_alloc metrics (s0 + Transient), got %d", len(sma.GetMetric()))
	}

	// SMF.
	if findFamily(families, "varnish_smf_g_space") == nil {
		t.Error("missing varnish_smf_g_space")
	}

	// LCK rename.
	if findFamily(families, "varnish_lock_collisions") == nil {
		t.Error("missing varnish_lock_collisions")
	}

	// MEMPOOL.
	if findFamily(families, "varnish_mempool_live") == nil {
		t.Error("missing varnish_mempool_live")
	}

	// VBE: kv_reload_1 should be filtered; only kv_reload_2 backends present.
	backendUp := findFamily(families, "varnish_backend_up")
	if backendUp == nil {
		t.Fatal("missing varnish_backend_up")
	}
	if len(backendUp.GetMetric()) != 2 {
		t.Errorf("expected 2 backend_up metrics, got %d", len(backendUp.GetMetric()))
	}
}

func TestVarnishstatCollectorV6Comprehensive(t *testing.T) {
	t.Parallel()

	// Same broad coverage but in V6 flat format.
	jsonOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.cache_hit": {"value": 5000, "flag": "c", "description": "Cache hits"},
		"MAIN.n_object": {"value": 150, "flag": "g", "description": "Num objects"},
		"MAIN.n_wrk": {"value": 4, "flag": "g", "description": "Worker threads"},
		"MAIN.n_wrk_create": {"value": 4, "flag": "c", "description": "Workers created"},
		"MGT.uptime": {"value": 86410, "flag": "c", "description": "MGT uptime"},
		"SMA.s0.g_alloc": {"value": 42, "flag": "g", "description": "Allocs", "ident": "s0"},
		"LCK.sms.locks": {"value": 99, "flag": "c", "description": "Locks", "ident": "sms"},
		"MEMPOOL.sess0.live": {"value": 5, "flag": "g", "description": "Live", "ident": "sess0"},
		"VBE.boot.default(10.0.0.1,,80).happy": {
			"value": 18446744073709551615, "flag": "b", "description": "Happy",
			"ident": "boot.default(10.0.0.1,,80)"
		}
	}`

	fn := func() (string, int, error) { return jsonOutput, 6, nil }
	c := NewVarnishstatCollector(fn, nil)

	families := collectMetrics(t, c)

	checks := []string{
		"varnish_up",
		"varnish_main_cache_hit",
		"varnish_main_n_object",
		"varnish_main_worker_threads",
		"varnish_main_worker_threads_total",
		"varnish_mgt_uptime",
		"varnish_sma_g_alloc",
		"varnish_lock_operations",
		"varnish_mempool_live",
		"varnish_backend_up",
	}

	for _, name := range checks {
		if findFamily(families, name) == nil {
			t.Errorf("missing %s in V6 comprehensive test", name)
		}
	}
}

// --- Parser: RawValue preserved for bitmap parsing ---

func TestParseVarnishstatV7PreservesRawValue(t *testing.T) {
	t.Parallel()

	data := `{
		"version": 1,
		"counters": {
			"VBE.boot.default(10.0.0.1,,80).happy": {
				"value": 18446744073709551615, "flag": "b", "description": "Happy",
				"ident": "boot.default(10.0.0.1,,80)"
			}
		}
	}`

	counters, err := parseVarnishstatV7(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV7() error: %v", err)
	}

	c := counters["VBE.boot.default(10.0.0.1,,80).happy"]
	if c.RawValue != "18446744073709551615" {
		t.Errorf("RawValue = %q, want 18446744073709551615", c.RawValue)
	}
	if c.Flag != "b" {
		t.Errorf("Flag = %q, want b", c.Flag)
	}
}

func TestParseVarnishstatV6PreservesRawValue(t *testing.T) {
	t.Parallel()

	data := `{
		"timestamp": "2024-01-01T00:00:00",
		"VBE.boot.default(10.0.0.1,,80).happy": {
			"value": 18446744073709551615, "flag": "b", "description": "Happy",
			"ident": "boot.default(10.0.0.1,,80)"
		}
	}`

	counters, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("parseVarnishstatV6() error: %v", err)
	}

	c := counters["VBE.boot.default(10.0.0.1,,80).happy"]
	if c.RawValue != "18446744073709551615" {
		t.Errorf("RawValue = %q, want 18446744073709551615", c.RawValue)
	}
}

// --- Unit test: backendLabelsFromIdent UUID ---

func TestBackendLabelsFromIdentUUID(t *testing.T) {
	t.Parallel()

	keys, values := backendLabelsFromIdent("12345678-abcd-1234-9abc-123456789012.my_backend")
	if !slices.Equal(keys, []string{"backend", "server"}) {
		t.Errorf("keys = %v, want [backend server]", keys)
	}
	if values[0] != "my_backend" {
		t.Errorf("backend = %q, want my_backend", values[0])
	}
	if values[1] != "12345678-abcd-1234-9abc-123456789012" {
		t.Errorf("server = %q, want 12345678-abcd-1234-9abc-123456789012", values[1])
	}
}

// --- Unit test: dropFirstSegment ---

func TestDropFirstSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"vbe.kv_reload_1.foo.happy", "kv_reload_1.foo.happy"},
		{"main.cache_hit", "cache_hit"},
		{"noDot", "noDot"},
		{".leading", "leading"},
	}

	for _, tt := range tests {
		if got := dropFirstSegment(tt.input); got != tt.want {
			t.Errorf("dropFirstSegment(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

// benchmarkCounterEntries builds a realistic set of ~160 varnishstat JSON
// counter entries spanning all major groups (MAIN, MGT, SMA, LCK, MEMPOOL,
// VBE) including reload-stale backends that exercise filtering.
func benchmarkCounterEntries() []string {
	entries := make([]string, 0, 200)

	add := func(name, flag, desc string, val int64) {
		entries = append(entries, fmt.Sprintf(
			`%q:{"value":%d,"flag":%q,"description":%q}`,
			name, val, flag, desc,
		))
	}
	addIdent := func(name, flag, desc, ident string, val int64) {
		entries = append(entries, fmt.Sprintf(
			`%q:{"value":%d,"flag":%q,"description":%q,"ident":%q}`,
			name, val, flag, desc, ident,
		))
	}
	addBitmap := func(name, desc, ident string) {
		entries = append(entries, fmt.Sprintf(
			`%q:{"value":18446744073709551615,"flag":"b","description":%q,"ident":%q}`,
			name, desc, ident,
		))
	}

	// MAIN counters (~45).
	add("MAIN.cache_hit", "c", "Cache hits", 50000)
	add("MAIN.cache_hitpass", "c", "Cache hitpass", 100)
	add("MAIN.cache_hitmiss", "c", "Cache hitmiss", 50)
	add("MAIN.cache_miss", "c", "Cache misses", 2000)
	add("MAIN.s_sess", "c", "Total sessions seen", 10000)
	add("MAIN.s_pipe", "c", "Total pipe sessions", 5)
	add("MAIN.s_pass", "c", "Total pass-ed requests", 1000)
	add("MAIN.s_fetch", "c", "Total backend fetches", 3000)
	add("MAIN.s_synth", "c", "Total synthetic responses", 10)
	add("MAIN.s_req_hdrbytes", "c", "Request header bytes", 5242880)
	add("MAIN.s_req_bodybytes", "c", "Request body bytes", 1048576)
	add("MAIN.s_resp_hdrbytes", "c", "Response header bytes", 10485760)
	add("MAIN.s_resp_bodybytes", "c", "Response body bytes", 104857600)
	add("MAIN.sess_conn", "c", "Sessions accepted", 10000)
	add("MAIN.sess_fail", "c", "Session accept failures", 0)
	add("MAIN.sess_queued", "c", "Sessions queued for thread", 50)
	add("MAIN.sess_dropped", "c", "Sessions dropped", 0)
	add("MAIN.n_object", "g", "Object structs", 150)
	add("MAIN.n_objectcore", "g", "Objectcore structs", 200)
	add("MAIN.n_objecthead", "g", "Objecthead structs", 180)
	add("MAIN.n_wrk", "g", "Worker threads", 200)
	add("MAIN.n_wrk_create", "c", "Worker threads created", 200)
	add("MAIN.n_wrk_failed", "c", "Worker thread creation failed", 0)
	add("MAIN.n_wrk_queued", "c", "Work requests queued", 50)
	add("MAIN.n_wrk_drop", "c", "Work requests dropped", 0)
	add("MAIN.n_backend", "g", "Number of backends", 5)
	add("MAIN.n_expired", "c", "Number of expired objects", 500)
	add("MAIN.n_lru_nuked", "c", "Number of LRU nuked objects", 100)
	add("MAIN.fetch_head", "c", "Fetch head", 100)
	add("MAIN.fetch_length", "c", "Fetch with length", 2000)
	add("MAIN.fetch_chunked", "c", "Fetch chunked", 800)
	add("MAIN.fetch_eof", "c", "Fetch EOF", 50)
	add("MAIN.fetch_bad", "c", "Fetch bad headers", 0)
	add("MAIN.fetch_none", "c", "Fetch no body", 50)
	add("MAIN.fetch_1xx", "c", "Fetch 1xx", 0)
	add("MAIN.fetch_204", "c", "Fetch 204", 10)
	add("MAIN.fetch_304", "c", "Fetch 304", 500)
	add("MAIN.pools", "g", "Number of thread pools", 2)
	add("MAIN.threads", "g", "Total number of threads", 200)
	add("MAIN.threads_limited", "c", "Threads hit max", 0)
	add("MAIN.uptime", "c", "Child process uptime", 86400)
	add("MAIN.client_req", "c", "Good client requests", 52000)
	add("MAIN.bereq_hdrbytes", "c", "Backend request header bytes", 2097152)
	add("MAIN.bereq_bodybytes", "c", "Backend request body bytes", 524288)
	add("MAIN.beresp_hdrbytes", "c", "Backend response header bytes", 4194304)
	add("MAIN.beresp_bodybytes", "c", "Backend response body bytes", 52428800)

	// MGT counters (5).
	add("MGT.uptime", "c", "Management process uptime", 86410)
	add("MGT.child_start", "c", "Child process started", 1)
	add("MGT.child_exit", "c", "Child process normal exit", 0)
	add("MGT.child_stop", "c", "Child process unexpected exit", 0)
	add("MGT.child_died", "c", "Child process died (signal)", 0)

	// SMA counters (2 storage types x 7 stats = 14).
	for _, id := range []string{"s0", "Transient"} {
		for _, stat := range []string{"c_req", "c_fail", "c_bytes", "c_freed", "g_alloc", "g_bytes", "g_space"} {
			flag := "g"
			if stat[0] == 'c' {
				flag = "c"
			}
			addIdent(fmt.Sprintf("SMA.%s.%s", id, stat), flag, "SMA "+stat, id, 1024)
		}
	}

	// LCK counters (11 lock types x 4 stats = 44).
	for _, id := range []string{"sms", "smp", "sma", "smf", "hsl", "hcb", "hcl", "vcl", "sessmem", "backend", "wstat"} {
		for _, stat := range []string{"colls", "creat", "destroy", "locks"} {
			addIdent(fmt.Sprintf("LCK.%s.%s", id, stat), "c", "Lock "+stat, id, 100)
		}
	}

	// MEMPOOL counters (3 pools x 5 stats = 15).
	for _, id := range []string{"sess0", "req0", "busyobj"} {
		addIdent(fmt.Sprintf("MEMPOOL.%s.live", id), "g", "In use", id, 5)
		addIdent(fmt.Sprintf("MEMPOOL.%s.pool", id), "g", "In pool", id, 10)
		addIdent(fmt.Sprintf("MEMPOOL.%s.sz_wanted", id), "g", "Size wanted", id, 512)
		addIdent(fmt.Sprintf("MEMPOOL.%s.allocs", id), "c", "Allocations", id, 1000)
		addIdent(fmt.Sprintf("MEMPOOL.%s.frees", id), "c", "Frees", id, 995)
	}

	// VBE counters: 5 current backends (kv_reload_2) x 6 stats = 30.
	for j := range 5 {
		ident := fmt.Sprintf("kv_reload_2.backend_%d(10.0.0.%d,,80)", j, j+1)
		prefix := "VBE." + ident
		addBitmap(prefix+".happy", "Health status bitmap", ident)
		addIdent(prefix+".bereq_hdrbytes", "c", "Request header bytes", ident, int64(j)*1024)
		addIdent(prefix+".bereq_bodybytes", "c", "Request body bytes", ident, int64(j)*2048)
		addIdent(prefix+".beresp_hdrbytes", "c", "Response header bytes", ident, int64(j)*512)
		addIdent(prefix+".beresp_bodybytes", "c", "Response body bytes", ident, int64(j)*4096)
		addIdent(prefix+".pipe_hdrbytes", "c", "Pipe header bytes", ident, 0)
	}
	// 3 stale backends from older reload (should be filtered out).
	for j := range 3 {
		ident := fmt.Sprintf("kv_reload_1.stale_%d(10.0.1.%d,,80)", j, j+1)
		prefix := "VBE." + ident
		addBitmap(prefix+".happy", "Health status bitmap", ident)
		addIdent(prefix+".bereq_hdrbytes", "c", "Request header bytes", ident, int64(j)*1024)
	}

	return entries
}

func BenchmarkCollect(b *testing.B) {
	entries := benchmarkCounterEntries()
	v7JSON := `{"version":1,"counters":{` + strings.Join(entries, ",") + `}}`
	v6JSON := `{"timestamp":"2024-01-01T00:00:00",` + strings.Join(entries, ",") + `}`

	b.Run("V7", func(b *testing.B) {
		fn := func() (string, int, error) { return v7JSON, 7, nil }
		c := NewVarnishstatCollector(fn, nil)
		ch := make(chan prometheus.Metric, 300)

		// Warm up the descriptor cache.
		c.Collect(ch)
		for len(ch) > 0 {
			<-ch
		}

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			c.Collect(ch)
			for len(ch) > 0 {
				<-ch
			}
		}
	})

	b.Run("V6", func(b *testing.B) {
		fn := func() (string, int, error) { return v6JSON, 6, nil }
		c := NewVarnishstatCollector(fn, nil)
		ch := make(chan prometheus.Metric, 300)

		c.Collect(ch)
		for len(ch) > 0 {
			<-ch
		}

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			c.Collect(ch)
			for len(ch) > 0 {
				<-ch
			}
		}
	})

	b.Run("V7/Filtered", func(b *testing.B) {
		fn := func() (string, int, error) { return v7JSON, 7, nil }
		c := NewVarnishstatCollector(fn, []string{"MAIN"})
		ch := make(chan prometheus.Metric, 300)

		c.Collect(ch)
		for len(ch) > 0 {
			<-ch
		}

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			c.Collect(ch)
			for len(ch) > 0 {
				<-ch
			}
		}
	})

	b.Run("V7/ColdCache", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			fn := func() (string, int, error) { return v7JSON, 7, nil }
			c := NewVarnishstatCollector(fn, nil)
			ch := make(chan prometheus.Metric, 300)
			c.Collect(ch)
			for len(ch) > 0 {
				<-ch
			}
		}
	})
}

func BenchmarkParseVarnishstatV7(b *testing.B) {
	entries := benchmarkCounterEntries()
	data := `{"version":1,"counters":{` + strings.Join(entries, ",") + `}}`

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = parseVarnishstatV7(data)
	}
}

func BenchmarkParseVarnishstatV6(b *testing.B) {
	entries := benchmarkCounterEntries()
	data := `{"timestamp":"2024-01-01T00:00:00",` + strings.Join(entries, ",") + `}`

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = parseVarnishstatV6(data)
	}
}
