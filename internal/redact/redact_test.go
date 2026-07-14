package redact

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestRedactBasic(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "super-secret-value"},
	})

	got := r.Redact("error: token was super-secret-value in VCL")
	want := "error: token was [REDACTED] in VCL"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Non-secret text is preserved.
	got = r.Redact("nothing sensitive here")
	if got != "nothing sensitive here" {
		t.Errorf("non-secret text changed: %q", got)
	}
}

func TestRedactMultiple(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"db":    {"password": "db-password-123"},
		"redis": {"auth": "redis-token-abc"},
	})

	got := r.Redact("db=db-password-123 redis=redis-token-abc")
	want := "db=[REDACTED] redis=[REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactNested(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {
			"outer": map[string]any{
				"inner": "nested-secret-value",
			},
		},
	})

	got := r.Redact("found nested-secret-value in output")
	want := "found [REDACTED] in output"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactMinLengthBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret string
		want   bool // true if the value should be redacted
	}{
		{"empty", "", false},
		{"len1", "a", false},
		{"len2", "ab", false},
		{"len3", "abc", false},
		{"len4", "abcd", false},
		{"len5", "abcde", false},
		{"len6_at_threshold", "abcdef", true},
		{"len7_above_threshold", "abcdefg", true},
		{"len20", "abcdefghijklmnopqrst", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRedactor()
			r.Update(map[string]map[string]any{
				"app": {"key": tt.secret},
			})

			input := "prefix " + tt.secret + " suffix"
			got := r.Redact(input)
			if tt.want {
				want := "prefix [REDACTED] suffix"
				if got != want {
					t.Errorf("expected redaction for %q (len %d):\n got: %q\nwant: %q", tt.secret, len(tt.secret), got, want)
				}
			} else if got != input {
				t.Errorf("unexpected redaction for %q (len %d):\n got: %q\nwant: %q", tt.secret, len(tt.secret), got, input)
			}
		})
	}
}

func TestRedactNonStringLeaves(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {
			"port":    8080,
			"enabled": true,
			"ratio":   3.14,
		},
	})

	// Non-string values are not extracted, so nothing is redacted.
	got := r.Redact("port=8080 enabled=true ratio=3.14")
	want := "port=8080 enabled=true ratio=3.14"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactUpdate(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"v1": {"key": "old-secret-value"},
	})

	got := r.Redact("old-secret-value and new-secret-value")
	if got != "[REDACTED] and new-secret-value" {
		t.Errorf("before update: %q", got)
	}

	// Update replaces old patterns with new ones.
	r.Update(map[string]map[string]any{
		"v2": {"key": "new-secret-value"},
	})

	got = r.Redact("old-secret-value and new-secret-value")
	if got != "old-secret-value and [REDACTED]" {
		t.Errorf("after update: %q", got)
	}
}

func TestRedactEmpty(t *testing.T) {
	t.Parallel()
	r := NewRedactor()

	// nil secrets.
	r.Update(nil)
	got := r.Redact("nothing to redact")
	if got != "nothing to redact" {
		t.Errorf("nil secrets: %q", got)
	}

	// Empty map.
	r.Update(map[string]map[string]any{})
	got = r.Redact("nothing to redact")
	if got != "nothing to redact" {
		t.Errorf("empty secrets: %q", got)
	}
}

func TestRedactWriter(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "my-secret-token"},
	})

	var buf bytes.Buffer
	w := r.Writer(&buf)
	_, err := w.Write([]byte("error: my-secret-token leaked\n"))
	if err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	want := "error: [REDACTED] leaked\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactConcurrent(t *testing.T) {
	t.Parallel()
	r := NewRedactor()

	var wg sync.WaitGroup
	// Concurrent readers.
	for range 10 {
		wg.Go(func() {
			for range 100 {
				r.Redact("some text with a-secret-value inside")
			}
		})
	}
	// Concurrent writer.
	wg.Go(func() {
		for range 100 {
			r.Update(map[string]map[string]any{
				"app": {"token": "a-secret-value"},
			})
		}
	})

	wg.Wait()
}

