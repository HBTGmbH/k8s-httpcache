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

	// testHookAfterRead, when non-nil, runs after each successful file read.
	// Tests use it to swap the ..data symlink mid-scan to exercise the torn-
	// snapshot rescan.
	testHookAfterRead func(name string)
}

// k8sDataSymlink is the symlink Kubernetes atomically replaces when a
// ConfigMap/Secret volume is updated; all mounted files resolve through it.
const k8sDataSymlink = "..data"

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
// The delivered map is also retained by the watcher as its dedup baseline and
// compared with [reflect.DeepEqual] on the poller goroutine: consumers must
// treat it as read-only (copy before mutating).
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
	// A ConfigMap volume update atomically replaces the ..data symlink, and
	// every os.ReadFile resolves it independently - a swap landing between two
	// reads of one pass would deliver a torn snapshot mixing old- and new-
	// generation files. Recheck the symlink after each pass and rescan when it
	// moved. Directories without a ..data link (non-Kubernetes) skip the check.
	const maxScanAttempts = 3

	var parsed map[string]any
	haveSnapshot := false
	for attempt := range maxScanAttempts {
		dataBefore, dataErr := os.Readlink(filepath.Join(w.dir, k8sDataSymlink))
		p, ok := w.readFiles()
		if !ok {
			// Transient ReadDir failure (e.g. volume remount). A prior pass of
			// THIS scan already read a complete snapshot (we only looped
			// because the ..data symlink moved) - deliver that instead of
			// discarding it: possibly torn across generations, but strictly
			// better than wiping the values, and the next poll corrects it.
			if haveSnapshot {
				slog.Warn("values directory listing failed mid-rescan, delivering the previous pass's snapshot", "dir", w.dir)

				break
			}
			// No successful pass at all: keep the previously delivered values
			// instead of wiping them for one interval and forcing a spurious
			// empty-values reload. Before the first successful delivery there
			// is nothing to keep - deliver the empty snapshot so startup
			// collection is never left blocking.
			w.mu.Lock()
			synced := w.synced
			w.mu.Unlock()
			if !synced {
				w.send(nil)
			}

			return
		}
		parsed = p
		haveSnapshot = true
		if dataErr != nil {
			break // no ..data symlink - nothing to recheck
		}
		dataAfter, afterErr := os.Readlink(filepath.Join(w.dir, k8sDataSymlink))
		if afterErr == nil && dataAfter == dataBefore {
			break // consistent snapshot
		}
		if attempt == maxScanAttempts-1 {
			// Deliver the last snapshot anyway: possibly torn, but the next
			// poll corrects it, and the initial send must never be skipped.
			slog.Warn("values directory kept changing during scan, delivering possibly inconsistent snapshot", "dir", w.dir)
		}
	}

	w.send(parsed)
}

// readFiles reads all value files in one pass. It returns ok=false when the
// directory listing itself failed; per-file errors only skip that file.
func (w *FileValuesWatcher) readFiles() (map[string]any, bool) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		slog.Error("reading values directory", "dir", w.dir, "error", err)

		return nil, false
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
		if w.testHookAfterRead != nil {
			w.testHookAfterRead(name)
		}

		var val any
		unmarshalErr := yaml.Unmarshal(data, &val)
		if unmarshalErr != nil {
			val = string(data) // fallback to raw string on parse error
		}

		key := strings.TrimSuffix(name, ext)
		parsed[key] = val
	}

	return parsed, true
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
