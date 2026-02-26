package telemetry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/prometheus/client_golang/prometheus"
)

// jsonAPI is a jsoniter instance configured for stdlib compatibility.
// It handles json.Number and json.RawMessage identically to encoding/json
// but parses significantly faster with fewer allocations.
var jsonAPI = jsoniter.ConfigCompatibleWithStandardLibrary

// varnishstatCounter holds a single parsed counter from varnishstat JSON output.
type varnishstatCounter struct {
	Value       float64
	RawValue    string // original JSON number string for lossless bitmap parsing
	Flag        string
	Description string
	Ident       string
}

// VarnishstatCollector implements prometheus.Collector and exports all
// varnishstat counters as Prometheus metrics on each scrape.
type VarnishstatCollector struct {
	varnishstatFn func() (string, int, error)
	groupFilter   map[string]bool // nil or empty = export all groups

	upDesc            *prometheus.Desc
	durationDesc      *prometheus.Desc
	totalScrapesDesc  *prometheus.Desc
	parseFailuresDesc *prometheus.Desc

	// descCache avoids re-creating identical prometheus.Desc objects on every
	// scrape. Keyed by metric name; safe without a mutex because Prometheus
	// serialises calls to Collect.
	descCache map[string]*prometheus.Desc

	totalScrapes  float64
	parseFailures float64
}

// NewVarnishstatCollector creates a collector that calls varnishstatFn on each
// scrape to obtain raw JSON output and the Varnish major version.
// filter optionally restricts which counter groups are exported (e.g. ["MAIN", "SMA"]).
// An empty or nil filter exports all groups.
func NewVarnishstatCollector(varnishstatFn func() (string, int, error), filter []string) *VarnishstatCollector {
	var gf map[string]bool
	if len(filter) > 0 {
		gf = make(map[string]bool, len(filter))
		for _, g := range filter {
			gf[strings.ToUpper(g)] = true
		}
	}

	return &VarnishstatCollector{
		varnishstatFn: varnishstatFn,
		groupFilter:   gf,
		descCache:     make(map[string]*prometheus.Desc, 256),
		upDesc: prometheus.NewDesc(
			"varnish_up",
			"Whether the last varnishstat scrape succeeded (1 = up, 0 = down).",
			nil, nil,
		),
		durationDesc: prometheus.NewDesc(
			"varnish_scrape_duration_seconds",
			"Duration of the varnishstat scrape in seconds.",
			nil, nil,
		),
		totalScrapesDesc: prometheus.NewDesc(
			"varnish_exporter_total_scrapes",
			"Count of total varnishstat scrapes performed.",
			nil, nil,
		),
		parseFailuresDesc: prometheus.NewDesc(
			"varnish_exporter_json_parse_failures_total",
			"Count of JSON parse failures during varnishstat scrapes.",
			nil, nil,
		),
	}
}

// Describe sends the fixed meta-metric descriptors. Dynamic counter metrics
// use the unchecked collector pattern and are not pre-registered.
func (c *VarnishstatCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.upDesc
	ch <- c.durationDesc
	ch <- c.totalScrapesDesc
	ch <- c.parseFailuresDesc
}

