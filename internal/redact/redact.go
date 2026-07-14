// Package redact provides a thread-safe string redactor for secret values.
package redact

import (
	"bytes"
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

// Redactor maintains a thread-safe [strings.Replacer] built from secret values.
type Redactor struct {
	mu       sync.RWMutex
	replacer *strings.Replacer
	secrets  []string            // targets from the last Update call
	static   map[string][]string // persistent targets per source key (e.g. TLS keys per cert name)
	values   []string            // combined targets, longest first (drives tailHold)
}

// NewRedactor creates a Redactor with no secrets (no-op replacer).
func NewRedactor() *Redactor {
	return &Redactor{
		replacer: strings.NewReplacer(),
		static:   make(map[string][]string),
	}
}

// Update rebuilds the replacer from the current set of secret maps.
// Each map value is walked recursively; leaf strings with len >= minSecretLen
// are added as redaction targets. Longer values are matched first.
// Values registered via SetStaticValues are preserved.
func (r *Redactor) Update(secrets map[string]map[string]any) {
	var vals []string
	for _, m := range secrets {
		collectLeafStrings(m, &vals)
	}

	r.mu.Lock()
	r.secrets = vals
	r.rebuildLocked()
	r.mu.Unlock()
}

// SetStaticValues registers persistent redaction targets under the given
// source key (e.g. TLS private-key lines keyed by certificate name),
// replacing that key's previous values. Static values survive Update calls,
// which only rebuild the Kubernetes-Secret-derived target set. Values
// shorter than the minimum secret length are dropped.
func (r *Redactor) SetStaticValues(key string, vals []string) {
	kept := make([]string, 0, len(vals))
	for _, v := range vals {
		if len(v) >= minSecretLen {
			kept = append(kept, v)
		}
	}

	r.mu.Lock()
	r.static[key] = kept
	r.rebuildLocked()
	r.mu.Unlock()
}

// Redact applies the current replacer to s.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	repl := r.replacer
	r.mu.RUnlock()

	return repl.Replace(s)
}

// bytesEqualString reports whether b equals s without converting either.
func bytesEqualString(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		if b[i] != s[i] {
			return false
		}
	}

	return true
}

// Writer returns an [io.Writer] that redacts data written through it before
// forwarding to w. Data is line-buffered: subprocess pipes deliver arbitrary
// chunks, and redacting each chunk in isolation would let a secret spanning
// two chunks escape. Complete lines are redacted and forwarded as they
// arrive; a trailing partial line is held until its newline (or the buffer
// bound) arrives. Call Flush (e.g. after the subprocess exited) to emit a
// final unterminated line.
//
// Limitation: a secret whose own value contains a newline can still straddle
// a flush boundary; single-line secrets never can.
func (r *Redactor) Writer(w io.Writer) io.Writer {
	return &redactingWriter{redactor: r, inner: w}
}

// rebuildLocked recomputes the combined target list and replacer from the
// secret and static value sets. Callers must hold r.mu.
func (r *Redactor) rebuildLocked() {
	var vals []string
	vals = append(vals, r.secrets...)
	for _, sv := range r.static {
		vals = append(vals, sv...)
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

	r.values = unique
	r.replacer = strings.NewReplacer(pairs...)
}

// redactTo writes b to w with all redaction targets replaced. When no
// targets are configured, b is written through directly with zero
// allocations; otherwise the only allocation is the string conversion the
// [strings.Replacer] API requires (the replaced output streams straight into
// w via WriteString, never materialising an intermediate copy).
func (r *Redactor) redactTo(w io.Writer, b []byte) error {
	r.mu.RLock()
	repl := r.replacer
	noTargets := len(r.values) == 0
	r.mu.RUnlock()

	if noTargets {
		_, err := w.Write(b)

		return err //nolint:wrapcheck // pass through underlying writer error
	}
	_, err := repl.WriteString(w, string(b))

	return err //nolint:wrapcheck // pass through underlying writer error
}

// tailHold returns the length of the longest suffix of buf that is a proper
// prefix of any redaction target. A buffered writer flushing buf must retain
// that many trailing bytes: they could be the beginning of a secret whose
// remainder arrives in a later write, and flushing them would emit the
// secret's head unredacted. Comparison is byte-wise; no allocations.
func (r *Redactor) tailHold(buf []byte) int {
	r.mu.RLock()
	vals := r.values
	r.mu.RUnlock()

	hold := 0
	for _, v := range vals {
		limit := min(len(v)-1, len(buf))
		for k := limit; k > hold; k-- {
			if bytesEqualString(buf[len(buf)-k:], v[:k]) {
				hold = k

				break
			}
		}
	}

	return hold
}

// maxRedactBuffer bounds the partial-line buffer. A pathological
// newline-free stream is flushed once it exceeds this, retaining only the
// tail that could still be a secret prefix (see tailHold).
const maxRedactBuffer = 1 << 20 // 1 MiB

type redactingWriter struct {
	redactor *Redactor
	inner    io.Writer
	buf      []byte // partial line not yet redacted and forwarded
}

// Write buffers p, redacts and forwards all complete lines, and retains the
// trailing partial line. It reports len(p) on success so [io.Copy] and friends
// don't interpret the (differently-sized) redacted output as a short write.
func (w *redactingWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)

	if nl := bytes.LastIndexByte(w.buf, '\n'); nl >= 0 {
		// Redact the complete lines as one segment (not line by line) so a
		// multi-line secret fully inside the segment still matches.
		err := w.emit(w.buf[:nl+1])
		rest := copy(w.buf, w.buf[nl+1:])
		w.buf = w.buf[:rest]
		if err != nil {
			return len(p), err
		}
	}

	// Bound memory for a newline-free stream: flush all but the longest
	// suffix that could still be the start of a secret.
	if len(w.buf) >= maxRedactBuffer {
		hold := w.redactor.tailHold(w.buf)
		cut := len(w.buf) - hold
		if cut > 0 {
			err := w.emit(w.buf[:cut])
			rest := copy(w.buf, w.buf[cut:])
			w.buf = w.buf[:rest]
			if err != nil {
				return len(p), err
			}
		}
	}

	return len(p), nil
}

// Flush redacts and forwards any buffered partial line. Call it once the
// writing subprocess has exited, so a final unterminated line is not lost.
func (w *redactingWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	err := w.emit(w.buf)
	w.buf = w.buf[:0]

	return err
}

func (w *redactingWriter) emit(b []byte) error {
	return w.redactor.redactTo(w.inner, b)
}

// KeyMaterialValues extracts redaction targets from PEM-encoded private-key
// material: every content line of the blocks, skipping the public
// -----BEGIN/END----- boundary markers (redacting those would mangle
// unrelated output that legitimately mentions them).
func KeyMaterialValues(keys ...[]byte) []string {
	var vals []string
	for _, k := range keys {
		for line := range strings.SplitSeq(string(k), "\n") {
			line = strings.TrimRight(line, "\r")
			if len(line) < minSecretLen || strings.HasPrefix(line, "-----") {
				continue
			}
			vals = append(vals, line)
		}
	}

	return vals
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
