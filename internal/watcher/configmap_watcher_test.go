package watcher

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// readConfigMapChanges reads from the ConfigMapWatcher's Changes channel with a timeout.
func readConfigMapChanges(t *testing.T, w *ConfigMapWatcher) map[string]any {
	t.Helper()
	select {
	case data := <-w.Changes():
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for ConfigMap change")
		return nil
	}
}

// assertNoConfigMapChanges verifies no message arrives within the timeout.
func assertNoConfigMapChanges(t *testing.T, w *ConfigMapWatcher, timeout time.Duration) {
	t.Helper()
	select {
	case data := <-w.Changes():
		t.Fatalf("unexpected ConfigMap change received: %v", data)
	case <-time.After(timeout):
		// OK — no change
	}
}

func makeConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Data: data,
	}
}

func TestConfigMapWatcherInitialSync(t *testing.T) {
	cm := makeConfigMap("my-cm", map[string]string{"key": "value"})
	clientset := fake.NewClientset(cm)
	w := NewConfigMapWatcher(clientset, "default", "my-cm")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readConfigMapChanges(t, w)
	if data["key"] != "value" {
		t.Errorf("expected key=value, got %v", data)
	}
}

func TestConfigMapWatcherMissing(t *testing.T) {
	clientset := fake.NewClientset()
	w := NewConfigMapWatcher(clientset, "default", "missing-cm")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readConfigMapChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data for missing ConfigMap, got %v", data)
	}
}

func TestConfigMapWatcherUpdate(t *testing.T) {
	cm := makeConfigMap("my-cm", map[string]string{"key": "v1"})
	clientset := fake.NewClientset(cm)
	w := NewConfigMapWatcher(clientset, "default", "my-cm")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initial sync.
	waitForWatch()
	data := readConfigMapChanges(t, w)
	if data["key"] != "v1" {
		t.Fatalf("expected v1, got %v", data)
	}

	// Update the ConfigMap.
	cm2, err := clientset.CoreV1().ConfigMaps("default").Get(ctx, "my-cm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting ConfigMap: %v", err)
	}
	cm2.Data["key"] = "v2"
	_, err = clientset.CoreV1().ConfigMaps("default").Update(ctx, cm2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating ConfigMap: %v", err)
	}

	data = readConfigMapChanges(t, w)
	if data["key"] != "v2" {
		t.Errorf("expected v2, got %v", data)
	}
}

func TestConfigMapWatcherDeduplicatesUnchanged(t *testing.T) {
	cm := makeConfigMap("my-cm", map[string]string{"key": "value"})
	clientset := fake.NewClientset(cm)
	w := NewConfigMapWatcher(clientset, "default", "my-cm")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial state.
	waitForWatch()
	readConfigMapChanges(t, w)

	// Update an unrelated field (annotation) — data stays the same.
	cm2, err := clientset.CoreV1().ConfigMaps("default").Get(ctx, "my-cm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting ConfigMap: %v", err)
	}
	cm2.Annotations = map[string]string{"unrelated": "change"}
	_, err = clientset.CoreV1().ConfigMaps("default").Update(ctx, cm2, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating ConfigMap: %v", err)
	}

	// Should NOT deliver a duplicate change.
	assertNoConfigMapChanges(t, w, 500*time.Millisecond)
}

func TestConfigMapWatcherDeleted(t *testing.T) {
	cm := makeConfigMap("my-cm", map[string]string{"key": "value"})
	clientset := fake.NewClientset(cm)
	w := NewConfigMapWatcher(clientset, "default", "my-cm")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initial: has data.
	waitForWatch()
	data := readConfigMapChanges(t, w)
	if data["key"] != "value" {
		t.Fatalf("expected key=value, got %v", data)
	}

	// Delete the ConfigMap.
	err := clientset.CoreV1().ConfigMaps("default").Delete(ctx, "my-cm", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("deleting ConfigMap: %v", err)
	}

	data = readConfigMapChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data after delete, got %v", data)
	}
}

func TestConfigMapWatcherAppearsLate(t *testing.T) {
	clientset := fake.NewClientset()
	w := NewConfigMapWatcher(clientset, "default", "late-cm")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initially no ConfigMap → empty data.
	data := readConfigMapChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data initially, got %v", data)
	}

	// ConfigMap appears after startup.
	cm := makeConfigMap("late-cm", map[string]string{"greeting": "hello"})
	_, err := clientset.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating ConfigMap: %v", err)
	}

	data = readConfigMapChanges(t, w)
	if data["greeting"] != "hello" {
		t.Errorf("expected greeting=hello, got %v", data)
	}
}

func TestConfigMapWatcherStopsOnContextCancel(t *testing.T) {
	cm := makeConfigMap("my-cm", map[string]string{"key": "value"})
	clientset := fake.NewClientset(cm)
	w := NewConfigMapWatcher(clientset, "default", "my-cm")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Let Run start and deliver initial state.
	readConfigMapChanges(t, w)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancel")
	}
}

func TestConfigMapWatcherYAMLParsing(t *testing.T) {
	cm := makeConfigMap("my-cm", map[string]string{
		"plain":      "hello",
		"number":     "42",
		"boolean":    "true",
		"structured": "host: example.com\nport: 8080",
		"list":       "- a\n- b\n- c",
	})
	clientset := fake.NewClientset(cm)
	w := NewConfigMapWatcher(clientset, "default", "my-cm")

	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readConfigMapChanges(t, w)

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
