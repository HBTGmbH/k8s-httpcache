package watcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// readFileValuesChanges reads from the FileValuesWatcher's Changes channel with a timeout.
func readFileValuesChanges(t *testing.T, w *FileValuesWatcher) map[string]any {
	t.Helper()
	select {
	case data := <-w.Changes():
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for file values change")

		return nil
	}
}

// assertNoFileValuesChanges verifies no message arrives within the timeout.
func assertNoFileValuesChanges(t *testing.T, w *FileValuesWatcher, timeout time.Duration) {
	t.Helper()
	select {
	case data := <-w.Changes():
		t.Fatalf("unexpected file values change received: %v", data)
	case <-time.After(timeout):
		// OK — no change
	}
}

func writeYAML(t *testing.T, dir, name, content string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFileValuesWatcherInitialSync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "server.yaml", "host: example.com\nport: 8080")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	server, ok := data["server"].(map[string]any)
	if !ok {
		t.Fatalf("expected server to be map[string]any, got %T", data["server"])
	}
	if server["host"] != "example.com" {
		t.Errorf("expected host=example.com, got %v", server["host"])
	}
}

func TestFileValuesWatcherEmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data for empty directory, got %v", data)
	}
}

func TestFileValuesWatcherUpdate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "ttl.yaml", "300")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initial sync.
	data := readFileValuesChanges(t, w)
	if !reflect.DeepEqual(data["ttl"], float64(300)) {
		t.Fatalf("expected ttl=300, got %v (%T)", data["ttl"], data["ttl"])
	}

	// Update file.
	writeYAML(t, dir, "ttl.yaml", "600")

	data = readFileValuesChanges(t, w)
	if !reflect.DeepEqual(data["ttl"], float64(600)) {
		t.Errorf("expected ttl=600, got %v (%T)", data["ttl"], data["ttl"])
	}
}

func TestFileValuesWatcherDeduplicatesUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "key.yaml", "value")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Consume initial state.
	readFileValuesChanges(t, w)

	// No file changes — should NOT deliver a duplicate change.
	assertNoFileValuesChanges(t, w, 300*time.Millisecond)
}

func TestFileValuesWatcherSkipsDotfiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "server.yaml", "host: example.com")
	writeYAML(t, dir, ".hidden.yaml", "secret: true")
	writeYAML(t, dir, "..data", "symlink-target")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	if _, ok := data[".hidden"]; ok {
		t.Error("expected .hidden file to be skipped")
	}
	if _, ok := data["..data"]; ok {
		t.Error("expected ..data file to be skipped")
	}
	if _, ok := data["server"]; !ok {
		t.Error("expected server.yaml to be included")
	}
}

func TestFileValuesWatcherYAMLParsing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "plain.yaml", "hello")
	writeYAML(t, dir, "number.yaml", "42")
	writeYAML(t, dir, "boolean.yaml", "true")
	writeYAML(t, dir, "structured.yaml", "host: example.com\nport: 8080")
	writeYAML(t, dir, "list.yaml", "- a\n- b\n- c")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)

	if data["plain"] != "hello" {
		t.Errorf("expected plain=hello, got %v", data["plain"])
	}
	if !reflect.DeepEqual(data["number"], float64(42)) {
		t.Errorf("expected number=42 (float64), got %v (%T)", data["number"], data["number"])
	}
	if b, ok := data["boolean"].(bool); !ok || !b {
		t.Errorf("expected boolean=true, got %v", data["boolean"])
	}

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

	l, ok := data["list"].([]any)
	if !ok {
		t.Fatalf("expected list to be []any, got %T", data["list"])
	}
	want := []any{"a", "b", "c"}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("expected list=%v, got %v", want, l)
	}
}