// Collect runs varnishstat and emits all counters as Prometheus metrics.
func (c *VarnishstatCollector) Collect(ch chan<- prometheus.Metric) {
	c.totalScrapes++

	start := time.Now()
	out, majorVersion, err := c.varnishstatFn()
	duration := time.Since(start).Seconds()

	ch <- prometheus.MustNewConstMetric(c.durationDesc, prometheus.GaugeValue, duration)
	ch <- prometheus.MustNewConstMetric(c.totalScrapesDesc, prometheus.CounterValue, c.totalScrapes)

	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, 0)
		ch <- prometheus.MustNewConstMetric(c.parseFailuresDesc, prometheus.CounterValue, c.parseFailures)

		return
	}

	ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, 1)

	var counters map[string]varnishstatCounter
	if majorVersion < 7 {
		counters, err = parseVarnishstatV6(out)
	} else {
		counters, err = parseVarnishstatV7(out)
	}
	if err != nil {
		c.parseFailures++
		ch <- prometheus.MustNewConstMetric(c.parseFailuresDesc, prometheus.CounterValue, c.parseFailures)

		return
	}

	ch <- prometheus.MustNewConstMetric(c.parseFailuresDesc, prometheus.CounterValue, c.parseFailures)

	// Only keep counters from the most recent VCL reload.
	latestReload := newestVBEReloadTag(counters)

	for name, counter := range counters {
		if isStaleBackendCounter(name, latestReload) {
			continue
		}

		// Apply group filter using the raw uppercase prefix (e.g. "VBE", "SMA").
		rawGroup, _, _ := strings.Cut(name, ".")
		upperGroup := strings.ToUpper(rawGroup)
		if len(c.groupFilter) > 0 && !c.groupFilter[upperGroup] {
			continue
		}

		// Map the raw group to the Prometheus group name (e.g. VBE → backend).
		group := strings.ToLower(rawGroup)
		if upperGroup == "VBE" {
			group = backendGroup
		}

		metricName, desc, labelKeys, labelValues := resolveMetric(name, group, counter.Ident, counter.Description)

		valueType := prometheus.GaugeValue
		if counter.Flag == "c" || counter.Flag == "a" {
			valueType = prometheus.CounterValue
		}

		// Skip zero-value counters — they carry no information and reduce scrape size.
		// Gauges and bitmaps at 0 remain meaningful (e.g. n_object=0, happy=0).
		if valueType == prometheus.CounterValue && counter.Value == 0 {
			continue
		}

		// Replace the raw happy bitmap with a boolean 0/1 up gauge.
		// The bitmap value (e.g. 18446744073709551615) is meaningless as
		// a Prometheus metric; bit 0 encodes current health status.
		if metricName == "varnish_backend_happy" {
			upValue := 0.0
			parsed, parseErr := strconv.ParseUint(counter.RawValue, 10, 64)
			if parseErr == nil && parsed&1 != 0 {
				upValue = 1.0
			}
			ch <- prometheus.MustNewConstMetric(c.cachedDesc("varnish_backend_up", "Whether the backend is healthy according to the latest probe", labelKeys), prometheus.GaugeValue, upValue, labelValues...)

			continue
		}

		ch <- prometheus.MustNewConstMetric(c.cachedDesc(metricName, desc, labelKeys), valueType, counter.Value, labelValues...)
	}
}

// cachedDesc returns a cached prometheus.Desc for the given metric name,
// creating and caching one on first access. This avoids re-allocating
// identical descriptors on every scrape.
func (c *VarnishstatCollector) cachedDesc(name, help string, labelKeys []string) *prometheus.Desc {
	if d, ok := c.descCache[name]; ok {
		return d
	}
	d := prometheus.NewDesc(name, help, labelKeys, nil)
	c.descCache[name] = d

	return d
}

const (
	// vbeReloadTag is the counter name prefix indicating a VCL reload backend.
	vbeReloadTag = "VBE.kv_reload_"

	// backendGroup is the Prometheus group name used for VBE counters.
	backendGroup = "backend"
)

// newestVBEReloadTag scans counter names for VBE.reload_<N> prefixes and
// returns the lexicographically greatest one (most recent VCL reload).
// Returns "" when no reload prefixes exist.
func newestVBEReloadTag(counters map[string]varnishstatCounter) string {
	latest := ""
	for key := range counters {
		if !strings.HasPrefix(key, vbeReloadTag) {
			continue
		}
		if !strings.HasSuffix(key, ".happy") {
			continue
		}
		// Extract "VBE.kv_reload_<tag>" before the next dot.
		tail := key[len(vbeReloadTag):]
		sep := strings.Index(tail, ".")
		if sep < 0 {
			continue
		}
		candidate := key[:len(vbeReloadTag)+sep]
		if candidate > latest {
			latest = candidate
		}
	}

	return latest
}

// isStaleBackendCounter reports whether a counter belongs to a VBE
// from an older VCL reload that should be skipped.
func isStaleBackendCounter(counterName, latestReload string) bool {
	if latestReload == "" {
		return false
	}

	return strings.HasPrefix(counterName, "VBE.") && !strings.HasPrefix(counterName, latestReload)
}

// mapGroupName converts a raw counter name to its Prometheus metric group.
// VBE counters become "backend"; everything else uses the lowercased prefix.
func mapGroupName(counterName string) string {
	prefix, _, _ := strings.Cut(counterName, ".")
	switch strings.ToUpper(prefix) {
	case "VBE":
		return backendGroup
	default:
		return strings.ToLower(prefix)
	}
}

// normalizeBackendName strips the VCL-name prefix (the first dot-separated
// segment) from a backend identifier. The VCL name is always the first
// segment (e.g. "boot", "kv_reload_3", "reload_1"), so a single strip
// handles all current and future naming conventions.
func normalizeBackendName(raw string) string {
	s := strings.Trim(raw, ".")
	if _, rest, found := strings.Cut(s, "."); found {
		return rest
	}

	return s
}

var (
	uuidBackendRe  = regexp.MustCompile(`^([0-9A-Za-z]{8}-[0-9A-Za-z]{4}-[0-9A-Za-z]{4}-[89ABab][0-9A-Za-z]{3}-[0-9A-Za-z]{12})(.*)$`)
	parenBackendRe = regexp.MustCompile(`^(.*)\((.*)\)$`)
)

