package varnish

import (
	"crypto/tls"
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

// FuzzCombinePEM feeds arbitrary cert/key/ca bytes through combinePEM, which
// validates the pair with [tls.X509KeyPair] (crypto/x509 + encoding/pem) before
// concatenating. The bytes originate from a Kubernetes Secret, so the crypto
// parsers must tolerate any input without panicking. When combinePEM accepts the
// material, the concatenated blob must itself be a usable key pair.
func FuzzCombinePEM(f *testing.F) {
	cert, key := genTestCert(f)
	// A valid pair (with and without a CA), then mismatched/garbage/empty/
	// truncated material so both the accept and reject paths are reached.
	f.Add(cert, key, []byte(nil))
	f.Add(cert, key, cert)
	f.Add(cert, []byte("not a key"), []byte(nil))
	f.Add([]byte("garbage"), []byte("garbage"), []byte(nil))
	f.Add([]byte(nil), []byte(nil), []byte(nil))
	f.Add([]byte("-----BEGIN CERTIFICATE-----\ntruncated"), key, []byte(nil))

	f.Fuzz(func(t *testing.T, cert, key, ca []byte) {
		out, err := combinePEM(cert, key, ca)
		if err != nil {
			return
		}
		// combinePEM only returns nil error after X509KeyPair validated the pair,
		// so the combined hitch-style blob (key + cert + ca) must re-parse as a
		// usable pair.
		_, err = tls.X509KeyPair(out, out)
		if err != nil {
			t.Fatalf("combinePEM produced an unusable blob: %v", err)
		}
	})
}