func TestRedactSliceLeaves(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {
			"list": []any{"array-secret-val", 42, true},
		},
	})

	got := r.Redact("found array-secret-val here")
	want := "found [REDACTED] here"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactDuplicateSecrets(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	// Same secret value appears in two different maps.
	r.Update(map[string]map[string]any{
		"app1": {"token": "shared-secret"},
		"app2": {"token": "shared-secret"},
	})

	got := r.Redact("leaked shared-secret here")
	want := "leaked [REDACTED] here"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactMultipleOccurrences(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "repeat-me"},
	})

	got := r.Redact("repeat-me appears repeat-me twice")
	want := "[REDACTED] appears [REDACTED] twice"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactWriterReportsOriginalLen(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "my-secret-token"},
	})

	var buf bytes.Buffer
	w := r.Writer(&buf)
	input := []byte("token: my-secret-token")
	n, err := w.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	// Writer must report the original byte count to avoid short-write errors.
	if n != len(input) {
		t.Errorf("Write returned %d, want %d", n, len(input))
	}
}

func TestRedactWriterInnerError(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "my-secret-token"},
	})

	errInner := errors.New("disk full")
	w := r.Writer(&failWriter{err: errInner, n: 3})
	input := []byte("token: my-secret-token\n")
	n, err := w.Write(input)

	if !errors.Is(err, errInner) {
		t.Errorf("expected inner error, got %v", err)
	}
	// The writer consumed all of p into its buffer; n reports accepted input
	// bytes (io.Writer contract: 0 <= n <= len(p)).
	if n > len(input) {
		t.Errorf("Write returned n=%d > len(p)=%d", n, len(input))
	}
}

func TestRedactWriterInnerErrorCountWithinInput(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "secret"}, // 6 bytes → "[REDACTED]" is 10 bytes
	})

	errInner := errors.New("disk full")
	// The inner writer consumed 8 bytes of the redacted (longer) output
	// before failing - more than the 7 input bytes.
	w := r.Writer(&failWriter{err: errInner, n: 8})
	input := []byte("secret\n")
	n, err := w.Write(input)

	if !errors.Is(err, errInner) {
		t.Errorf("expected inner error, got %v", err)
	}
	// io.Writer contract: 0 <= n <= len(p). Reporting more makes io.Copy
	// fail with "invalid write result".
	if n > len(input) {
		t.Errorf("Write returned n=%d > len(p)=%d, violating the io.Writer contract", n, len(input))
	}
}

// failWriter is a test helper that always returns a fixed error.
type failWriter struct {
	err error
	n   int
}

func (w *failWriter) Write([]byte) (int, error) {
	return w.n, w.err
}

func TestRedactLongestFirst(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {
			"short":  "secret",
			"longer": "secret-long-version",
		},
	})

	got := r.Redact("value: secret-long-version")
	want := "value: [REDACTED]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRedactWriterDynamicUpdate verifies that an existing Writer created via
// Writer() immediately picks up new secrets when Update() is called. This is
// critical for the long-running varnishd process: the redacting writers are
// created once at startup, but secrets may be added/rotated at any time.
func TestRedactWriterDynamicUpdate(t *testing.T) {
	t.Parallel()

	r := NewRedactor()

	var buf bytes.Buffer
	w := r.Writer(&buf)

	const (
		secret1 = "initial-secret-value"
		secret2 = "rotated-secret-value"
	)

	// Before any Update, nothing is redacted.
	_, _ = w.Write([]byte(secret1 + " and " + secret2 + "\n"))
	if strings.Contains(buf.String(), "[REDACTED]") {
		t.Fatal("expected no redaction before Update")
	}

	// After first Update, only secret1 is redacted.
	buf.Reset()
	r.Update(map[string]map[string]any{"app": {"key": secret1}})
	_, _ = w.Write([]byte(secret1 + " and " + secret2 + "\n"))
	got := buf.String()
	if strings.Contains(got, secret1) {
		t.Errorf("secret1 not redacted after first Update: %q", got)
	}
	if !strings.Contains(got, secret2) {
		t.Errorf("secret2 unexpectedly redacted after first Update: %q", got)
	}

	// After second Update with a replacement secret, secret1 is no longer
	// redacted (it was removed) and secret2 is now redacted.
	buf.Reset()
	r.Update(map[string]map[string]any{"app": {"key": secret2}})
	_, _ = w.Write([]byte(secret1 + " and " + secret2 + "\n"))
	got = buf.String()
	if !strings.Contains(got, secret1) {
		t.Errorf("secret1 still redacted after replacement Update: %q", got)
	}
	if strings.Contains(got, secret2) {
		t.Errorf("secret2 not redacted after replacement Update: %q", got)
	}
}

