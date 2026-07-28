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

// redactTo writes b to w with all redaction targets replaced, using exactly
// ONE Write call on w. That single call matters: w is the shared [os.Stdout]
// / [os.Stderr], whose Write is atomic per call but which is also written
// concurrently by the controller's own slog handler and by one os/exec copy
// goroutine per subprocess stream. Letting [strings.Replacer] stream into w
// would emit a matching line as 2k+1 separate writes (pre-match text,
// placeholder, remainder, ...), so another writer's complete record could
// land inside the line and corrupt both.
//
// When no targets are configured, b is written through directly with zero
// allocations; otherwise the replaced output is collected in scratch, whose
// capacity is reused across calls, leaving only the string conversion the
// [strings.Replacer] API requires.
func (r *Redactor) redactTo(w io.Writer, scratch *bytes.Buffer, b []byte) error {
	r.mu.RLock()
	repl := r.replacer
	noTargets := len(r.values) == 0
	r.mu.RUnlock()

	if noTargets {
		_, err := w.Write(b)

		return err //nolint:wrapcheck // pass through underlying writer error
	}
	scratch.Reset()
	// bytes.Buffer implements io.StringWriter (so the fragments are appended
	// without a per-fragment conversion) and never reports an error.
	_, _ = repl.WriteString(scratch, string(b))
	_, err := w.Write(scratch.Bytes())

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

// safeCut lowers cut until no COMPLETE redaction-target occurrence straddles
// it. tailHold only guards against a partial secret at the buffer tail (a
// suffix that is a proper prefix of a target); a complete occurrence whose
// "border" (a proper prefix equal to a proper suffix, e.g. "abcXabc") ends at
// the buffer tail would be split by the raw cut - its head emitted with no
// full match to redact and its tail flushed unredacted later, leaking the
// whole secret in cleartext across the cut. Only the window around the cut
// needs scanning. Returns 0 when no safe cut exists (pathologically
// overlapping occurrences, e.g. a run of "aaaaaa"): cut only reaches 0 by
// following a chain of occurrences that overlap all the way down, so buf[:cut]
// is then entirely secret material and the caller collapses it to a single
// placeholder rather than keeping it buffered.
func (r *Redactor) safeCut(buf []byte, cut int) int {
	r.mu.RLock()
	vals := r.values
	r.mu.RUnlock()

	for changed := true; changed && cut > 0; {
		changed = false
		for _, v := range vals {
			// An occurrence straddles cut iff it starts in [cut-len(v)+1, cut)
			// and extends past cut; only that window can contain one.
			lo := max(0, cut-len(v)+1)
			hi := min(len(buf), cut+len(v)-1)
			if lo >= cut {
				continue
			}
			window := buf[lo:hi]
			from := 0
			for {
				idx := bytes.Index(window[from:], []byte(v))
				if idx < 0 {
					break
				}
				start := lo + from + idx
				if start < cut && start+len(v) > cut {
					cut = start
					changed = true

					break
				}
				from += idx + 1
			}
			if changed {
				break
			}
		}
	}

	return cut
}

// maxRedactBuffer bounds the partial-line buffer. A pathological
// newline-free stream is flushed once it exceeds this, retaining only the
// tail that could still be a secret prefix (see tailHold) - or, when no safe
// cut exists at all, collapsing the unsplittable head to a single placeholder
// (see safeCut). Either way the buffer is trimmed on every overflow, so it
// stays below maxRedactBuffer + the size of the write that crossed it (or, if
// a configured secret is itself longer than maxRedactBuffer, below that
// secret's length plus that write).
const maxRedactBuffer = 1 << 20 // 1 MiB

type redactingWriter struct {
	redactor *Redactor
	inner    io.Writer
	buf      []byte       // partial line not yet redacted and forwarded
	out      bytes.Buffer // reused scratch holding one segment's redacted form
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
		raw := len(w.buf) - hold
		cut := w.redactor.safeCut(w.buf, raw)
		// The head must be dropped either way, or the bound bounds nothing:
		// leaving it buffered lets a newline-free stream grow the buffer 1:1
		// with the stream while every later Write re-scans it.
		drop := raw
		var err error
		switch {
		case cut > 0:
			drop = cut
			err = w.emit(w.buf[:cut])
		case raw > 0:
			// No safe cut exists, so buf[:raw] is one unbroken chain of
			// overlapping target occurrences (see safeCut): emitting it
			// verbatim would leak a secret split across the cut, and it
			// carries no non-secret bytes to preserve. Collapse it.
			err = w.emitPlaceholder()
		}
		if drop > 0 {
			rest := copy(w.buf, w.buf[drop:])
			w.buf = w.buf[:rest]
		}
		if err != nil {
			return len(p), err
		}
	}

	return len(p), nil
}

// Flush redacts and forwards any buffered partial line, newline-terminated.
// Call it once the writing subprocess has exited, so a final unterminated
// line is not lost. The added newline matters: without it, the next writer's
// first line (e.g. a restarted subprocess) would merge into the truncated one.
func (w *redactingWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	w.buf = append(w.buf, '\n')
	err := w.emit(w.buf)
	w.buf = w.buf[:0]

	return err
}

func (w *redactingWriter) emit(b []byte) error {
	return w.redactor.redactTo(w.inner, &w.out, b)
}

// emitPlaceholder forwards a single placeholder, standing in for a stretch of
// buffer that is entirely secret material and cannot be cut safely.
func (w *redactingWriter) emitPlaceholder() error {
	_, err := io.WriteString(w.inner, placeholder)

	return err //nolint:wrapcheck // pass through underlying writer error
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
