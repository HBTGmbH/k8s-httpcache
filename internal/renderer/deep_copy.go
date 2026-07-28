package renderer

import (
	"slices"

	"github.com/HBTGmbH/k8s-httpcache/internal/watcher"
)

// copyEndpoints returns a per-render copy of eps in which every endpoint owns
// its own ForZones slice. Template functions may mutate a []string argument in
// place - sprig/sprout sortAlpha hands its argument straight to [sort.Strings]
// without copying it - and the endpoint slices reaching the template are the
// very slices the endpoint watchers retain as their EndpointsEqual dedup
// baselines (read on the informer goroutine) and the event loop keeps as
// latestFrontends/latestBackends. Sorting one in place would therefore be an
// unsynchronised write racing the informer's comparison, and would leave the
// baseline permanently out of order so every later sync of an unchanged store
// reported "changed". Copying per render keeps such mutations render-local.
func copyEndpoints(eps []watcher.Endpoint) []watcher.Endpoint {
	if eps == nil {
		return nil
	}
	out := make([]watcher.Endpoint, len(eps))
	for i, ep := range eps {
		ep.ForZones = slices.Clone(ep.ForZones)
		out[i] = ep
	}

	return out
}

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
