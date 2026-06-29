package varnish

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// readMetric extracts the float value from a counter or gauge metric.
func readMetric(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var d dto.Metric
	err := m.Write(&d)
	if err != nil {
		t.Fatalf("reading metric: %v", err)
	}
	switch {
	case d.Counter != nil:
		return d.Counter.GetValue()
	case d.Gauge != nil:
		return d.Gauge.GetValue()
	default:
		return 0
	}
}

// genTestCert returns a freshly-generated self-signed certificate and key in PEM
// form (cert, key), suitable for combinePEM/X509KeyPair validation. It accepts a
// [testing.TB] so both the unit tests and the fuzz harness can seed a valid pair.
func genTestCert(tb testing.TB) ([]byte, []byte) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("generating key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatalf("creating cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		tb.Fatalf("marshaling key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM
}

// tlsRunFn returns a runFn that models the varnishadm tls.cert.* staging API:
// tls.cert.list reports the active ids; tls.cert.commit appends a new id.
func tlsRunFn(active *[]string, nextID *int) func(string, []string) (string, error) {
	return func(_ string, args []string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "tls.cert.list":
			return strings.Join(*active, "\n"), nil
		case "tls.cert.commit":
			id := fmt.Sprintf("cert%d", *nextID)
			*nextID++
			*active = append(*active, id)

			return "", nil
		default:
			return "", nil
		}
	}
}

func findAdmCall(calls [][]string, sub string) []string {
	for _, c := range calls {
		if len(c) >= 2 && c[1] == sub {
			return c
		}
	}

	return nil
}

func hasAdmCall(calls [][]string, args ...string) bool {
	for _, c := range calls {
		if len(c) >= 1+len(args) && slices.Equal(c[1:1+len(args)], args) {
			return true
		}
	}

	return false
}

func TestTLSSupported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		major int
		want  bool
	}{
		{6, false},
		{8, false},
		{9, true},
		{trunkMajorVersion, true},
	}
	for _, tt := range tests {
		m := newTestManager(&mockRunner{})
		m.majorVersion = tt.major
		if got := m.TLSSupported(); got != tt.want {
			t.Errorf("major %d: TLSSupported()=%v, want %v", tt.major, got, tt.want)
		}
	}
}

func TestLoadCertUnsupportedVersion(t *testing.T) {
	t.Parallel()
	r := &mockRunner{runFn: func(string, []string) (string, error) { return "", nil }}
	m := newTestManager(r)
	m.majorVersion = 8
	cert, key := genTestCert(t)

	err := m.LoadCert("frontend", cert, key, nil)
	if !errors.Is(err, errTLSUnsupported) {
		t.Fatalf("expected errTLSUnsupported, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no varnishadm calls, got %v", r.calls)
	}
}

func TestLoadCertEmptyMaterialNoOp(t *testing.T) {
	t.Parallel()
	r := &mockRunner{runFn: func(string, []string) (string, error) { return "", nil }}
	m := newTestManager(r)
	m.majorVersion = 9

	err := m.LoadCert("frontend", nil, nil, nil)
	if err != nil {
		t.Fatalf("expected nil for empty material, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no varnishadm calls for empty material, got %v", r.calls)
	}
}

func TestLoadCertFirstLoad(t *testing.T) {
	t.Parallel()
	active := []string{}
	nextID := 0
	r := &mockRunner{runFn: tlsRunFn(&active, &nextID)}
	m := newTestManager(r)
	m.majorVersion = 9
	t.Cleanup(m.CleanupTLS)

	cert, key := genTestCert(t)
	err := m.LoadCert("frontend", cert, key, nil)
	if err != nil {
		t.Fatalf("LoadCert error: %v", err)
	}

	loadCall := findAdmCall(r.calls, "tls.cert.load")
	if loadCall == nil {
		t.Fatal("expected tls.cert.load call")
	}
	if !strings.HasSuffix(loadCall[2], "frontend.pem") {
		t.Errorf("expected load path to end with frontend.pem, got %q", loadCall[2])
	}
	if findAdmCall(r.calls, "tls.cert.commit") == nil {
		t.Error("expected tls.cert.commit call")
	}
	if findAdmCall(r.calls, "tls.cert.discard") != nil {
		t.Error("did not expect a discard on first load")
	}
	if got := m.tlsCertIDs["frontend"]; got != "cert0" {
		t.Errorf("expected tracked id cert0, got %q", got)
	}
}

func TestLoadCertRotationDiscardsPrior(t *testing.T) {
	t.Parallel()
	active := []string{"cert0"}
	nextID := 1
	r := &mockRunner{runFn: tlsRunFn(&active, &nextID)}
	m := newTestManager(r)
	m.majorVersion = 9
	m.tlsCertIDs["frontend"] = "cert0" // a cert is already active for this name
	t.Cleanup(m.CleanupTLS)

	cert, key := genTestCert(t)
	err := m.LoadCert("frontend", cert, key, nil)
	if err != nil {
		t.Fatalf("LoadCert error: %v", err)
	}

	if got := m.tlsCertIDs["frontend"]; got != "cert1" {
		t.Errorf("expected tracked id cert1 after rotation, got %q", got)
	}
	if !hasAdmCall(r.calls, "tls.cert.discard", "cert0") {
		t.Errorf("expected discard of prior cert0, calls: %v", r.calls)
	}
}