// TestRedactWriterCrossWriteBoundary documents a known limitation: if a secret
// value is split across two Write() calls (e.g. due to OS pipe buffering), the
// redactor cannot detect it. In practice this is extremely unlikely because
// varnishd writes line-buffered output and secrets are much shorter than the
// typical pipe buffer (32–64 KB). The adm() path is unaffected because
// CombinedOutput() collects all output before redaction.
func TestRedactWriterCrossWriteBoundary(t *testing.T) {
	t.Parallel()

	r := NewRedactor()
	r.Update(map[string]map[string]any{"app": {"key": "my-secret-token"}})

	var buf bytes.Buffer
	w := r.Writer(&buf)

	// Single write: secret is redacted.
	_, _ = w.Write([]byte("token: my-secret-token\n"))
	if strings.Contains(buf.String(), "my-secret-token") {
		t.Error("secret not redacted in single write")
	}

	// Split write across boundary: secret spans two Write calls.
	// This is the documented limitation - the halves are not redacted.
	buf.Reset()
	_, _ = w.Write([]byte("token: my-secret"))
	_, _ = w.Write([]byte("-token\n"))
	// We expect the secret to survive because neither half matches.
	if strings.Contains(buf.String(), "[REDACTED]") {
		// If this ever passes, it means we added buffered redaction - great!
		t.Log("cross-boundary redaction now works (buffered writer?)")
	}
}

// captureWriter is a single-writer [io.Writer] sink that records the last bytes
// written, so a test goroutine can inspect what its own redactingWriter emitted.
type captureWriter struct {
	last []byte
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.last = append(c.last[:0], p...)

	return len(p), nil
}

// TestRedactWriterNoTornReadUnderConcurrentUpdate hammers the production Writer
// path (used on varnishd stdout/stderr) with concurrent writes while Update
// swaps the replacer pointer, and asserts every write emits either the fully
// redacted line or the fully verbatim line - never a torn mix of the two. This
// adds the correctness invariant TestRedactConcurrent lacks. Run under -race.
func TestRedactWriterNoTornReadUnderConcurrentUpdate(t *testing.T) {
	t.Parallel()

	const secret = "SUPERSECRETVALUE1234567890"
	const line = "token=" + secret + " end"
	const redacted = "token=[REDACTED] end"

	r := NewRedactor()
	var wg sync.WaitGroup

	// Updater: toggle whether the secret is a redaction target, swapping the
	// replacer pointer under the concurrent writers.
	wg.Go(func() {
		with := map[string]map[string]any{"app": {"token": secret}}
		without := map[string]map[string]any{"app": {"token": "otherlongvalue999"}}
		for i := range 5000 {
			if i%2 == 0 {
				r.Update(with)
			} else {
				r.Update(without)
			}
		}
	})

	// Writers: each with its own sink (single-writer → no sink race). Every write's
	// output must be one of the two valid full forms, never a fragment.
	var mu sync.Mutex
	var failure string
	for range 8 {
		wg.Go(func() {
			sink := &captureWriter{}
			w := r.Writer(sink)
			for range 5000 {
				_, _ = w.Write([]byte(line))
				out := string(sink.last)
				if out != line && out != redacted {
					mu.Lock()
					if failure == "" {
						failure = out
					}
					mu.Unlock()

					return
				}
			}
		})
	}

	wg.Wait()
	if failure != "" {
		t.Fatalf("torn redaction observed: %q (want %q or %q)", failure, line, redacted)
	}
}