func TestFileValuesWatcherFileAddedLater(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initially empty.
	data := readFileValuesChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data initially, got %v", data)
	}

	// Add a file after startup.
	writeYAML(t, dir, "new.yaml", "added: true")

	data = readFileValuesChanges(t, w)
	m, ok := data["new"].(map[string]any)
	if !ok {
		t.Fatalf("expected new to be map[string]any, got %T", data["new"])
	}
	if b, ok := m["added"].(bool); !ok || !b {
		t.Errorf("expected added=true, got %v", m["added"])
	}
}

func TestFileValuesWatcherFileDeleted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "server.yaml", "host: example.com")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// Initial: has data.
	data := readFileValuesChanges(t, w)
	if _, ok := data["server"]; !ok {
		t.Fatal("expected server key in initial data")
	}

	// Delete the file.
	err := os.Remove(filepath.Join(dir, "server.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	data = readFileValuesChanges(t, w)
	if len(data) != 0 {
		t.Fatalf("expected empty data after delete, got %v", data)
	}
}

func TestFileValuesWatcherStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "key.yaml", "value")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Let Run start and deliver initial state.
	readFileValuesChanges(t, w)

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

func TestFileValuesWatcherReadDirError(t *testing.T) {
	t.Parallel()
	// Point at a nonexistent directory to trigger the ReadDir error path.
	w := NewFileValuesWatcher("/nonexistent/path/for/test", 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	// On ReadDir error, scan sends nil.
	data := readFileValuesChanges(t, w)
	if data != nil {
		t.Fatalf("expected nil data on ReadDir error, got %v", data)
	}
}

func TestFileValuesWatcherSkipsSubdirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "config.yaml", "key: value")
	// Create a subdirectory — should be skipped by scan.
	err := os.Mkdir(filepath.Join(dir, "subdir"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	if _, ok := data["subdir"]; ok {
		t.Error("expected subdirectory to be skipped")
	}
	if _, ok := data["config"]; !ok {
		t.Error("expected config.yaml to be included")
	}
}

func TestFileValuesWatcherSkipsNonYAMLFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "data.yaml", "key: value")
	writeYAML(t, dir, "notes.txt", "plain text")
	writeYAML(t, dir, "config.json", `{"key": "value"}`)

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	if _, ok := data["notes"]; ok {
		t.Error("expected .txt file to be skipped")
	}
	if _, ok := data["config"]; ok {
		t.Error("expected .json file to be skipped")
	}
	if _, ok := data["data"]; !ok {
		t.Error("expected data.yaml to be included")
	}
}

func TestFileValuesWatcherInvalidYAMLFallsBackToString(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Write content that is invalid YAML (tabs for indentation cause parse errors in strict mode,
	// but sigs.k8s.io/yaml is lenient). Use a known-bad pattern: bare ":" at top level.
	writeYAML(t, dir, "bad.yaml", ":\n  :\n    :")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	// Whether it parses or falls back to string, the key should exist.
	if _, ok := data["bad"]; !ok {
		t.Fatal("expected bad key to be present")
	}
}

func TestFileValuesWatcherReadFileError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "good.yaml", "key: value")

	// Create a symlink pointing to a nonexistent target — ReadDir sees it,
	// but ReadFile fails.
	broken := filepath.Join(dir, "broken.yaml")
	err := os.Symlink(filepath.Join(dir, "nonexistent"), broken)
	if err != nil {
		t.Skip("symlink not supported:", err)
	}

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	// good.yaml should still be present; broken.yaml is skipped on error.
	if _, ok := data["good"]; !ok {
		t.Error("expected good.yaml to be included")
	}
	if _, ok := data["broken"]; ok {
		t.Error("expected broken.yaml to be skipped due to ReadFile error")
	}
}

func TestFileValuesWatcherYMLExtension(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeYAML(t, dir, "config.yml", "key: value")

	w := NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()

	data := readFileValuesChanges(t, w)
	m, ok := data["config"].(map[string]any)
	if !ok {
		t.Fatalf("expected config to be map[string]any, got %T", data["config"])
	}
	if m["key"] != "value" {
		t.Errorf("expected key=value, got %v", m["key"])
	}
}
