package varnish

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// tlsMinMajorVersion is the lowest cache major version with native TLS support.
// Varnish Cache and Vinyl Cache gained in-core TLS in their 9.0 release.
const tlsMinMajorVersion = 9

// logKeyCert is the structured-log attribute key for a logical certificate name.
const logKeyCert = "cert"

// defaultCertFileName is the fallback base name when a logical certificate name
// sanitises to an unsafe or empty value.
const defaultCertFileName = "tlscert"

var (
	errTLSUnsupported   = errors.New("native TLS requires Varnish/Vinyl major version >= 9")
	errEmptyTLSMaterial = errors.New("TLS certificate or private key is empty")
)

// tlsCertIDRe matches a varnishadm TLS certificate identifier when it forms a
// complete whitespace-delimited field (e.g. "cert0"). Full-field matching
// keeps id extraction independent of the exact column layout of
// `tls.cert.list` output without misreading name/SAN/path tokens that merely
// contain a cert<N> substring (e.g. "foo.cert12.example" or "my-cert5", which
// the previous substring match would have counted as ids - corrupting the
// before/after diff LoadCert uses to identify the committed certificate).
var tlsCertIDRe = regexp.MustCompile(`^cert\d+$`)

// TLSSupported reports whether the detected cache daemon supports native TLS
// (Varnish/Vinyl major version >= 9). DetectVersion must have run first.
func (m *Manager) TLSSupported() bool {
	return m.majorVersion.Load() >= tlsMinMajorVersion
}

// LoadCert installs a frontend TLS certificate from raw PEM material via the
// varnishadm tls.cert.* staging API. The key and certificate (plus optional CA)
// are combined into a single hitch-style PEM file, staged with tls.cert.load,
// activated with tls.cert.commit, and - on rotation - the certificate this
// logical name previously used is discarded so in-flight sessions on it finish
// gracefully while new handshakes use the fresh certificate.
//
// LoadCert is fully serialised: concurrent rotations cannot interleave the
// shared staging area. It is independent of VCL reloads and never restarts the
// cache, so the cache stays warm across certificate rotations.
//
// Empty material (cert or key absent - e.g. the Secret is missing or was
// deleted) is a no-op that leaves any already-active certificate untouched.
func (m *Manager) LoadCert(name string, cert, key, ca []byte) error {
	if !m.TLSSupported() {
		return errTLSUnsupported
	}

	m.tlsMu.Lock()
	defer m.tlsMu.Unlock()

	if len(cert) == 0 || len(key) == 0 {
		m.log.Warn("TLS certificate material is empty, keeping previous certificate active", logKeyCert, name)
		m.metrics.TLSCertReloadsTotal.WithLabelValues(name, "noop").Inc()

		return nil
	}

	pem, err := combinePEM(cert, key, ca)
	if err != nil {
		m.metrics.TLSCertReloadsTotal.WithLabelValues(name, "error").Inc()

		return fmt.Errorf("combining PEM for %q: %w", name, err)
	}

	err = m.ensureTLSDir()
	if err != nil {
		m.metrics.TLSCertReloadsTotal.WithLabelValues(name, "error").Inc()

		return err
	}
	path, err := m.writeCertFile(name, pem)
	if err != nil {
		m.metrics.TLSCertReloadsTotal.WithLabelValues(name, "error").Inc()

		return err
	}

	before, beforeErr := m.listCertIDs()
	prior := m.tlsCertIDs[name]

	// Stage the new certificate.
	resp, err := m.tlsOp("load", "tls.cert.load", path)
	if err != nil {
		_, _ = m.tlsOp("rollback", "tls.cert.rollback")
		m.metrics.TLSCertReloadsTotal.WithLabelValues(name, "error").Inc()

		return fmt.Errorf("tls.cert.load: %w: %s", err, resp)
	}

	// Activate the staged certificate.
	resp, err = m.tlsOp("commit", "tls.cert.commit")
	if err != nil {
		_, _ = m.tlsOp("rollback", "tls.cert.rollback")
		m.metrics.TLSCertReloadsTotal.WithLabelValues(name, "error").Inc()

		return fmt.Errorf("tls.cert.commit: %w: %s", err, resp)
	}

	// Identify the newly-added certificate id by set difference. The diff is
	// only meaningful when both list calls succeeded: with a failed "before"
	// list every id looks new and an arbitrary one - possibly another
	// certificate's active id - would be recorded for this name and discarded
	// on a later rotation; with a failed "after" list a successful commit
	// would look like it removed every certificate.
	after, afterErr := m.listCertIDs()
	newID := ""
	if beforeErr == nil && afterErr == nil {
		for id := range after {
			if _, existed := before[id]; !existed {
				newID = id

				break
			}
		}
	}
	if newID != "" {
		m.tlsCertIDs[name] = newID
	} else {
		// Keep the recorded id unchanged: the prior certificate is not
		// discarded below, so a later successful rotation still knows which
		// id to retry discarding.
		m.log.Warn("could not identify newly committed TLS certificate id, skipping discard of prior certificate",
			logKeyCert, name, "before_err", beforeErr, "after_err", afterErr)
	}

	// Discard the certificate this name previously used (rotation). Only when
	// we positively identified a distinct new id, so we never discard the cert
	// we just committed.
	activeCerts := len(after)
	if newID != "" && prior != "" && prior != newID {
		_, discardErr := m.tlsOp("discard", "tls.cert.discard", prior)
		if discardErr != nil {
			m.log.Warn("failed to discard previous TLS certificate", logKeyCert, name, "id", prior, "error", discardErr)
		} else {
			// The post-commit listing still contained the prior certificate;
			// count it out so the gauge reflects the post-discard state
			// (setting the gauge from len(after) alone would overcount by one
			// until the next rotation).
			activeCerts--
		}
	}
	if afterErr == nil {
		m.metrics.TLSCertsActive.Set(float64(activeCerts))
	}

	m.metrics.TLSCertReloadsTotal.WithLabelValues(name, "success").Inc()
	m.log.Info("loaded TLS certificate", logKeyCert, name, "id", newID, "rotated_from", prior)

	return nil
}