// TestRedactWriterSecretSpansWrites verifies the chunk-boundary guarantee:
// subprocess pipes deliver arbitrary chunks, and a secret split across two
// Write calls must still be redacted. Without line buffering each chunk is
// redacted in isolation, neither half matches, and the cleartext secret
// escapes into the log stream.
func TestRedactWriterSecretSpansWrites(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "supersecretvalue"},
	})

	var buf bytes.Buffer
	w := r.Writer(&buf)
	// The pipe splits varnishd's output mid-secret.
	for _, chunk := range []string{"backend .host = \"super", "secretvalue\"; // end\n"} {
		_, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
	}

	got := buf.String()
	if strings.Contains(got, "supersecretvalue") {
		t.Fatalf("secret escaped redaction across a write boundary: %q", got)
	}
	if !strings.Contains(got, placeholder) {
		t.Fatalf("expected %s in output, got %q", placeholder, got)
	}
}

// TestRedactWriterFlushEmitsFinalPartialLine verifies an unterminated final
// line (a subprocess dying mid-line) is delivered - redacted - on Flush
// rather than silently dropped.
func TestRedactWriterFlushEmitsFinalPartialLine(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "supersecretvalue"},
	})

	var buf bytes.Buffer
	w := r.Writer(&buf)
	_, err := w.Write([]byte("dying: supersecretvalue"))
	if err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("partial line written before flush: %q", buf.String())
	}

	f, ok := w.(interface{ Flush() error })
	if !ok {
		t.Fatal("redacting writer must expose Flush")
	}
	err = f.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "dying: "+placeholder+"\n" {
		t.Fatalf("flushed output = %q, want redacted, newline-terminated final line", got)
	}
}

// TestRedactWriterOverflowRetainsSecretPrefix verifies the bounded-buffer
// flush cannot leak a secret straddling the flush cut: the tail that is a
// prefix of a configured secret is retained until the remainder arrives.
func TestRedactWriterOverflowRetainsSecretPrefix(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.Update(map[string]map[string]any{
		"app": {"token": "supersecretvalue"},
	})

	var buf bytes.Buffer
	w := r.Writer(&buf)

	// A newline-free stream that overflows the buffer bound and ends exactly
	// on the first half of the secret.
	filler := bytes.Repeat([]byte("x"), maxRedactBuffer-5)
	_, err := w.Write(filler)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Write([]byte("supersec")) // buffer now > bound, ends mid-secret
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Write([]byte("retvalue rest\n"))
	if err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	if strings.Contains(got, "supersecretvalue") {
		t.Fatal("secret escaped across the overflow flush boundary")
	}
	if !strings.Contains(got, placeholder) {
		t.Fatal("expected the reassembled secret to be redacted")
	}
}

// TestSetStaticValues verifies static targets (e.g. TLS private-key lines)
// survive Update calls, are keyed independently, and respect the minimum
// length filter.
func TestSetStaticValues(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	r.SetStaticValues("cert-a", []string{"MIIEvKEYLINEDATA", "tiny"})

	if got := r.Redact("leak MIIEvKEYLINEDATA here"); got != "leak "+placeholder+" here" {
		t.Errorf("static value not redacted: %q", got)
	}
	if got := r.Redact("tiny stays"); got != "tiny stays" {
		t.Errorf("below-minimum static value must not be redacted: %q", got)
	}

	// Update must not evict static values.
	r.Update(map[string]map[string]any{"app": {"token": "secret-token-1"}})
	if got := r.Redact("MIIEvKEYLINEDATA and secret-token-1"); got != placeholder+" and "+placeholder {
		t.Errorf("static value lost after Update: %q", got)
	}

	// Re-setting a key replaces that key's values only.
	r.SetStaticValues("cert-a", []string{"NEWKEYLINEDATA00"})
	if got := r.Redact("MIIEvKEYLINEDATA"); got != "MIIEvKEYLINEDATA" {
		t.Errorf("replaced static value still redacted: %q", got)
	}
	if got := r.Redact("NEWKEYLINEDATA00"); got != placeholder {
		t.Errorf("new static value not redacted: %q", got)
	}
}