func TestLoadCertLoadFailureRollsBack(t *testing.T) {
	t.Parallel()
	r := &mockRunner{runFn: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "tls.cert.load" {
			return "bad cert", errors.New("load failed")
		}

		return "", nil
	}}
	m := newTestManager(r)
	m.majorVersion = 9
	t.Cleanup(m.CleanupTLS)

	cert, key := genTestCert(t)
	err := m.LoadCert("frontend", cert, key, nil)
	if err == nil || !strings.Contains(err.Error(), "tls.cert.load") {
		t.Fatalf("expected tls.cert.load error, got %v", err)
	}
	if findAdmCall(r.calls, "tls.cert.rollback") == nil {
		t.Error("expected rollback after load failure")
	}
	if findAdmCall(r.calls, "tls.cert.commit") != nil {
		t.Error("did not expect commit after load failure")
	}
}

func TestLoadCertCommitFailureRollsBack(t *testing.T) {
	t.Parallel()
	r := &mockRunner{runFn: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "tls.cert.commit" {
			return "commit error", errors.New("commit failed")
		}

		return "", nil
	}}
	m := newTestManager(r)
	m.majorVersion = 9
	t.Cleanup(m.CleanupTLS)

	cert, key := genTestCert(t)
	err := m.LoadCert("frontend", cert, key, nil)
	if err == nil || !strings.Contains(err.Error(), "tls.cert.commit") {
		t.Fatalf("expected tls.cert.commit error, got %v", err)
	}
	if findAdmCall(r.calls, "tls.cert.rollback") == nil {
		t.Error("expected rollback after commit failure")
	}
}

func TestCombinePEM(t *testing.T) {
	t.Parallel()
	cert, key := genTestCert(t)

	out, err := combinePEM(cert, key, nil)
	if err != nil {
		t.Fatalf("combinePEM error: %v", err)
	}
	// Key block comes before the certificate block.
	keyIdx := bytes.Index(out, []byte("PRIVATE KEY"))
	certIdx := bytes.Index(out, []byte("CERTIFICATE"))
	if keyIdx < 0 || certIdx < 0 || keyIdx > certIdx {
		t.Errorf("expected key before certificate in combined PEM:\n%s", out)
	}

	_, err = combinePEM(nil, key, nil)
	if !errors.Is(err, errEmptyTLSMaterial) {
		t.Errorf("expected errEmptyTLSMaterial for missing cert, got %v", err)
	}

	// Mismatched key/cert pair must be rejected.
	_, key2 := genTestCert(t)
	_, err = combinePEM(cert, key2, nil)
	if err == nil {
		t.Error("expected error for mismatched key pair")
	}
}

