package watcher

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// readSecretChanges reads from the SecretWatcher's Changes channel with a timeout.
func readSecretChanges(t *testing.T, w *SecretWatcher) map[string]any {
	t.Helper()
	select {
	case data := <-w.Changes():
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Secret change")

		return nil
	}
}

// assertNoSecretChanges verifies no message arrives within the timeout.
func assertNoSecretChanges(t *testing.T, w *SecretWatcher, timeout time.Duration) {
	t.Helper()
	select {
	case data := <-w.Changes():
		t.Fatalf("unexpected Secret change received: %v", data)
	case <-time.After(timeout):
		// OK — no change
	}
}

func makeSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Data: data,
	}
}

func TestSecretWatcherInitialSync(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-secret", map[string][]byte{"key": []byte("value")})
	clientset := fake.NewClientset(secret)
	w := NewSecretWatcher(clientset, "default", "my-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readSecretChanges(t, w)
	if data["key"] != "value" {
		t.Errorf("expected key=value, got %v", data)
	}
}

func TestSecretWatcherMissing(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := NewSecretWatcher(clientset, "default", "missing-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readSecretChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data for missing Secret, got %v", data)
	}
}

func TestSecretWatcherUpdate(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-secret", map[string][]byte{"key": []byte("v1")})
	clientset := fake.NewClientset(secret)
	w := NewSecretWatcher(clientset, "default", "my-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initial sync.
	waitForWatch()
	data := readSecretChanges(t, w)
	if data["key"] != "v1" {
		t.Fatalf("expected v1, got %v", data)
	}

	// Update the Secret.
	s2, err := clientset.CoreV1().Secrets("default").Get(ctx, "my-secret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Secret: %v", err)
	}
	s2.Data["key"] = []byte("v2")
	_, err = clientset.CoreV1().Secrets("default").Update(ctx, s2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Secret: %v", err)
	}

	data = readSecretChanges(t, w)
	if data["key"] != "v2" {
		t.Errorf("expected v2, got %v", data)
	}
}

func TestSecretWatcherDeduplicatesUnchanged(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-secret", map[string][]byte{"key": []byte("value")})
	clientset := fake.NewClientset(secret)
	w := NewSecretWatcher(clientset, "default", "my-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial state.
	waitForWatch()
	readSecretChanges(t, w)

	// Update an unrelated field (annotation) — data stays the same.
	s2, err := clientset.CoreV1().Secrets("default").Get(ctx, "my-secret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Secret: %v", err)
	}
	s2.Annotations = map[string]string{"unrelated": "change"}
	_, err = clientset.CoreV1().Secrets("default").Update(ctx, s2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating Secret: %v", err)
	}

	// Should NOT deliver a duplicate change.
	assertNoSecretChanges(t, w, 500*time.Millisecond)
}

func TestSecretWatcherDeleted(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-secret", map[string][]byte{"key": []byte("value")})
	clientset := fake.NewClientset(secret)
	w := NewSecretWatcher(clientset, "default", "my-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initial: has data.
	waitForWatch()
	data := readSecretChanges(t, w)
	if data["key"] != "value" {
		t.Fatalf("expected key=value, got %v", data)
	}

	// Delete the Secret.
	err := clientset.CoreV1().Secrets("default").Delete(ctx, "my-secret", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting Secret: %v", err)
	}

	data = readSecretChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data after delete, got %v", data)
	}
}

func TestSecretWatcherAppearsLate(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	w := NewSecretWatcher(clientset, "default", "late-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initially no Secret → empty data.
	data := readSecretChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data initially, got %v", data)
	}

	// Secret appears after startup.
	secret := makeSecret("late-secret", map[string][]byte{"greeting": []byte("hello")})
	_, err := clientset.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Secret: %v", err)
	}

	data = readSecretChanges(t, w)
	if data["greeting"] != "hello" {
		t.Errorf("expected greeting=hello, got %v", data)
	}
}

func TestSecretWatcherInvalidYAMLFallback(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-secret", map[string][]byte{
		"valid":   []byte("hello"),
		"invalid": []byte("a: [unterminated"),
	})
	clientset := fake.NewClientset(secret)
	w := NewSecretWatcher(clientset, "default", "my-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readSecretChanges(t, w)

	// Valid key parsed normally.
	if data["valid"] != "hello" {
		t.Errorf("expected valid=hello, got %v", data["valid"])
	}

	// Invalid YAML falls back to raw string.
	if data["invalid"] != "a: [unterminated" {
		t.Errorf("expected raw string fallback, got %v (%T)", data["invalid"], data["invalid"])
	}
}

func TestSecretWatcherYAMLParsing(t *testing.T) {
	t.Parallel()
	secret := makeSecret("my-secret", map[string][]byte{
		"plain":      []byte("hello"),
		"number":     []byte("42"),
		"boolean":    []byte("true"),
		"structured": []byte("host: example.com\nport: 8080"),
		"list":       []byte("- a\n- b\n- c"),
	})
	clientset := fake.NewClientset(secret)
	w := NewSecretWatcher(clientset, "default", "my-secret")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readSecretChanges(t, w)

	// Plain string stays string.
	if data["plain"] != "hello" {
		t.Errorf("expected plain=hello, got %v", data["plain"])
	}

	// Number is parsed (sigs.k8s.io/yaml uses float64 for numbers).
	if !reflect.DeepEqual(data["number"], float64(42)) {
		t.Errorf("expected number=42 (float64), got %v (%T)", data["number"], data["number"])
	}

	// Boolean is parsed.
	if data["boolean"] != true {
		t.Errorf("expected boolean=true, got %v", data["boolean"])
	}

	// Structured YAML becomes a map.
	m, ok := data["structured"].(map[string]any)
	if !ok {
		t.Fatalf("expected structured to be map[string]any, got %T", data["structured"])
	}
	if m["host"] != "example.com" {
		t.Errorf("expected host=example.com, got %v", m["host"])
	}
	if !reflect.DeepEqual(m["port"], float64(8080)) {
		t.Errorf("expected port=8080 (float64), got %v (%T)", m["port"], m["port"])
	}

	// List YAML becomes a slice.
	l, ok := data["list"].([]any)
	if !ok {
		t.Fatalf("expected list to be []any, got %T", data["list"])
	}
	want := []any{"a", "b", "c"}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("expected list=%v, got %v", want, l)
	}
}
