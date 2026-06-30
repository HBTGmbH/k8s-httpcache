package watcher

import (
	"bytes"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// readTLSCertChanges reads from the TLSCertWatcher's Changes channel with a timeout.
func readTLSCertChanges(t *testing.T, w *TLSCertWatcher) TLSCertData {
	t.Helper()
	select {
	case data := <-w.Changes():
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for TLS cert change")

		return TLSCertData{}
	}
}

// assertNoTLSCertChanges verifies no message arrives within the timeout.
func assertNoTLSCertChanges(t *testing.T, w *TLSCertWatcher, timeout time.Duration) {
	t.Helper()
	select {
	case data := <-w.Changes():
		t.Fatalf("unexpected TLS cert change received: %v", data)
	case <-time.After(timeout):
		// OK - no change
	}
}

func TestTLSCertWatcherInitialSync(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-tls", map[string][]byte{
		tlsCertKey: []byte("CERT"),
		tlsKeyKey:  []byte("KEY"),
		tlsCAKey:   []byte("CA"),
	})
	clientset := fake.NewClientset(secret)
	w := NewTLSCertWatcher(clientset, "default", "my-tls")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readTLSCertChanges(t, w)
	if !bytes.Equal(data.Cert, []byte("CERT")) || !bytes.Equal(data.Key, []byte("KEY")) || !bytes.Equal(data.CA, []byte("CA")) {
		t.Errorf("unexpected cert data: %+v", data)
	}
	if data.Empty() {
		t.Error("expected non-empty cert data")
	}
}

func TestTLSCertWatcherMissing(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := NewTLSCertWatcher(clientset, "default", "missing-tls")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readTLSCertChanges(t, w)
	if !data.Empty() {
		t.Fatalf("expected empty data for missing Secret, got %+v", data)
	}
}

func TestTLSCertWatcherRotation(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-tls", map[string][]byte{
		tlsCertKey: []byte("CERT1"),
		tlsKeyKey:  []byte("KEY1"),
	})
	clientset := fake.NewClientset(secret)
	w := NewTLSCertWatcher(clientset, "default", "my-tls")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	waitForWatch()
	data := readTLSCertChanges(t, w)
	if !bytes.Equal(data.Cert, []byte("CERT1")) {
		t.Fatalf("expected CERT1, got %q", data.Cert)
	}

	// Rotate the certificate.
	s2, err := clientset.CoreV1().Secrets("default").Get(ctx, "my-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Secret: %v", err)
	}
	s2.Data[tlsCertKey] = []byte("CERT2")
	s2.Data[tlsKeyKey] = []byte("KEY2")
	_, err = clientset.CoreV1().Secrets("default").Update(ctx, s2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Secret: %v", err)
	}

	data = readTLSCertChanges(t, w)
	if !bytes.Equal(data.Cert, []byte("CERT2")) || !bytes.Equal(data.Key, []byte("KEY2")) {
		t.Errorf("expected rotated cert, got %+v", data)
	}
}

func TestTLSCertWatcherDeduplicatesUnchanged(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-tls", map[string][]byte{
		tlsCertKey: []byte("CERT"),
		tlsKeyKey:  []byte("KEY"),
	})
	clientset := fake.NewClientset(secret)
	w := NewTLSCertWatcher(clientset, "default", "my-tls")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	waitForWatch()
	readTLSCertChanges(t, w)

	// Update an unrelated field (annotation) - cert material stays the same.
	s2, err := clientset.CoreV1().Secrets("default").Get(ctx, "my-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Secret: %v", err)
	}
	s2.Annotations = map[string]string{"unrelated": "change"}
	_, err = clientset.CoreV1().Secrets("default").Update(ctx, s2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Secret: %v", err)
	}

	assertNoTLSCertChanges(t, w, 500*time.Millisecond)
}

func TestTLSCertWatcherDeleted(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-tls", map[string][]byte{
		tlsCertKey: []byte("CERT"),
		tlsKeyKey:  []byte("KEY"),
	})
	clientset := fake.NewClientset(secret)
	w := NewTLSCertWatcher(clientset, "default", "my-tls")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	waitForWatch()
	data := readTLSCertChanges(t, w)
	if data.Empty() {
		t.Fatalf("expected non-empty initial data, got %+v", data)
	}

	err := clientset.CoreV1().Secrets("default").Delete(ctx, "my-tls", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Secret: %v", err)
	}

	data = readTLSCertChanges(t, w)
	if !data.Empty() {
		t.Fatalf("expected empty data after delete, got %+v", data)
	}
}

func TestTLSCertWatcherAppearsLate(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := NewTLSCertWatcher(clientset, "default", "late-tls")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initially no Secret → empty data.
	data := readTLSCertChanges(t, w)
	if !data.Empty() {
		t.Fatalf("expected empty data initially, got %+v", data)
	}

	// Secret appears after startup.
	secret := makeSecret("late-tls", map[string][]byte{
		tlsCertKey: []byte("CERT"),
		tlsKeyKey:  []byte("KEY"),
	})
	_, err := clientset.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Secret: %v", err)
	}

	data = readTLSCertChanges(t, w)
	if !bytes.Equal(data.Cert, []byte("CERT")) {
		t.Errorf("expected CERT, got %q", data.Cert)
	}
}
