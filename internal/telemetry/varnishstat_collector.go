package telemetry

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

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
	// scrape. Keyed by metric name; guarded by mu.
	descCache map[string]*prometheus.Desc

	// mu serialises Collect: the prometheus.Collector contract allows
	// concurrent Collect calls (e.g. two scrapes in flight), which would
	// otherwise race on descCache and the scrape counters.
	mu            sync.Mutex
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
	c.mu.Lock()
	defer c.mu.Unlock()

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

	// Stack-allocated label buffers reused each iteration.
	var keyBuf, valBuf [3]string

	for name, counter := range counters {
		if isStaleBackendCounter(name, latestReload) {
			continue
		}

		// Apply group filter using the raw uppercase prefix (e.g. "VBE", "SMA").
		// Varnishstat always emits uppercase group prefixes, so no ToUpper needed.
		rawGroup, _, _ := strings.Cut(name, ".")
		if len(c.groupFilter) > 0 && !c.groupFilter[rawGroup] {
			continue
		}

		// Map the raw group to the Prometheus group name (e.g. VBE → backend).
		group := lowerGroup(rawGroup)

		metricName, desc, labelKeys, labelValues := resolveMetric(name, group, counter.Ident, counter.Description, keyBuf[:0], valBuf[:0])

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

	// unknownServer is the placeholder server label when no address is available.
	unknownServer = "unknown"

	// Counter group names.
	groupMAIN = "MAIN"
	groupMain = "main"
	groupSMA  = "sma"

	// Renamed lock metric names.
	metricLockCollisions = "varnish_lock_collisions"
	metricLockCreated    = "varnish_lock_created"
	metricLockDestroyed  = "varnish_lock_destroyed"
	metricLockOperations = "varnish_lock_operations"

	// metricSMAGAlloc anchors the SMA "g_alloc" gauge — referenced from both
	// labelKeyByMetric and as a key in resolveMetric.
	metricSMAGAlloc = "varnish_sma_g_alloc"

	// Prometheus label keys.
	labelType   = "type"
	labelTarget = "target"
)

// compareReloadTags orders two reload tag suffixes (the part after
// "VBE.kv_reload_"). The controller names reloads with an unpadded counter
// (kv_reload_1, … kv_reload_10), so purely numeric tags are compared
// numerically — lexicographic comparison would rank 9 above 10. Non-numeric
// tags (e.g. timestamp-style) fall back to lexicographic ordering.
func compareReloadTags(a, b string) int {
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	if errA == nil && errB == nil {
		return cmp.Compare(na, nb)
	}

	return cmp.Compare(a, b)
}