func TestSanitizeCertFileName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"frontend":    "frontend",
		"my.cert-1":   "my.cert-1",
		"a/b":         "a_b",
		"../escape":   ".._escape",
		"":            defaultCertFileName,
		"..":          defaultCertFileName,
		"weird name!": "weird_name_",
	}
	for in, want := range tests {
		if got := sanitizeCertFileName(in); got != want {
			t.Errorf("sanitizeCertFileName(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestLoadCertMismatchedPairErrors(t *testing.T) {
	t.Parallel()
	r := &mockRunner{runFn: func(string, []string) (string, error) { return "", nil }}
	m := newTestManager(r)
	m.majorVersion = 9
	t.Cleanup(m.CleanupTLS)

	cert, _ := genTestCert(t)
	_, key2 := genTestCert(t) // key from a different pair
	err := m.LoadCert("frontend", cert, key2, nil)
	if err == nil {
		t.Fatal("expected error for mismatched key/cert pair")
	}
	if findAdmCall(r.calls, "tls.cert.load") != nil {
		t.Error("did not expect tls.cert.load for an invalid pair")
	}
	if got := readMetric(t, m.metrics.TLSCertReloadsTotal.WithLabelValues("frontend", "error")); got != 1 {
		t.Errorf("tls_cert_reloads_total(frontend,error) = %v, want 1", got)
	}
}

func TestLoadCertWithCAWritesChain(t *testing.T) {
	t.Parallel()
	active := []string{}
	nextID := 0
	r := &mockRunner{runFn: tlsRunFn(&active, &nextID)}
	m := newTestManager(r)
	m.majorVersion = 9
	t.Cleanup(m.CleanupTLS)

	cert, key := genTestCert(t)
	ca, _ := genTestCert(t) // a second certificate used as the CA chain
	err := m.LoadCert("frontend", cert, key, ca)
	if err != nil {
		t.Fatalf("LoadCert error: %v", err)
	}

	loadCall := findAdmCall(r.calls, "tls.cert.load")
	if loadCall == nil {
		t.Fatal("expected tls.cert.load call")
	}
	pemBytes, readErr := os.ReadFile(loadCall[2])
	if readErr != nil {
		t.Fatalf("reading combined PEM: %v", readErr)
	}
	if n := bytes.Count(pemBytes, []byte("-----BEGIN CERTIFICATE-----")); n != 2 {
		t.Errorf("expected 2 CERTIFICATE blocks (leaf + CA), got %d", n)
	}
	if !bytes.Contains(pemBytes, []byte("PRIVATE KEY")) {
		t.Error("expected a PRIVATE KEY block in the combined PEM")
	}
}

func TestLoadCertMetrics(t *testing.T) {
	t.Parallel()
	active := []string{}
	nextID := 0
	r := &mockRunner{runFn: tlsRunFn(&active, &nextID)}
	m := newTestManager(r)
	m.majorVersion = 9
	t.Cleanup(m.CleanupTLS)

	cert, key := genTestCert(t)
	err := m.LoadCert("frontend", cert, key, nil)
	if err != nil {
		t.Fatalf("LoadCert error: %v", err)
	}

	checks := []struct {
		name string
		val  float64
	}{
		{"load/success", readMetric(t, m.metrics.TLSCertOperationsTotal.WithLabelValues("load", "success"))},
		{"commit/success", readMetric(t, m.metrics.TLSCertOperationsTotal.WithLabelValues("commit", "success"))},
		{"reload/success", readMetric(t, m.metrics.TLSCertReloadsTotal.WithLabelValues("frontend", "success"))},
		{"active", readMetric(t, m.metrics.TLSCertsActive)},
	}
	for _, c := range checks {
		if c.val != 1 {
			t.Errorf("metric %s = %v, want 1", c.name, c.val)
		}
	}
}

func TestListCertIDs(t *testing.T) {
	t.Parallel()

	// Error from varnishadm yields an empty set.
	rErr := &mockRunner{runFn: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "tls.cert.list" {
			return "", errors.New("boom")
		}

		return "", nil
	}}
	mErr := newTestManager(rErr)
	if ids := mErr.listCertIDs(); len(ids) != 0 {
		t.Errorf("expected empty set on error, got %v", ids)
	}

	// Multi-line output is parsed into the set of cert ids.
	rOK := &mockRunner{runFn: func(string, []string) (string, error) {
		return "Label   ID\nfoo     cert0\nbar     cert2\n", nil
	}}
	mOK := newTestManager(rOK)
	ids := mOK.listCertIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %v", ids)
	}
	for _, want := range []string{"cert0", "cert2"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("missing id %q in %v", want, ids)
		}
	}
}

func TestLoadCertDiscardErrorIsBestEffort(t *testing.T) {
	t.Parallel()
	active := []string{"cert0"}
	nextID := 1
	r := &mockRunner{runFn: func(_ string, args []string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "tls.cert.discard":
			return "", errors.New("discard failed")
		case "tls.cert.list":
			return strings.Join(active, "\n"), nil
		case "tls.cert.commit":
			id := fmt.Sprintf("cert%d", nextID)
			nextID++
			active = append(active, id)

			return "", nil
		default:
			return "", nil
		}
	}}
	m := newTestManager(r)
	m.majorVersion = 9
	m.tlsCertIDs["frontend"] = "cert0"
	t.Cleanup(m.CleanupTLS)

	cert, key := genTestCert(t)
	// A failing discard must not fail the rotation (it is best-effort).
	err := m.LoadCert("frontend", cert, key, nil)
	if err != nil {
		t.Fatalf("LoadCert should succeed despite discard error: %v", err)
	}
	if !hasAdmCall(r.calls, "tls.cert.discard", "cert0") {
		t.Error("expected a discard attempt for the prior cert")
	}
	if got := m.tlsCertIDs["frontend"]; got != "cert1" {
		t.Errorf("expected tracked id cert1, got %q", got)
	}
}

func TestLoadCertWriteFileError(t *testing.T) {
	t.Parallel()
	r := &mockRunner{runFn: func(string, []string) (string, error) { return "", nil }}
	m := newTestManager(r)
	m.majorVersion = 9
	// Point the cert dir at a regular file so writing a PEM under it fails.
	notDir := filepath.Join(t.TempDir(), "not-a-dir")
	writeErr := os.WriteFile(notDir, []byte("x"), 0o600)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	m.tlsCertDir = notDir

	cert, key := genTestCert(t)
	err := m.LoadCert("frontend", cert, key, nil)
	if err == nil {
		t.Fatal("expected an error writing the PEM under a non-directory")
	}
	if findAdmCall(r.calls, "tls.cert.load") != nil {
		t.Error("did not expect tls.cert.load when writing the PEM failed")
	}
	if got := readMetric(t, m.metrics.TLSCertReloadsTotal.WithLabelValues("frontend", "error")); got != 1 {
		t.Errorf("tls_cert_reloads_total(frontend,error) = %v, want 1", got)
	}
}
