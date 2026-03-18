// Package redact provides a thread-safe string redactor for secret values.
package redact

import (
	"cmp"
	"io"
	"slices"
	"strings"
	"sync"
)

// minSecretLen is the minimum string length to consider for redaction.
// Short strings (e.g. "yes", "null") are skipped to avoid false positives.
const minSecretLen = 6

// placeholder is the replacement text for redacted values.
const placeholder = "[REDACTED]"

// Redactor maintains a thread-safe strings.Replacer built from secret values.
type Redactor struct {
	mu       sync.RWMutex
	replacer *strings.Replacer
}

// NewRedactor creates a Redactor with no secrets (no-op replacer).
func NewRedactor() *Redactor {
	return &Redactor{
		replacer: strings.NewReplacer(),
	}
}

// Update rebuilds the replacer from the current set of secret maps.
// Each map value is walked recursively; leaf strings with len >= minSecretLen
// are added as redaction targets. Longer values are matched first.
func (r *Redactor) Update(secrets map[string]map[string]any) {
	var vals []string
	for _, m := range secrets {
		collectLeafStrings(m, &vals)
	}

	// Deduplicate.
	seen := make(map[string]struct{}, len(vals))
	unique := vals[:0]
	for _, v := range vals {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			unique = append(unique, v)
		}
	}

	// Sort longest first so the replacer greedily matches longer secrets.
	slices.SortFunc(unique, func(a, b string) int {
		return cmp.Compare(len(b), len(a)) // descending by length
	})

	pairs := make([]string, 0, len(unique)*2)
	for _, v := range unique {
		pairs = append(pairs, v, placeholder)
	}

	repl := strings.NewReplacer(pairs...)

	r.mu.Lock()
	r.replacer = repl
	r.mu.Unlock()
}

// Redact applies the current replacer to s.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	repl := r.replacer
	r.mu.RUnlock()

	return repl.Replace(s)
}

// Writer returns an io.Writer that redacts each Write through w.
func (r *Redactor) Writer(w io.Writer) io.Writer {
	return &redactingWriter{redactor: r, inner: w}
}

type redactingWriter struct {
	redactor *Redactor
	inner    io.Writer
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	redacted := w.redactor.Redact(string(p))
	n, err := w.inner.Write([]byte(redacted))
	// Report the original byte count to the caller so io.Copy and friends
	// don't interpret a length mismatch as a short write.
	if err == nil {
		return len(p), nil
	}

	return n, err //nolint:wrapcheck // thin wrapper; callers handle errors
}

// collectLeafStrings recursively walks v and appends string leaves
// with len >= minSecretLen to out.
func collectLeafStrings(v any, out *[]string) {
	switch val := v.(type) {
	case string:
		if len(val) >= minSecretLen {
			*out = append(*out, val)
		}
	case map[string]any:
		for _, child := range val {
			collectLeafStrings(child, out)
		}
	case []any:
		for _, child := range val {
			collectLeafStrings(child, out)
		}
	}
}