// newestVBEReloadTag scans counter names for VBE.kv_reload_<tag> prefixes and
// returns the one from the most recent VCL reload (numeric tags compared
// numerically). Returns "" when no reload prefixes exist.
func newestVBEReloadTag(counters map[string]varnishstatCounter) string {
	latest := ""
	latestSuffix := ""
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
		suffix := tail[:sep]
		if latest == "" || compareReloadTags(suffix, latestSuffix) > 0 {
			latest = key[:len(vbeReloadTag)+sep]
			latestSuffix = suffix
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
	if !strings.HasPrefix(counterName, "VBE.") {
		return false
	}

	// Current-reload counters have the form "<latestReload>.<backend>…".
	// The boundary dot must be checked explicitly: "VBE.kv_reload_2" is a
	// plain prefix of "VBE.kv_reload_25.…" but reload 25 is a different
	// (non-stale) generation.
	if strings.HasPrefix(counterName, latestReload) &&
		len(counterName) > len(latestReload) && counterName[len(latestReload)] == '.' {
		return false
	}

	return true
}

// lowerGroup maps a raw uppercase group name (e.g. "MAIN", "VBE") to its
// lowercase Prometheus equivalent. Known groups are handled via a switch to
// avoid allocating a new string from [strings.ToLower] on every call.
func lowerGroup(upper string) string {
	switch upper {
	case groupMAIN:
		return groupMain
	case "VBE":
		return backendGroup
	case "SMA":
		return groupSMA
	case "LCK":
		return "lck"
	case "MGT":
		return "mgt"
	case "MEMPOOL":
		return "mempool"
	case "SMF":
		return "smf"
	default:
		return strings.ToLower(upper)
	}
}

// mapGroupName converts a raw counter name to its Prometheus metric group.
// VBE counters become "backend"; everything else uses the lowercased prefix.
func mapGroupName(counterName string) string {
	prefix, _, _ := strings.Cut(counterName, ".")

	return lowerGroup(strings.ToUpper(prefix))
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

// parseBackendIdent extracts backend name and server address from a VBE
// identifier string. Handles parenthesized (host,,port) and plain backend
// name formats.
func parseBackendIdent(ident string) (string, string) {
	// Parenthesized: name(ip,,port) — most common format.
	if paren := strings.LastIndexByte(ident, '('); paren >= 0 &&
		len(ident) > paren+1 && ident[len(ident)-1] == ')' {
		addr := strings.Replace(ident[paren+1:len(ident)-1], ",,", ":", 1)

		return normalizeBackendName(ident[:paren]), addr
	}

	return normalizeBackendName(ident), unknownServer
}

// counterAggregation collapses a family of similarly-prefixed metrics into
// a single metric distinguished by a label. The fq* fields are pre-computed
// at package init time to avoid repeated string concatenation per counter.
type counterAggregation struct {
	fqTotal       string // "varnish_" + totalKey — exact match for the total counter
	fqMatch       string // "varnish_" + match — prefix to test against
	fqMatchPrefix string // fqMatch + "_" — pre-computed prefix for HasPrefix checks
	target        string // "varnish_" + (renamed or match) — output metric name
	targetTotal   string // target + "_total" — pre-computed total metric name
	help          string // shared help text
	label         string // label key (always populated, defaults to "type")
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
		label := labelType
		if r.label != "" {
			label = r.label
		}
		fqMatch := "varnish_" + r.match
		out[i] = counterAggregation{
			fqTotal:       "varnish_" + r.totalKey,
			fqMatch:       fqMatch,
			fqMatchPrefix: fqMatch + "_",
			target:        target,
			targetTotal:   target + "_total",
			help:          r.help,
			label:         label,
		}
	}

	return out
}()

// renamedMetrics maps original metric names to canonical Prometheus names.
var renamedMetrics = map[string]string{
	"varnish_lck_colls":   metricLockCollisions,
	"varnish_lck_creat":   metricLockCreated,
	"varnish_lck_destroy": metricLockDestroyed,
	"varnish_lck_locks":   metricLockOperations,
}

// labelKeyByMetric overrides the default "id" label key for specific metrics.
var labelKeyByMetric = map[string]string{
	metricLockCollisions:      labelTarget,
	metricLockCreated:         labelTarget,
	metricLockDestroyed:       labelTarget,
	metricLockOperations:      labelTarget,
	"varnish_sma_c_bytes":     labelType,
	"varnish_sma_c_fail":      labelType,
	"varnish_sma_c_freed":     labelType,
	"varnish_sma_c_req":       labelType,
	metricSMAGAlloc:           labelType,
	"varnish_sma_g_bytes":     labelType,
	"varnish_sma_g_space":     labelType,
	"varnish_smf_c_bytes":     labelType,
	"varnish_smf_c_fail":      labelType,
	"varnish_smf_c_freed":     labelType,
	"varnish_smf_c_req":       labelType,
	"varnish_smf_g_alloc":     labelType,
	"varnish_smf_g_bytes":     labelType,
	"varnish_smf_g_smf_frag":  labelType,
	"varnish_smf_g_smf_large": labelType,
	"varnish_smf_g_smf":       labelType,
	"varnish_smf_g_space":     labelType,
}

// toLowerASCII returns s lowercased. When s is already lowercase (the common
// case for varnishstat stat names), the original string is returned without
// any allocation.
func toLowerASCII(s string) string {
	for i := range len(s) {
		if s[i] < 'A' || s[i] > 'Z' {
			continue
		}

		// Found uppercase — build lowered copy.
		var b strings.Builder
		b.Grow(len(s))
		_, _ = b.WriteString(s[:i])

		for j := i; j < len(s); j++ {
			c := s[j]
			if c >= 'A' && c <= 'Z' {
				_ = b.WriteByte(c + 32)
			} else {
				_ = b.WriteByte(c)
			}
		}

		return b.String()
	}

	return s
}

// needsTransform reports whether stat contains uppercase letters or dots
// that need to be transformed for the Prometheus metric name.
func needsTransform(s string) bool {
	for i := range len(s) {
		if s[i] == '.' || (s[i] >= 'A' && s[i] <= 'Z') {
			return true
		}
	}

	return false
}

// resolveMetric transforms a varnishstat counter into a Prometheus metric name
// with appropriate labels. It handles identifier extraction, name normalization,
// label assignment, and metric aggregation.
//
// keysBuf and valsBuf are caller-provided scratch slices (typically backed by
// stack-allocated arrays) that are used instead of allocating new slices.
func resolveMetric(counterName, group, ident, help string, keysBuf, valsBuf []string) (string, string, []string, []string) {
	// Locate the suffix after the group prefix (e.g. "MAIN.").
	firstDot := strings.IndexByte(counterName, '.')
	if firstDot < 0 {
		firstDot = len(counterName) - 1
	}
	suffix := counterName[firstDot+1:] // everything after "GROUP."

	// When the JSON lacks an ident field, derive it from the counter name.
	// Counters with more than one dot have the pattern "<group>.<ident>.<stat>".
	if ident == "" {
		if lastDot := strings.LastIndex(suffix, "."); lastDot > 0 {
			ident = toLowerASCII(suffix[:lastDot])
		}
	}

	// Remove the ident from the suffix to get the stat name.
	stat := suffix
	if ident != "" && len(suffix) > len(ident) && suffix[len(ident)] == '.' &&
		strings.EqualFold(suffix[:len(ident)], ident) {
		stat = suffix[len(ident)+1:]
	}

	// Build the metric name with minimal allocations.
	var name string
	if !needsTransform(stat) {
		name = "varnish_" + group + "_" + stat
	} else {
		var b strings.Builder
		b.Grow(8 + len(group) + 1 + len(stat)) // "varnish_" + group + "_" + stat
		_, _ = b.WriteString("varnish_")
		_, _ = b.WriteString(group)
		_ = b.WriteByte('_')

		for i := range len(stat) {
			c := stat[i]
			switch {
			case c == '.':
				_ = b.WriteByte('_')
			case c >= 'A' && c <= 'Z':
				_ = b.WriteByte(c + 32)
			default:
				_ = b.WriteByte(c)
			}
		}
		name = b.String()
	}

	// Apply canonical name renames (e.g. lck_creat → lock_created).
	if canonical, ok := renamedMetrics[name]; ok {
		name = canonical
	}
	description := help

	// Assign labels based on the counter group and identifier.
	labelKeys := keysBuf
	labelValues := valsBuf
	if ident != "" {
		if strings.HasPrefix(counterName, "VBE.") {
			backend, server := parseBackendIdent(ident)
			labelKeys = append(labelKeys, "backend", "server")
			labelValues = append(labelValues, backend, server)
		}
		if len(labelKeys) == 0 {
			lk := labelKeyByMetric[name]
			if lk == "" {
				lk = "id"
			}
			labelKeys = append(labelKeys, lk)
			labelValues = append(labelValues, ident)
		}
	}

	// Collapse metric families with shared prefixes into a single metric
	// distinguished by a type label.
	for i := range counterAggregations {
		agg := &counterAggregations[i]

		switch {
		case name == agg.fqTotal:
			return agg.targetTotal, agg.help, labelKeys, labelValues
		case len(name) > len(agg.fqMatchPrefix) && strings.HasPrefix(name, agg.fqMatchPrefix):
			labelKeys = append(labelKeys, agg.label)
			labelValues = append(labelValues, name[len(agg.fqMatchPrefix):])

			return agg.target, agg.help, labelKeys, labelValues
		}
	}

	return name, description, labelKeys, labelValues
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