// counterAggregation collapses a family of similarly-prefixed metrics into
// a single metric distinguished by a label. The fq* fields are pre-computed
// at package init time to avoid repeated string concatenation per counter.
type counterAggregation struct {
	fqTotal string // "varnish_" + totalKey — exact match for the total counter
	fqMatch string // "varnish_" + match — prefix to test against
	target  string // "varnish_" + (renamed or match) — output metric name
	help    string // shared help text
	label   string // label key (always populated, defaults to "type")
}

var counterAggregations = func() []counterAggregation {
	type rule struct {
		match, renamed, totalKey, help, label string
	}
	rules := []rule{
		{match: "main_fetch", totalKey: "main_s_fetch", help: "Number of fetches"},
		{renamed: "main_sessions", match: "main_sess", totalKey: "main_s_sess", help: "Number of sessions"},
		{renamed: "main_worker_threads", match: "main_n_wrk", totalKey: "main_n_wrk", help: "Number of worker threads"},
	}
	out := make([]counterAggregation, len(rules))
	for i, r := range rules {
		target := "varnish_" + r.match
		if r.renamed != "" {
			target = "varnish_" + r.renamed
		}
		label := "type"
		if r.label != "" {
			label = r.label
		}
		out[i] = counterAggregation{
			fqTotal: "varnish_" + r.totalKey,
			fqMatch: "varnish_" + r.match,
			target:  target,
			help:    r.help,
			label:   label,
		}
	}

	return out
}()

// renamedMetrics maps original metric names to canonical Prometheus names.
var renamedMetrics = map[string]string{
	"varnish_lck_colls":   "varnish_lock_collisions",
	"varnish_lck_creat":   "varnish_lock_created",
	"varnish_lck_destroy": "varnish_lock_destroyed",
	"varnish_lck_locks":   "varnish_lock_operations",
}

// labelKeyByMetric overrides the default "id" label key for specific metrics.
var labelKeyByMetric = map[string]string{
	"varnish_lock_collisions": "target",
	"varnish_lock_created":    "target",
	"varnish_lock_destroyed":  "target",
	"varnish_lock_operations": "target",
	"varnish_sma_c_bytes":     "type",
	"varnish_sma_c_fail":      "type",
	"varnish_sma_c_freed":     "type",
	"varnish_sma_c_req":       "type",
	"varnish_sma_g_alloc":     "type",
	"varnish_sma_g_bytes":     "type",
	"varnish_sma_g_space":     "type",
	"varnish_smf_c_bytes":     "type",
	"varnish_smf_c_fail":      "type",
	"varnish_smf_c_freed":     "type",
	"varnish_smf_c_req":       "type",
	"varnish_smf_g_alloc":     "type",
	"varnish_smf_g_bytes":     "type",
	"varnish_smf_g_smf_frag":  "type",
	"varnish_smf_g_smf_large": "type",
	"varnish_smf_g_smf":       "type",
	"varnish_smf_g_space":     "type",
}

// resolveMetric transforms a varnishstat counter into a Prometheus metric name
// with appropriate labels. It handles identifier extraction, name normalization,
// label assignment, and metric aggregation.
func resolveMetric(counterName, group, ident, help string) (string, string, []string, []string) {
	lowerName := strings.ToLower(counterName)

	// When the JSON lacks an ident field, derive it from the counter name.
	// Counters with more than one dot have the pattern "<group>.<ident>.<stat>".
	if ident == "" && strings.Count(lowerName, ".") > 1 {
		withoutGroup := dropFirstSegment(lowerName)
		if lastDot := strings.LastIndex(withoutGroup, "."); lastDot > 0 {
			ident = withoutGroup[:lastDot]
		}
	}

	// Build the base metric name: lowercase, remove ident segment, prepend group.
	normalized := lowerName
	if ident != "" {
		normalized = strings.Replace(normalized, "."+strings.ToLower(ident), "", 1)
	}
	normalized = dropFirstSegment(normalized)
	name := "varnish_" + group + "_" + strings.ReplaceAll(normalized, ".", "_")

	// Apply canonical name renames (e.g. lck_creat → lock_created).
	if canonical, ok := renamedMetrics[name]; ok {
		name = canonical
	}
	description := help

	// Assign labels based on the counter group and identifier.
	var labelKeys, labelValues []string
	if ident != "" {
		if strings.HasPrefix(counterName, "VBE.") {
			labelKeys, labelValues = backendLabelsFromIdent(ident)
		}
		if len(labelKeys) == 0 {
			lk := labelKeyByMetric[name]
			if lk == "" {
				lk = "id"
			}
			labelKeys = []string{lk}
			labelValues = []string{ident}
		}
	}

	// Collapse metric families with shared prefixes into a single metric
	// distinguished by a type label.
	for i := range counterAggregations {
		agg := &counterAggregations[i]

		switch {
		case name == agg.fqTotal:
			return agg.target + "_total", agg.help, labelKeys, labelValues
		case len(name) > len(agg.fqMatch)+1 && strings.HasPrefix(name, agg.fqMatch+"_"):
			labelKeys = append(labelKeys, agg.label)
			labelValues = append(labelValues, name[len(agg.fqMatch)+1:])

			return agg.target, agg.help, labelKeys, labelValues
		}
	}

	return name, description, labelKeys, labelValues
}

