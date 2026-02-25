package redact

import (
	"bytes"
	"errors"
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
	_, err := w.Write([]byte("error: my-secret-token leaked"))
	if err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	want := "error: [REDACTED] leaked"
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
	input := []byte("token: my-secret-token")
	n, err := w.Write(input)

	if !errors.Is(err, errInner) {
		t.Errorf("expected inner error, got %v", err)
	}
	// When the inner writer fails, we forward its n (not len(input)).
	if n != 3 {
		t.Errorf("expected n=3 from inner writer, got %d", n)
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
	// This is the documented limitation — the halves are not redacted.
	buf.Reset()
	_, _ = w.Write([]byte("token: my-secret"))
	_, _ = w.Write([]byte("-token\n"))
	// We expect the secret to survive because neither half matches.
	if strings.Contains(buf.String(), "[REDACTED]") {
		// If this ever passes, it means we added buffered redaction — great!
		t.Log("cross-boundary redaction now works (buffered writer?)")
	}
}
