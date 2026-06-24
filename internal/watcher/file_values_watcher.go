package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// FileValuesWatcher polls a filesystem directory for .yaml/.yml files and
// emits their parsed contents whenever a change is detected. It mirrors
// the ConfigMapWatcher interface so it can be used interchangeably.
type FileValuesWatcher struct {
	dir      string
	interval time.Duration
	ch       chan map[string]any

	mu       sync.Mutex
	previous map[string]any
	synced   bool
}

// NewFileValuesWatcher creates a new FileValuesWatcher that polls dir at
// the given interval.
func NewFileValuesWatcher(dir string, interval time.Duration) *FileValuesWatcher {
	return &FileValuesWatcher{
		dir:      dir,
		interval: interval,
		ch:       make(chan map[string]any, 1),
	}
}

// Changes returns the channel on which directory data updates are delivered.
func (w *FileValuesWatcher) Changes() <-chan map[string]any {
	return w.ch
}

// ScanOnce performs a single scan and delivers the result on Changes(), without
// starting the polling loop. Use it to obtain initial directory values when
// watching is disabled, so no polling goroutine is created.
func (w *FileValuesWatcher) ScanOnce() {
	w.scan()
}

// Run starts polling the directory and blocks until ctx is cancelled. On exit it
// closes the Changes() channel so consumers ranging over it terminate cleanly
// (no leaked fan-in goroutine on shutdown).
func (w *FileValuesWatcher) Run(ctx context.Context) error {
	defer close(w.ch)

	// Deliver initial state immediately.
	w.scan()

	slog.Info("watching directory for values", "dir", w.dir, "interval", w.interval)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.scan()
		}
	}
}

func (w *FileValuesWatcher) scan() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		slog.Error("reading values directory", "dir", w.dir, "error", err)
		w.send(nil)

		return
	}

	parsed := make(map[string]any)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()

		// Skip dotfiles (Kubernetes mounts ConfigMaps with ..data, ..version symlinks).
		if strings.HasPrefix(name, ".") {
			continue
		}

		ext := filepath.Ext(name)
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(w.dir, name))
		if err != nil {
			slog.Error("reading values file", "file", name, "error", err)

			continue
		}

		var val any
		unmarshalErr := yaml.Unmarshal(data, &val)
		if unmarshalErr != nil {
			val = string(data) // fallback to raw string on parse error
		}

		key := strings.TrimSuffix(name, ext)
		parsed[key] = val
	}

	w.send(parsed)
}

func (w *FileValuesWatcher) send(data map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// reflect.DeepEqual is deliberate; see ConfigMapWatcher.send for why a
	// content hash is not used on this dedup path.
	if w.synced && reflect.DeepEqual(data, w.previous) {
		return
	}

	w.synced = true
	w.previous = data

	coalescingSend(w.ch, data)
}