// backendLabelsFromIdent parses a VBE identifier string into backend and
// server labels. Handles UUID-prefixed, parenthesized (host,,port), and
// plain backend name formats.
func backendLabelsFromIdent(ident string) ([]string, []string) {
	// UUID-based identifier (Varnish 4): <uuid><backend_name>
	if m := uuidBackendRe.FindStringSubmatch(ident); len(m) >= 3 {
		return []string{"backend", "server"},
			[]string{normalizeBackendName(m[2]), m[1]}
	}
	// Parenthesized: name(ip,,port)
	if m := parenBackendRe.FindStringSubmatch(ident); len(m) >= 3 {
		addr := strings.Replace(m[2], ",,", ":", 1)

		return []string{"backend", "server"},
			[]string{normalizeBackendName(m[1]), addr}
	}
	// Plain backend name without address information.
	return []string{"backend", "server"},
		[]string{normalizeBackendName(ident), "unknown"}
}

// dropFirstSegment removes the leading dot-separated segment from a name.
// For example, "vbe.reload_1.foo.happy" becomes "reload_1.foo.happy".
func dropFirstSegment(name string) string {
	if _, rest, found := strings.Cut(name, "."); found {
		return rest
	}

	return name
}

// invalidMetricChars matches characters not permitted in Prometheus metric names.
var invalidMetricChars = regexp.MustCompile(`[^a-zA-Z0-9_:]`)

// sanitizeMetricName converts a varnishstat counter name like "MAIN.cache_hit"
// into a valid Prometheus metric name like "varnish_main_cache_hit".
func sanitizeMetricName(name string) string {
	s := strings.ReplaceAll(name, ".", "_")
	s = invalidMetricChars.ReplaceAllString(s, "_")
	s = strings.ToLower(s)

	return "varnish_" + s
}

// parseVarnishstatV7 decodes the Varnish 7+ JSON format where counters
// are nested under a "counters" key.
func parseVarnishstatV7(data string) (map[string]varnishstatCounter, error) {
	var parsed struct {
		Counters map[string]struct {
			Value       json.Number `json:"value"`
			Flag        string      `json:"flag"`
			Description string      `json:"description"`
			Ident       string      `json:"ident"`
		} `json:"counters"`
	}

	err := jsonAPI.Unmarshal([]byte(data), &parsed)
	if err != nil {
		return nil, fmt.Errorf("decoding varnishstat v7 output: %w", err)
	}

	result := make(map[string]varnishstatCounter, len(parsed.Counters))
	for k, v := range parsed.Counters {
		fVal, err := v.Value.Float64()
		if err != nil {
			continue
		}
		result[k] = varnishstatCounter{
			Value:       fVal,
			RawValue:    v.Value.String(),
			Flag:        v.Flag,
			Description: v.Description,
			Ident:       v.Ident,
		}
	}

	return result, nil
}

// parseVarnishstatV6 decodes the Varnish 6.x flat JSON format where
// counters sit at the top level alongside non-counter keys like "timestamp".
func parseVarnishstatV6(data string) (map[string]varnishstatCounter, error) {
	var raw map[string]json.RawMessage

	err := jsonAPI.Unmarshal([]byte(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("decoding varnishstat v6 output: %w", err)
	}

	result := make(map[string]varnishstatCounter, len(raw))
	for k, v := range raw {
		// Non-counter entries like "timestamp" lack a dot separator.
		if !strings.Contains(k, ".") {
			continue
		}

		var entry struct {
			Value       json.Number `json:"value"`
			Flag        string      `json:"flag"`
			Description string      `json:"description"`
			Ident       string      `json:"ident"`
		}

		err := jsonAPI.Unmarshal(v, &entry)
		if err != nil {
			continue
		}

		fVal, err := entry.Value.Float64()
		if err != nil {
			continue
		}

		result[k] = varnishstatCounter{
			Value:       fVal,
			RawValue:    entry.Value.String(),
			Flag:        entry.Flag,
			Description: entry.Description,
			Ident:       entry.Ident,
		}
	}

	return result, nil
}