// TestKeyMaterialValues verifies private-key PEM lines become redaction
// targets while the public BEGIN/END markers and short lines are skipped.
func TestKeyMaterialValues(t *testing.T) {
	t.Parallel()
	// Fabricated non-key test fixture; the PEM markers are the point of the test.
	key := []byte("-----BEGIN EC PRIVATE KEY-----\r\nMIGkAgEBBDBsecretline1\nMHcCAQEEIsecretline2\nab\n-----END EC PRIVATE KEY-----\n") //gitleaks:allow
	vals := KeyMaterialValues(key)
	want := []string{"MIGkAgEBBDBsecretline1", "MHcCAQEEIsecretline2"}
	if len(vals) != len(want) {
		t.Fatalf("vals = %q, want %q", vals, want)
	}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("vals[%d] = %q, want %q", i, vals[i], want[i])
		}
	}
}

// BenchmarkRedactWriterLine pins the steady-state allocation cost of the
// line-buffered redacting writer with secrets configured: one string
// conversion per emitted segment (required by the [strings.Replacer] API),
// nothing else.
func BenchmarkRedactWriterLine(b *testing.B) {
	r := NewRedactor()
	r.Update(map[string]map[string]any{"app": {"token": "supersecretvalue"}})
	w := r.Writer(io.Discard)
	line := []byte("10.2.3.4 - - GET /some/path HTTP/1.1 200 1234\n")

	b.ReportAllocs()
	for b.Loop() {
		_, _ = w.Write(line)
	}
}

// BenchmarkRedactWriterLineNoSecrets pins the zero-allocation fast path when
// no redaction targets are configured.
func BenchmarkRedactWriterLineNoSecrets(b *testing.B) {
	r := NewRedactor()
	w := r.Writer(io.Discard)
	line := []byte("10.2.3.4 - - GET /some/path HTTP/1.1 200 1234\n")

	b.ReportAllocs()
	for b.Loop() {
		_, _ = w.Write(line)
	}
}

