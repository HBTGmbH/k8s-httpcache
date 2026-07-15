package renderer

// deepCopyTemplateMap converts a per-source values/secrets map to the
// map[string]any shape templates consume, deep-copying every nested
// map[string]any and []any. Sprig/sprout dict helpers (set, unset, merge)
// mutate their arguments in place during template execution; handing the
// template a per-render copy keeps those mutations away from the maps shared
// with the watchers' [reflect.DeepEqual] dedup baselines (read on informer and
// poller goroutines, where a concurrent mutation is a fatal map race) and
// the event loop's latestValues/latestSecrets (where a persisted mutation
// would make identical inputs render differently, defeating the rendered-VCL
// hash dedup, and could corrupt which secret leaves are redacted).
func deepCopyTemplateMap(m map[string]map[string]any) map[string]any {
	r := make(map[string]any, len(m))
	for k, v := range m {
		r[k] = deepCopyValue(v)
	}

	return r
}

// deepCopyValue recursively copies map[string]any and []any containers (the
// only container types yaml.Unmarshal produces on the values/secrets paths);
// scalars are returned as-is.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopyValue(e)
		}

		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}

		return out
	default:
		return v
	}
}
