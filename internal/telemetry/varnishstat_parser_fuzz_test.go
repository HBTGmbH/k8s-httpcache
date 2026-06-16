package telemetry

import "testing"

// FuzzParseVarnishstat ensures the hand-rolled JSON scanner never panics on
// arbitrary (possibly malformed or truncated) varnishstat output. Both the v7
// and v6 entrypoints must always return cleanly with an error rather than
// crashing the metrics scrape.
func FuzzParseVarnishstat(f *testing.F) {
	seeds := []string{
		`{"version":1,"counters":{"MAIN.cache_hit":{"value":42,"flag":"c"}}}`,
		`{"timestamp":"2024","MAIN.cache_hit":{"value":42,"flag":"c","description":"hits"}}`,
		`{}`,
		`{"counters":{}}`,
		`{"counters":{"VBE.kv_reload_1.default.happy":{"value":18446744073709551615,"flag":"b"}}}`,
		`{"counters":{"SMA.s0.g_bytes":{"value":1.5e10,"ident":"s0","flag":"g"}}}`,
		`{"counters":{"x":{"value":"non-numeric"}}}`,
		`{"counters":{"x":{"value":null},"y":{"value":3}}}`,
		`{"counters":{"esc":{"value":1,"ident":"aé𝄞b","description":"q\t\"x\""}}}`,
		``,
		`{`,
		`{"counters":`,
		`{"counters":{`,
		`{"counters":{"a":{"value":`,
		`[`,
		`{"a":\ud800}`,
		`{"counters":{"a":{"value":1,}}}`,
		`{"MAIN.x":{"value":-12},"nodot":"meta"}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, data string) {
		// The contract: never panic, regardless of input. Results are ignored;
		// we only care that neither entrypoint crashes.
		_, _ = parseVarnishstatV7(data)
		_, _ = parseVarnishstatV6(data)
	})
}