// TestWriterOverflowFlushDoesNotSplitBorderedSecret pins the overflow-flush
// fix: a COMPLETE single-line secret with a "border" (a proper prefix equal
// to a proper suffix, e.g. "abcXabc") sitting at the overflow cut used to be
// split - tailHold only held the border-length suffix, so the secret's head
// was emitted unredacted and its tail flushed unredacted later, leaking the
// whole secret across the cut and contradicting the writer's documented
// "single-line secrets never straddle a flush boundary" guarantee.
func TestWriterOverflowFlushDoesNotSplitBorderedSecret(t *testing.T) {
	t.Parallel()

	const secret = "abcSECRETabc" // border: "abc" (len 3)
	r := NewRedactor()
	r.Update(map[string]map[string]any{"s": {"k": secret}})

	var out bytes.Buffer
	w, ok := r.Writer(&out).(interface {
		io.Writer
		Flush() error
	})
	if !ok {
		t.Fatal("redacting writer must expose Flush")
	}

	// One giant newline-free write ending exactly with the complete secret,
	// large enough to trigger the bounded-buffer overflow flush.
	payload := strings.Repeat("x", 1<<20) + secret
	_, err := w.Write([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	err = w.Flush()
	if err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if strings.Contains(got, secret) {
		t.Fatal("complete secret leaked in cleartext across the overflow flush cut")
	}
	if !strings.Contains(got, placeholder) {
		t.Fatal("secret was neither leaked nor redacted - output lost data")
	}
}

// TestWriterFlushTerminatesPartialLine pins the flush-newline fix: Flush
// emits a buffered partial line, and without a terminating newline the next
// writer's first line is appended to the truncated one, merging two log lines.
func TestWriterFlushTerminatesPartialLine(t *testing.T) {
	t.Parallel()
	r := NewRedactor()
	var out bytes.Buffer
	w, ok := r.Writer(&out).(interface {
		io.Writer
		Flush() error
	})
	if !ok {
		t.Fatal("redacting writer must expose Flush")
	}
	_, err := w.Write([]byte("dying mid-line"))
	if err != nil {
		t.Fatal(err)
	}
	err = w.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "dying mid-line\n" {
		t.Errorf("Flush output = %q, want %q (newline-terminated)", got, "dying mid-line\n")
	}
}

// TestWriterOverflowPathologicalOverlapHoldsAll covers safeCut's cut==0
// fallback: with a buffer made entirely of overlapping secret occurrences
// there is no safe cut, so the overflow flush must emit NOTHING (the buffer
// keeps growing, matching tailHold's documented worst case) and the eventual
// Flush must redact everything within one segment.
func TestWriterOverflowPathologicalOverlapHoldsAll(t *testing.T) {
	t.Parallel()
	const secret = "aaaaaa"
	r := NewRedactor()
	r.Update(map[string]map[string]any{"s": {"k": secret}})

	var out bytes.Buffer
	w, ok := r.Writer(&out).(interface {
		io.Writer
		Flush() error
	})
	if !ok {
		t.Fatal("redacting writer must expose Flush")
	}

	payload := strings.Repeat("a", maxRedactBuffer)
	if _, err := w.Write([]byte(payload)); err != nil { //nolint:noinlineerr // sequential test steps
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("overflow emitted %d bytes although every cut splits a secret occurrence", out.Len())
	}
	if err := w.Flush(); err != nil { //nolint:noinlineerr // sequential test steps
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal("pathological overlapping secrets leaked through the overflow path")
	}
	if !strings.Contains(out.String(), placeholder) {
		t.Fatal("output lost the redacted content entirely")
	}
}

// TestWriterFlushEmptyAndErrorBranches covers Flush's empty-buffer early
// return (second Flush emits nothing) and its inner-writer error path.
func TestWriterFlushEmptyAndErrorBranches(t *testing.T) {
	t.Parallel()
	r := NewRedactor()

	var out bytes.Buffer
	w, ok := r.Writer(&out).(interface {
		io.Writer
		Flush() error
	})
	if !ok {
		t.Fatal("redacting writer must expose Flush")
	}
	if _, err := w.Write([]byte("partial")); err != nil { //nolint:noinlineerr // sequential test steps
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil { //nolint:noinlineerr // sequential test steps
		t.Fatal(err)
	}
	before := out.Len()
	if err := w.Flush(); err != nil { //nolint:noinlineerr // sequential test steps
		t.Fatal(err)
	}
	if out.Len() != before {
		t.Errorf("second Flush emitted %d extra bytes, want 0", out.Len()-before)
	}

	errInner := errors.New("disk full")
	wf, ok := r.Writer(&failWriter{err: errInner}).(interface {
		io.Writer
		Flush() error
	})
	if !ok {
		t.Fatal("redacting writer must expose Flush")
	}
	if _, err := wf.Write([]byte("partial")); err != nil { //nolint:noinlineerr // sequential test steps
		t.Fatal(err)
	}
	flushErr := wf.Flush()
	if !errors.Is(flushErr, errInner) {
		t.Errorf("Flush error = %v, want inner %v", flushErr, errInner)
	}
}

// TestWriterOverflowNonStraddlingOccurrenceKeepsCut covers safeCut's
// scan-past branch: an occurrence inside the inspection window that ends
// exactly AT the cut does not straddle it, so the cut stays and the complete
// occurrence is redacted within the emitted segment.
func TestWriterOverflowNonStraddlingOccurrenceKeepsCut(t *testing.T) {
	t.Parallel()
	const secret = "abcdef" // no border: prefix "ab" is not a suffix
	r := NewRedactor()
	r.Update(map[string]map[string]any{"s": {"k": secret}})

	var out bytes.Buffer
	w, ok := r.Writer(&out).(interface {
		io.Writer
		Flush() error
	})
	if !ok {
		t.Fatal("redacting writer must expose Flush")
	}

	// Buffer ends with <secret><2-char secret prefix>: tailHold keeps "ab",
	// putting the cut exactly at the end of the complete occurrence.
	payload := strings.Repeat("x", maxRedactBuffer-len(secret)-2) + secret + secret[:2]
	if _, err := w.Write([]byte(payload)); err != nil { //nolint:noinlineerr // sequential test steps
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal("complete occurrence at the cut boundary leaked unredacted")
	}
	if !strings.Contains(out.String(), placeholder) {
		t.Fatal("complete occurrence at the cut boundary was not redacted in the emitted segment")
	}
}
