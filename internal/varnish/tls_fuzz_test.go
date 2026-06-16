package varnish

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzSanitizeCertFileNameNoTraversal asserts the security property the
// sanitizer exists for: a logical --tls-cert name, after sanitization, must
// always yield a file that lands directly inside the cert directory — never a
// path separator, never a parent-directory escape — regardless of input.
func FuzzSanitizeCertFileNameNoTraversal(f *testing.F) {
	seeds := []string{
		"web", "../escape", "../../etc/passwd", "..", ".", "", "....",
		"a/b\\c", "\x00name", "....//....", "name with spaces", "café-ñ",
		"/abs/path", "C:\\win\\path", "..\\..\\x",
		strings.Repeat("../", 64) + "passwd",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	dir := filepath.Clean("/tmp/k8s-httpcache-tls-xyz")
	f.Fuzz(func(t *testing.T, name string) {
		base := sanitizeCertFileName(name)

		if base == "" {
			t.Fatalf("sanitized name is empty for %q", name)
		}
		if strings.ContainsRune(base, '/') || strings.ContainsRune(base, filepath.Separator) {
			t.Fatalf("sanitized %q contains a path separator: %q", name, base)
		}
		if base == "." || base == ".." {
			t.Fatalf("sanitized %q is a dot directory: %q", name, base)
		}

		full := filepath.Join(dir, base+".pem")
		// The file must sit directly in dir and stay under it.
		if got := filepath.Dir(full); got != dir {
			t.Fatalf("sanitized %q escaped dir: file=%q parent=%q want=%q", name, full, got, dir)
		}
		if !strings.HasPrefix(full, dir+string(filepath.Separator)) {
			t.Fatalf("sanitized %q produced a path outside dir: %q", name, full)
		}
	})
}
