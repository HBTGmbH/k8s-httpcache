package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// FuzzCollect ensures a scrape never panics, regardless of the varnishstat
// output: parsing, metric-name resolution, label assignment, and metric
// construction must all degrade gracefully (drop the offending counter) rather
// than crash the Collect call.
func FuzzCollect(f *testing.F) {
	seeds := []string{
		`{"counters":{"MAIN.cache_hit":{"value":1,"flag":"c"}}}`,
		`{"counters":{"MAIN.sess_conn":{"value":1,"flag":"c","ident":"x"},"MAIN.sess_drop":{"value":2,"flag":"c"}}}`,
		`{"counters":{"VBE.kv_reload_1.default(1.2.3.4,,80).happy":{"value":18446744073709551615,"flag":"b"}}}`,
		`{"counters":{"SMA.s0.g_bytes":{"value":5,"flag":"g","ident":"s0"}}}`,
		`{"counters":{"LCK.sma.creat":{"value":1,"flag":"c","ident":"sma"}}}`,
		`{"timestamp":"x","MAIN.n_wrk":{"value":3,"flag":"g"}}`,
		`{"counters":{"weird.(name).with.dots":{"value":1,"flag":"c"}}}`,
		``,
		`{`,
	}
	for _, s := range seeds {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(_ *testing.T, data string, v7 bool) {
		major := 6
		if v7 {
			major = 7
		}
		c := NewVarnishstatCollector(func() (string, int, error) { return data, major, nil }, nil)
		ch := make(chan prometheus.Metric)
		go func() {
			for range ch { //nolint:revive // drain only
			}
		}()
		c.Collect(ch) // must never panic
		close(ch)
	})
}