// tlsOp runs a varnishadm TLS command and records its outcome in the
// TLSCertOperationsTotal counter under the given operation label.
func (m *Manager) tlsOp(operation string, args ...string) (string, error) {
	resp, err := m.adm(args...)
	result := "success"
	if err != nil {
		result = "error"
	}
	m.metrics.TLSCertOperationsTotal.WithLabelValues(operation, result).Inc()

	return resp, err
}

// CleanupTLS removes the temporary directory holding combined PEM files.
// It is best-effort and safe to call when no TLS certificates were loaded.
func (m *Manager) CleanupTLS() {
	m.tlsMu.Lock()
	defer m.tlsMu.Unlock()

	if m.tlsCertDir == "" {
		return
	}
	err := os.RemoveAll(m.tlsCertDir)
	if err != nil {
		m.log.Warn("failed to remove TLS cert dir", "dir", m.tlsCertDir, "error", err)
	}
	m.tlsCertDir = ""
}

// tlsListAttempts and tlsListRetryDelay bound listCertIDs' retries. A
// transient tls.cert.list failure during a rotation makes the just-committed
// certificate unidentifiable, and the prior certificate is then never
// discarded - a permanent active-cert leak in varnishd per occurrence - so a
// short retry is worth blocking the event loop a few hundred milliseconds.
const (
	tlsListAttempts   = 3
	tlsListRetryDelay = 100 * time.Millisecond
)

// listCertIDs returns the set of active TLS certificate ids reported by
// `tls.cert.list`, retrying transient failures, or an error when every
// attempt failed. Callers must not treat a failed listing as an empty set:
// LoadCert's before/after diff would misidentify the committed certificate id.
func (m *Manager) listCertIDs() (map[string]struct{}, error) {
	var resp string
	var err error
	for attempt := range tlsListAttempts {
		if attempt > 0 {
			time.Sleep(tlsListRetryDelay)
		}
		resp, err = m.adm("tls.cert.list")
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tls.cert.list: %w", err)
	}
	ids := make(map[string]struct{})
	for field := range strings.FieldsSeq(resp) {
		if tlsCertIDRe.MatchString(field) {
			ids[field] = struct{}{}
		}
	}

	return ids, nil
}

// ensureTLSDir lazily creates the temp directory that holds combined PEM files.
// Callers must hold tlsMu.
func (m *Manager) ensureTLSDir() error {
	if m.tlsCertDir != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "k8s-httpcache-tls-")
	if err != nil {
		return fmt.Errorf("creating TLS cert dir: %w", err)
	}
	m.tlsCertDir = dir

	return nil
}

// writeCertFile atomically writes pem to a stable per-name path (0600) so that
// rotations overwrite in place and `tls.cert.reload` can re-read the same path.
// Callers must hold tlsMu.
func (m *Manager) writeCertFile(name string, pem []byte) (string, error) {
	path := filepath.Join(m.tlsCertDir, sanitizeCertFileName(name)+".pem")
	tmp := path + ".tmp"
	err := os.WriteFile(tmp, pem, 0o600)
	if err != nil {
		return "", fmt.Errorf("writing TLS cert file: %w", err)
	}
	err = os.Rename(tmp, path)
	if err != nil {
		_ = os.Remove(tmp)

		return "", fmt.Errorf("renaming TLS cert file: %w", err)
	}

	return path, nil
}

// combinePEM concatenates the private key, certificate (leaf + chain) and an
// optional CA certificate into a single hitch-style PEM blob, validating that
// the key and certificate form a usable pair before writing anything.
func combinePEM(cert, key, ca []byte) ([]byte, error) {
	if len(cert) == 0 || len(key) == 0 {
		return nil, errEmptyTLSMaterial
	}
	_, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("invalid TLS key pair: %w", err)
	}

	var buf bytes.Buffer
	writePEMBlock(&buf, key)
	writePEMBlock(&buf, cert)
	if len(ca) > 0 {
		writePEMBlock(&buf, ca)
	}

	return buf.Bytes(), nil
}

// writePEMBlock appends a PEM block to buf, normalising it to end with exactly
// one trailing newline so concatenated blocks stay well-formed.
func writePEMBlock(buf *bytes.Buffer, block []byte) {
	// bytes.Buffer.Write/WriteByte never return a non-nil error (they panic on
	// overflow), so the errors are safe to discard.
	_, _ = buf.Write(bytes.TrimRight(block, "\r\n "))
	_ = buf.WriteByte('\n')
}

// sanitizeCertFileName maps a logical certificate name to a filesystem-safe
// base name, preventing path traversal from user-supplied --tls-cert names.
// When sanitisation had to alter the name, a short hash of the original is
// appended: character replacement is lossy, so two distinct logical names
// (e.g. "web api" and "web_api") would otherwise collapse onto the same PEM
// file and one certificate's material would overwrite the other's.
func sanitizeCertFileName(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
	if safe == name && safe != "" && safe != "." && safe != ".." {
		return safe
	}

	if safe == "" || safe == "." || safe == ".." {
		safe = defaultCertFileName
	}
	sum := sha256.Sum256([]byte(name))

	return safe + "-" + hex.EncodeToString(sum[:4])
}
