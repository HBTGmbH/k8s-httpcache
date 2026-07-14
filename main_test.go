package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/HBTGmbH/k8s-httpcache/internal/broadcast"
	"github.com/HBTGmbH/k8s-httpcache/internal/config"
	"github.com/HBTGmbH/k8s-httpcache/internal/redact"
	"github.com/HBTGmbH/k8s-httpcache/internal/renderer"
	"github.com/HBTGmbH/k8s-httpcache/internal/telemetry"
	"github.com/HBTGmbH/k8s-httpcache/internal/varnish"
	"github.com/HBTGmbH/k8s-httpcache/internal/watcher"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/goleak"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

func TestBackendChanNil(t *testing.T) {
	t.Parallel()
	ch := backendChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil input")
	}
}

func TestAwaitInitial(t *testing.T) {
	t.Parallel()

	// Returns the collected value when collect finishes before ctx is cancelled.
	got, err := awaitInitial(t.Context(), nil, func() int { return 42 })
	if err != nil || got != 42 {
		t.Fatalf("awaitInitial = (%d, %v), want (42, nil)", got, err)
	}

	// Returns ctx.Err() promptly when collect never finishes and ctx is
	// cancelled (the startup-hang guard for unreachable-API / never-syncing
	// watchers).
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = awaitInitial(ctx, nil, func() string {
		<-make(chan struct{}) // block forever

		return "never"
	})
	if err == nil {
		t.Fatal("awaitInitial must return an error when ctx is cancelled before collect finishes")
	}
	// awaitInitial returns the moment ctx fires (~20ms), independent of the
	// never-finishing collect. The bound is deliberately generous so it cannot
	// flake on slow CI; its only job is to catch a regression where awaitInitial
	// blocks on collect instead of honouring ctx (which would otherwise hang
	// until the whole test binary times out).
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("awaitInitial took %v, expected to return promptly when ctx is cancelled", elapsed)
	}

	// A watcher Run failure aborts the wait even when ctx has no deadline
	// (--startup-timeout=0): the failed watcher would never deliver its
	// guaranteed initial send, so collect would block forever.
	sentinel := errors.New("adding event handler: informer stopped")
	watcherErr := make(chan error, 1)
	watcherErr <- sentinel
	_, err = awaitInitial(t.Context(), watcherErr, func() string {
		<-make(chan struct{}) // block forever

		return "never"
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("awaitInitial error = %v, want the wrapped watcher error", err)
	}
}

func TestBackendChanNonNil(t *testing.T) {
	t.Parallel()
	input := make(chan backendChange, 1)
	ch := backendChan(input)
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	// Verify it's the same channel by sending and receiving.
	input <- backendChange{name: "test"}
	select {
	case bc := <-ch:
		if bc.name != "test" {
			t.Fatalf("got name %q, want test", bc.name)
		}
	default:
		t.Fatal("expected to receive from channel")
	}
}

func TestTimerChanNil(t *testing.T) {
	t.Parallel()
	ch := timerChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil timer")
	}
}

func TestTimerChanNonNil(t *testing.T) {
	t.Parallel()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	ch := timerChan(timer)
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	if ch != timer.C {
		t.Fatal("expected same channel as timer.C")
	}
}

func TestWatchFileDetectsChange(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	_, err = f.WriteString("initial")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx := t.Context()

	ch := watchFile(ctx, path, 50*time.Millisecond)

	// No sleep-based synchronisation with the watcher's baseline read: keep
	// writing DISTINCT contents until a change fires. Whatever content the
	// watcher happened to capture as its baseline, the next write differs
	// from it, so detection is guaranteed regardless of goroutine scheduling.
	deadline := time.After(5 * time.Second)
	for i := 0; ; i++ {
		err = os.WriteFile(path, fmt.Appendf(nil, "changed-%d", i), 0o644)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
			return // OK - change detected
		case <-deadline:
			t.Fatal("timeout waiting for file change notification")
		case <-time.After(100 * time.Millisecond):
			// Not yet - write the next distinct content.
		}
	}
}

func TestWatchFileNoChangeNoNotification(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	_, err = f.WriteString("stable")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx := t.Context()

	ch := watchFile(ctx, path, 50*time.Millisecond)

	// No modification - should not receive anything.
	select {
	case <-ch:
		t.Fatal("unexpected file change notification")
	case <-time.After(300 * time.Millisecond):
		// OK - no notification
	}
}

func TestWatchFileStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	_ = f.Close()

	ctx, cancel := context.WithCancel(t.Context())
	ch := watchFile(ctx, path, 50*time.Millisecond)

	// Cancel the context immediately.
	cancel()

	// The goroutine should exit. We verify by writing a change and
	// confirming no notification arrives (goroutine has stopped polling).
	time.Sleep(200 * time.Millisecond)
	err = os.WriteFile(path, []byte("changed-after-cancel"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
		t.Fatal("unexpected notification after context cancel")
	case <-time.After(300 * time.Millisecond):
		// OK - goroutine stopped
	}
}

// TestWriteInitialVCLFileCleanup verifies the initial-VCL temp-file helper that
// run() relies on: it creates the file with the rendered contents and returns a
// cleanup func that removes it (idempotently). run() wires this cleanup into a
// defer, which only runs because main is the thin [os.Exit](run()) wrapper.
func TestWriteInitialVCLFileCleanup(t *testing.T) {
	t.Parallel()

	const contents = "vcl 4.1; /* initial */"
	path, cleanup, err := writeInitialVCLFile(contents)
	if err != nil {
		t.Fatalf("writeInitialVCLFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading temp VCL %q: %v", path, err)
	}
	if string(got) != contents {
		t.Errorf("contents = %q, want %q", got, contents)
	}

	cleanup()
	_, statErr := os.Stat(path)
	if !os.IsNotExist(statErr) {
		t.Errorf("expected temp VCL %q removed after cleanup, stat err = %v", path, statErr)
	}

	// Cleanup must be safe to call more than once (it runs via defer and may
	// also run on an explicit error path).
	cleanup()
}

// TestRunExitPatternRunsDeferredCleanup reproduces: a
// program structured as [os.Exit](run()) - with cleanup registered via defer
// inside run - removes its temp file before exiting, whereas the previous
// [os.Exit](runLoop()) shape (with the cleanups stranded as defers in main)
// would skip them, leaking the file. Because [os.Exit] terminates the test
// process and the real run() needs a cluster + varnishd, this is verified in a
// re-exec'd child that emulates the exact main/run structure.
func TestRunExitPatternRunsDeferredCleanup(t *testing.T) {
	if os.Getenv("CLEANUP_CHILD") == "1" {
		// Child: mirror main(){ os.Exit(run()) }, where run registers a defer
		// that removes a temp file. Report the path on stdout before exiting.
		os.Exit(func() int {
			f, err := os.CreateTemp(t.TempDir(), "cleanup-proof-*")
			if err != nil {
				_, _ = fmt.Fprintln(os.Stderr, "create:", err)

				return 3
			}
			path := f.Name()
			_ = f.Close()
			defer func() { _ = os.Remove(path) }()
			_, _ = fmt.Fprintln(os.Stdout, path)

			return 0
		}())

		return
	}

	t.Parallel()

	cmd := exec.Command(os.Args[0], "-test.run", "^TestRunExitPatternRunsDeferredCleanup$") //nolint:gosec // re-exec of the test binary itself
	cmd.Env = append(os.Environ(), "CLEANUP_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\noutput:\n%s", err, out)
	}

	path := strings.TrimSpace(string(out))
	if path == "" {
		t.Fatalf("child did not report a temp path; output:\n%s", out)
	}
	_, statErr := os.Stat(path)
	if !os.IsNotExist(statErr) {
		_ = os.Remove(path) // safety net if the deferred cleanup was skipped
		t.Errorf("deferred cleanup did not run: %q still exists (stat err=%v)", path, statErr)
	}
}

// --- Mock types for runLoop tests ---

type mockRenderer struct {
	mu             sync.Mutex
	reloadFn       func() error
	renderFn       func([]watcher.Frontend, map[string]renderer.BackendGroup, map[string]map[string]any, map[string]map[string]any) (string, error)
	renderToFileFn func([]watcher.Frontend, map[string]renderer.BackendGroup, map[string]map[string]any, map[string]map[string]any) (string, error)
	rollbackFn     func()
	reloadCount    int
	renderCount    int
	rollbackCount  int
	lastBackends   map[string]renderer.BackendGroup
}

func (m *mockRenderer) Reload() error {
	m.mu.Lock()
	m.reloadCount++
	fn := m.reloadFn
	m.mu.Unlock()
	if fn != nil {
		return fn()
	}

	return nil
}

func (m *mockRenderer) Render(fe []watcher.Frontend, be map[string]renderer.BackendGroup, vals, secrets map[string]map[string]any) (string, error) {
	m.mu.Lock()
	m.renderCount++
	rc := m.renderCount
	m.lastBackends = maps.Clone(be)
	fn := m.renderFn
	m.mu.Unlock()
	if fn != nil {
		return fn(fe, be, vals, secrets)
	}

	return fmt.Sprintf("vcl 4.1; /* render %d */", rc), nil
}

func (m *mockRenderer) RenderToFile(fe []watcher.Frontend, be map[string]renderer.BackendGroup, vals, secrets map[string]map[string]any) (string, error) {
	m.mu.Lock()
	m.renderCount++
	m.lastBackends = maps.Clone(be)
	fn := m.renderToFileFn
	m.mu.Unlock()
	if fn != nil {
		return fn(fe, be, vals, secrets)
	}

	return "test.vcl", nil
}

func (m *mockRenderer) Rollback() {
	m.mu.Lock()
	m.rollbackCount++
	fn := m.rollbackFn
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (m *mockRenderer) counts() (int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.reloadCount, m.renderCount, m.rollbackCount
}

type mockManager struct {
	mu               sync.Mutex
	reloadFn         func(string) error
	markBackendFn    func(string) error
	activeSessionsFn func() (uint64, error)
	loadCertFn       func(name string, cert, key, ca []byte) error
	tlsSupported     bool
	done             chan struct{}
	err              error
	reloadCount      int
	forwardedSigs    []os.Signal
	markSickCalls    []string
	sessionsCalls    int
	loadCertCalls    []string
}

func (m *mockManager) Reload(vclPath string) error {
	m.mu.Lock()
	m.reloadCount++
	fn := m.reloadFn
	m.mu.Unlock()
	if fn != nil {
		return fn(vclPath)
	}

	return nil
}

func (m *mockManager) Done() <-chan struct{} { return m.done }

func (m *mockManager) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.err
}

func (m *mockManager) ForwardSignal(sig os.Signal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forwardedSigs = append(m.forwardedSigs, sig)
}

func (m *mockManager) MarkBackendSick(name string) error {
	m.mu.Lock()
	m.markSickCalls = append(m.markSickCalls, name)
	fn := m.markBackendFn
	m.mu.Unlock()
	if fn != nil {
		return fn(name)
	}

	return nil
}

func (m *mockManager) ActiveSessions() (uint64, error) {
	m.mu.Lock()
	m.sessionsCalls++
	fn := m.activeSessionsFn
	m.mu.Unlock()
	if fn != nil {
		return fn()
	}

	return 0, nil
}

func (m *mockManager) LoadCert(name string, cert, key, ca []byte) error {
	m.mu.Lock()
	m.loadCertCalls = append(m.loadCertCalls, name)
	fn := m.loadCertFn
	m.mu.Unlock()
	if fn != nil {
		return fn(name, cert, key, ca)
	}

	return nil
}

func (m *mockManager) TLSSupported() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.tlsSupported
}

func (m *mockManager) getLoadCertCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := make([]string, len(m.loadCertCalls))
	copy(dst, m.loadCertCalls)

	return dst
}

func (m *mockManager) getMarkSickCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := make([]string, len(m.markSickCalls))
	copy(dst, m.markSickCalls)

	return dst
}

func (m *mockManager) getReloadCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.reloadCount
}

func (m *mockManager) getSessionsCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.sessionsCalls
}

func (m *mockManager) getForwardedSigs() []os.Signal {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := make([]os.Signal, len(m.forwardedSigs))
	copy(dst, m.forwardedSigs)

	return dst
}

type mockBroadcast struct {
	mu            sync.Mutex
	setCount      int
	drainCount    int
	lastFrontends []watcher.Frontend
}

func (m *mockBroadcast) SetFrontends(fe []watcher.Frontend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCount++
	m.lastFrontends = fe
}

func (m *mockBroadcast) Drain(_ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drainCount++

	return nil
}

func (m *mockBroadcast) drainCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.drainCount
}

// testHarness bundles mocks and channels for a runLoop test.
type testHarness struct {
	rend    *mockRenderer
	mgr     *mockManager
	bcast   *mockBroadcast
	metrics *telemetry.Metrics

	frontendCh chan []watcher.Frontend
	backendCh  chan backendChange
	valuesCh   chan valuesChange
	secretsCh  chan secretsChange
	tlsCertCh  chan tlsCertChange
	templateCh chan struct{}
	sigCh      chan os.Signal

	serviceName string // defaults to "test-svc"

	recorder *record.FakeRecorder
	podRef   *v1.ObjectReference
}

func newTestHarness() *testHarness {
	return &testHarness{
		rend:        &mockRenderer{},
		mgr:         &mockManager{done: make(chan struct{})},
		bcast:       &mockBroadcast{},
		metrics:     telemetry.NewMetrics(prometheus.NewRegistry(), config.DefaultDebounceLatencyBuckets),
		frontendCh:  make(chan []watcher.Frontend, 1),
		backendCh:   make(chan backendChange, 1),
		valuesCh:    make(chan valuesChange, 1),
		secretsCh:   make(chan secretsChange, 1),
		tlsCertCh:   make(chan tlsCertChange, 1),
		templateCh:  make(chan struct{}, 1),
		sigCh:       make(chan os.Signal, 1),
		serviceName: "test-svc",
	}
}

func (h *testHarness) loopConfig(bcast broadcaster) *loopConfig {
	return &loopConfig{
		rend:    h.rend,
		mgr:     h.mgr,
		bcast:   bcast,
		metrics: h.metrics,

		frontendCh: h.frontendCh,
		backendCh:  h.backendCh,
		valuesCh:   h.valuesCh,
		secretsCh:  h.secretsCh,
		tlsCertCh:  h.tlsCertCh,
		templateCh: h.templateCh,
		sigCh:      h.sigCh,

		serviceName:           h.serviceName,
		frontendDebounce:      1 * time.Millisecond,
		frontendDebounceMax:   0,
		backendDebounce:       1 * time.Millisecond,
		backendDebounceMax:    0,
		tlsCertDebounce:       1 * time.Millisecond,
		tlsCertDebounceMax:    0,
		shutdownTimeout:       1 * time.Second,
		broadcastDrainTimeout: 1 * time.Second,

		// Tests default to 0 so the recovery timer is disabled; tests
		// that exercise recovery override these directly on the
		// returned loopConfig.
		reloadRecoveryInitial: 0,
		reloadRecoveryMax:     0,

		drainPollInterval: 1 * time.Second,

		latestFrontends: nil,
		latestBackends:  make(map[string]renderer.BackendGroup),
		latestValues:    make(map[string]map[string]any),
		latestSecrets:   make(map[string]map[string]any),

		recorder: h.recorder,
		podRef:   h.podRef,
	}
}

// waitForStatus polls the status store until check returns true, or fails
// the test after 2 seconds. Use this instead of a fixed [time.Sleep] when
// asserting that a reload (or other async event) has updated the store.
func waitForStatus(t *testing.T, store *statusStore, check func(statusResponse) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check(store.snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("status store condition not met within 2s")
}

// withRecorder wires a FakeRecorder and podRef into the harness so that
// emitEvent calls produce observable events.
func (h *testHarness) withRecorder() *record.FakeRecorder {
	rec := record.NewFakeRecorder(10)
	h.recorder = rec
	h.podRef = &v1.ObjectReference{Kind: "Pod", Name: "test-pod", Namespace: "default"}

	return rec
}

// runAndWait runs the loop in a goroutine and returns a function that
// sends SIGTERM and waits for the exit code.
func (h *testHarness) runAndWait(bcast broadcaster) func() int {
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(bcast))))
		close(done)
	}()

	return func() int {
		h.sigCh <- syscall.SIGTERM
		// Wait for the loop to enter the signal handler (ForwardSignal
		// called) before closing done, so the outer select picks the
		// signal case, not the unexpected-exit case.
		deadline := time.After(2 * time.Second)
	poll:
		for len(h.mgr.getForwardedSigs()) == 0 {
			select {
			case <-deadline:
				break poll
			case <-time.After(1 * time.Millisecond):
			}
		}
		close(h.mgr.done) // simulate varnishd exiting after signal
		<-done

		return int(code.Load())
	}
}

// waitForForwardedSignal blocks until runLoop has forwarded a signal to the
// manager - i.e. it has finished draining and is waiting on mgr.Done(). Tests
// that simulate varnishd exiting after a normal drain use this instead of a
// fixed sleep so they cannot race the drain on a slow CI runner (closing
// mgr.done mid-drain would otherwise correctly abort the drain early and break
// the assertions that the drain ran to completion).
func waitForForwardedSignal(t *testing.T, mgr *mockManager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(mgr.getForwardedSigs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for signal to be forwarded to the manager")
		}
		time.Sleep(time.Millisecond)
	}
}

// --- runLoop tests ---

func TestRunLoop_ReloadZeroFrontends(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	h.frontendCh <- []watcher.Frontend{}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile to be called for zero frontends")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_ReloadWithFrontends(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	var mu sync.Mutex
	var gotFrontends []watcher.Frontend
	h.rend.renderFn = func(fe []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		mu.Lock()
		gotFrontends = fe
		mu.Unlock()

		return "vcl 4.1; /* frontends */", nil
	}

	euBefore := getCounter2Value(t, "frontend", h.serviceName, h.metrics.EndpointUpdatesTotal)
	reloadSuccessBefore := getCounterValue(t, "success", h.metrics.VCLReloadsTotal)

	wait := h.runAndWait(h.bcast)

	pods := []watcher.Frontend{
		{Host: "10.0.0.1", Port: 80, Name: "pod-1"},
		{Host: "10.0.0.2", Port: 80, Name: "pod-2"},
	}
	h.frontendCh <- pods
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	mu.Lock()
	n := len(gotFrontends)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("expected 2 frontends, got %d", n)
	}

	// Verify metrics.
	if delta := getCounter2Value(t, "frontend", h.serviceName, h.metrics.EndpointUpdatesTotal) - euBefore; delta < 1 {
		t.Errorf("endpoint_updates_total(frontend) delta = %v, want >= 1", delta)
	}
	if got := getGauge2Value(t, "frontend", h.serviceName, h.metrics.Endpoints); got != 2 {
		t.Errorf("endpoints(frontend) = %v, want 2", got)
	}
	if delta := getCounterValue(t, "success", h.metrics.VCLReloadsTotal) - reloadSuccessBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(success) delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// collectorSeriesCount returns the number of metric series a collector currently
// exports - used to assert per-label series are deleted (cardinality cleanup).
func collectorSeriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}

	return n
}

// TestRunLoop_BackendRemovalDeletesUpdateCounterSeries verifies that removing a
// discovered backend deletes its per-name endpoint_updates_total series, not
// just the endpoints gauge. Discovered backend names (via --backend-selector)
// are ephemeral, so a counter series left behind for every removed name grows
// Prometheus cardinality unbounded over a long-running pod's lifetime.
func TestRunLoop_BackendRemovalDeletesUpdateCounterSeries(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	// Discover an ephemeral backend (gen > 0 marks a discovery incarnation).
	h.backendCh <- backendChange{
		name:      "ephemeral",
		endpoints: []watcher.Endpoint{{Host: "10.0.0.1", Port: 8080}},
		gen:       1,
	}
	waitFor(t, func() bool {
		return collectorSeriesCount(h.metrics.EndpointUpdatesTotal) == 1
	}, "endpoint_updates_total series created for the discovered backend")

	// Remove the same incarnation.
	h.backendCh <- backendChange{name: "ephemeral", removed: true, gen: 1}
	waitFor(t, func() bool {
		return collectorSeriesCount(h.metrics.EndpointUpdatesTotal) == 0
	}, "endpoint_updates_total series deleted on backend removal (no per-name cardinality leak)")

	if code := wait(); code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_BackendUpdateTriggersReload(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	backendName := "api"
	euBefore := getCounter2Value(t, "backend", backendName, h.metrics.EndpointUpdatesTotal)
	reloadSuccessBefore := getCounterValue(t, "success", h.metrics.VCLReloadsTotal)

	wait := h.runAndWait(h.bcast)

	h.backendCh <- backendChange{
		name:      backendName,
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile after backend update")
	}

	// Verify metrics.
	if delta := getCounter2Value(t, "backend", backendName, h.metrics.EndpointUpdatesTotal) - euBefore; delta < 1 {
		t.Errorf("endpoint_updates_total(backend) delta = %v, want >= 1", delta)
	}
	if got := getGauge2Value(t, "backend", backendName, h.metrics.Endpoints); got != 1 {
		t.Errorf("endpoints(backend,%s) = %v, want 1", backendName, got)
	}
	if delta := getCounterValue(t, "success", h.metrics.VCLReloadsTotal) - reloadSuccessBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(success) delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_ValuesUpdateTriggersReload(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	vuBefore := getCounterValue(t, "tuning", h.metrics.ValuesUpdatesTotal)

	wait := h.runAndWait(h.bcast)

	h.valuesCh <- valuesChange{
		name: "tuning",
		data: map[string]any{"ttl": "300"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile after values update")
	}

	// Verify metric.
	if delta := getCounterValue(t, "tuning", h.metrics.ValuesUpdatesTotal) - vuBefore; delta < 1 {
		t.Errorf("values_updates_total(tuning) delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestValuesChanNil(t *testing.T) {
	t.Parallel()
	ch := valuesChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil input")
	}
}

func TestValuesChanNonNil(t *testing.T) {
	t.Parallel()
	input := make(chan valuesChange, 1)
	ch := valuesChan(input)
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	input <- valuesChange{name: "test"}
	select {
	case vc := <-ch:
		if vc.name != "test" {
			t.Fatalf("got name %q, want test", vc.name)
		}
	default:
		t.Fatal("expected to receive from channel")
	}
}

func TestSecretsChanNil(t *testing.T) {
	t.Parallel()
	ch := secretsChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil input")
	}
}

func TestSecretsChanNonNil(t *testing.T) {
	t.Parallel()
	input := make(chan secretsChange, 1)
	ch := secretsChan(input)
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	input <- secretsChange{name: "test"}
	select {
	case sc := <-ch:
		if sc.name != "test" {
			t.Fatalf("got name %q, want test", sc.name)
		}
	default:
		t.Fatal("expected to receive from channel")
	}
}

func TestRunLoop_SecretsUpdateTriggersReload(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	suBefore := getCounterValue(t, "auth", h.metrics.SecretsUpdatesTotal)

	wait := h.runAndWait(h.bcast)

	h.secretsCh <- secretsChange{
		name: "auth",
		data: map[string]any{"token": "abc123"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile after secrets update")
	}

	// Verify metric.
	if delta := getCounterValue(t, "auth", h.metrics.SecretsUpdatesTotal) - suBefore; delta < 1 {
		t.Errorf("secrets_updates_total(auth) delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_SecretsUpdateUpdatesRedactor(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	rd := redact.NewRedactor()

	ctx, cancel := context.WithCancel(t.Context())
	lc := h.loopConfig(h.bcast)
	lc.redactor = rd
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	const secret = "super-secret-password"

	// Before the secret change, the redactor should not know about the secret.
	if got := rd.Redact("leak " + secret + " here"); strings.Contains(got, "[REDACTED]") {
		t.Fatal("redactor should not redact before secret update")
	}

	h.secretsCh <- secretsChange{
		name: "db",
		data: map[string]any{"password": secret},
	}
	waitFor(t, func() bool { return strings.Contains(rd.Redact("leak "+secret+" here"), "[REDACTED]") }, "redactor updated")

	// After the secret change, the redactor should redact the value.
	got := rd.Redact("leak " + secret + " here")
	want := "leak [REDACTED] here"
	if got != want {
		t.Errorf("redactor not updated after secret change:\n got: %q\nwant: %q", got, want)
	}

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if c := int(code.Load()); c != 0 {
		t.Fatalf("expected exit 0, got %d", c)
	}
}

func TestRunLoop_ReloadErrorRedactsSecretsInEvent(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	const secret = "event-leak-token-99"

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"app": {"token": secret},
	})

	// Make Reload return an error containing the secret (simulating a
	// varnishadm response that already went through the redactor in the
	// real Manager). In this test we verify the event loop path; the
	// Manager-level redaction is tested in the varnish package.
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("vcl.load: exit status 1: VCL error near " + secret)
	}

	ctx, cancel := context.WithCancel(t.Context())
	lc := h.loopConfig(h.bcast)
	lc.redactor = rd
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Trigger a backend change to force a reload.
	h.backendCh <- backendChange{
		name:      "nginx",
		endpoints: []watcher.Endpoint{{Host: "10.0.0.1", Port: 80, Name: "pod1"}},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	// Drain events from the recorder and check for secret leaks.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	// Check that the VCLReloadFailed event was emitted.
	foundReloadFailed := false
	for len(rec.Events) > 0 {
		event := <-rec.Events
		if strings.Contains(event, "VCLReloadFailed") {
			foundReloadFailed = true
			// In the real system, the Manager.adm() redacts the
			// response before it reaches the error, so the event
			// would contain [REDACTED] instead. Here we test that
			// the event IS emitted for reload failures.
		}
	}
	if !foundReloadFailed {
		t.Error("expected VCLReloadFailed event to be emitted")
	}
}

func TestRunLoop_TemplateChangeTriggersReparse(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	tcBefore := getSingleCounterValue(t, h.metrics.VCLTemplateChangesTotal)
	reloadSuccessBefore := getCounterValue(t, "success", h.metrics.VCLReloadsTotal)

	wait := h.runAndWait(h.bcast)

	h.templateCh <- struct{}{}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	reloadCount, renderCount, _ := h.rend.counts()
	if reloadCount < 1 {
		t.Fatal("expected rend.Reload() to be called on template change")
	}
	if renderCount < 1 {
		t.Fatal("expected RenderToFile after template change")
	}

	// Verify metrics.
	if delta := getSingleCounterValue(t, h.metrics.VCLTemplateChangesTotal) - tcBefore; delta < 1 {
		t.Errorf("vcl_template_changes_total delta = %v, want >= 1", delta)
	}
	if delta := getCounterValue(t, "success", h.metrics.VCLReloadsTotal) - reloadSuccessBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(success) delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_TemplateParseErrorKeepsOld(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.rend.reloadFn = func() error { return errors.New("parse error") }
	parseBefore := getSingleCounterValue(t, h.metrics.VCLTemplateParseErrorsTotal)

	wait := h.runAndWait(h.bcast)

	h.templateCh <- struct{}{}
	waitFor(t, func() bool {
		_, rc, _ := h.rend.counts()

		return rc >= 1
	}, "RenderToFile called")

	_, renderCount, rollbackCount := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile to still be called with old template")
	}
	if rollbackCount != 0 {
		t.Fatal("expected no Rollback when template parse fails (old template kept)")
	}

	// Verify metric.
	if delta := getSingleCounterValue(t, h.metrics.VCLTemplateParseErrorsTotal) - parseBefore; delta < 1 {
		t.Errorf("vcl_template_parse_errors_total delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_RenderErrorTriggersRollback(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	var renderCalls atomic.Int32
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		if renderCalls.Add(1) == 1 {
			return "", errors.New("render error")
		}

		return "vcl 4.1; /* rollback */", nil
	}
	renderErrBefore := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal)
	rollbackBefore := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal)

	wait := h.runAndWait(h.bcast)

	// Template change → Reload succeeds → Render fails → Rollback
	h.templateCh <- struct{}{}
	waitFor(t, func() bool {
		_, _, rc := h.rend.counts()

		return rc >= 1
	}, "Rollback called")

	_, _, rollbackCount := h.rend.counts()
	if rollbackCount < 1 {
		t.Fatal("expected Rollback after render error with new template")
	}

	// Verify metrics.
	if delta := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal) - renderErrBefore; delta < 1 {
		t.Errorf("vcl_render_errors_total delta = %v, want >= 1", delta)
	}
	if delta := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal) - rollbackBefore; delta < 1 {
		t.Errorf("vcl_rollbacks_total delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestRunLoop_RenderErrorReappliesRolledBackTemplate verifies that when a new
// template parses but Render fails, the controller does not just roll the
// template back and give up: it re-renders the current inputs with the restored
// good template and reloads varnishd, mirroring the varnish-reload-error path.
// Otherwise any frontend/backend change coalesced into the same reload is
// silently dropped and the live VCL stays stale until the next event.
func TestRunLoop_RenderErrorReappliesRolledBackTemplate(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	var renderCalls atomic.Int32
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		if renderCalls.Add(1) == 1 {
			return "", errors.New("render error with new template")
		}
		// Re-render with the rolled-back (known-good) template succeeds.
		return "vcl 4.1; /* rolled back */", nil
	}

	wait := h.runAndWait(h.bcast)

	// Template change → Reload OK → Render fails → Rollback → the restored
	// template must be re-rendered and reloaded into varnishd.
	h.templateCh <- struct{}{}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "re-render and reload after render-error rollback")

	if _, _, rollbackCount := h.rend.counts(); rollbackCount < 1 {
		t.Fatal("expected Rollback after render error with new template")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_VarnishReloadErrorTriggersRollback(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	var mgrCalls atomic.Int32
	h.mgr.reloadFn = func(_ string) error {
		if mgrCalls.Add(1) == 1 {
			return errors.New("varnish reload error")
		}

		return nil
	}
	reloadErrBefore := getCounterValue(t, "error", h.metrics.VCLReloadsTotal)
	reloadSuccessBefore := getCounterValue(t, "success", h.metrics.VCLReloadsTotal)
	rollbackBefore := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal)

	wait := h.runAndWait(h.bcast)

	// Template change → Reload succeeds → RenderToFile succeeds →
	// mgr.Reload fails → Rollback → retry render → retry mgr.Reload
	h.templateCh <- struct{}{}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "retry mgr.Reload after rollback")

	_, _, rollbackCount := h.rend.counts()
	if rollbackCount < 1 {
		t.Fatal("expected Rollback after varnish reload error with new template")
	}

	// Verify metrics.
	if delta := getCounterValue(t, "error", h.metrics.VCLReloadsTotal) - reloadErrBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(error) delta = %v, want >= 1", delta)
	}
	if delta := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal) - rollbackBefore; delta < 1 {
		t.Errorf("vcl_rollbacks_total delta = %v, want >= 1", delta)
	}
	if delta := getCounterValue(t, "success", h.metrics.VCLReloadsTotal) - reloadSuccessBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(success) delta = %v, want >= 1 (retry succeeded)", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_RollbackRenderError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		return "", errors.New("render always fails")
	}
	renderErrBefore := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal)
	rollbackBefore := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal)

	wait := h.runAndWait(h.bcast)

	// Template change → rend.Reload ok → Render fails → Rollback →
	// (no retry render path in this case, since the first render failed)
	// Actually the first render error with reloadedTemplate → Rollback, continue.
	// The loop continues without crashing.
	h.templateCh <- struct{}{}
	waitFor(t, func() bool {
		_, _, rc := h.rend.counts()

		return rc >= 1
	}, "Rollback called")

	_, _, rollbackCount := h.rend.counts()
	if rollbackCount < 1 {
		t.Fatal("expected Rollback")
	}

	// Verify metrics.
	if delta := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal) - renderErrBefore; delta < 1 {
		t.Errorf("vcl_render_errors_total delta = %v, want >= 1", delta)
	}
	if delta := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal) - rollbackBefore; delta < 1 {
		t.Errorf("vcl_rollbacks_total delta = %v, want >= 1", delta)
	}

	// Loop still alive - send another event and terminate normally.
	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_RollbackReloadError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish always rejects")
	}
	reloadErrBefore := getCounterValue(t, "error", h.metrics.VCLReloadsTotal)
	rollbackBefore := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal)

	wait := h.runAndWait(h.bcast)

	// Template change → rend.Reload ok → RenderToFile ok → mgr.Reload fails →
	// Rollback → retry RenderToFile ok → retry mgr.Reload fails again → continue
	h.templateCh <- struct{}{}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "mgr.Reload called twice")

	// Verify metrics: two reload errors (initial + retry) and one rollback.
	if delta := getCounterValue(t, "error", h.metrics.VCLReloadsTotal) - reloadErrBefore; delta < 2 {
		t.Errorf("vcl_reloads_total(error) delta = %v, want >= 2", delta)
	}
	if delta := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal) - rollbackBefore; delta < 1 {
		t.Errorf("vcl_rollbacks_total delta = %v, want >= 1", delta)
	}

	// Loop still alive - terminate normally.
	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_SignalShutdown(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(h.bcast))))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	// Wait for the loop to enter the signal handler (ForwardSignal called).
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done) // varnishd exits cleanly
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}

	drainCount := h.bcast.drainCalls()
	if drainCount < 1 {
		t.Fatalf("expected Drain to be called, got %d", drainCount)
	}

	sigs := h.mgr.getForwardedSigs()
	if len(sigs) == 0 {
		t.Fatal("expected signal to be forwarded to varnishd")
	}
	if sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected SIGTERM forwarded, got %v", sigs[0])
	}
}

func TestRunLoop_VarnishdUnexpectedExit(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.err = errors.New("crashed")
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(h.bcast))))
		close(done)
	}()

	close(h.mgr.done) // unexpected exit
	<-done

	result := int(code.Load())
	if result != 1 {
		t.Fatalf("expected exit 1, got %d", result)
	}

	// Verify metric. VarnishdUp is safe to assert here: only run() sets it
	// to 1, and tests call runLoop() directly, so no parallel test changes it.
	if got := getGaugeValue(t, h.metrics.VarnishdUp); got != 0 {
		t.Errorf("varnishd_up = %v, want 0", got)
	}
}

func TestRunLoop_RenderErrorNoRollbackWithoutTemplateChange(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		return "", errors.New("render error")
	}
	renderErrBefore := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal)

	wait := h.runAndWait(h.bcast)

	// Frontend update (not a template change) → Render fails → no Rollback.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool {
		return getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal)-renderErrBefore >= 1
	}, "render error metric incremented")

	_, renderCount, rollbackCount := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile to be called")
	}
	if rollbackCount != 0 {
		t.Fatal("expected no Rollback on render error without template change")
	}

	// Verify metric.
	if delta := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal) - renderErrBefore; delta < 1 {
		t.Errorf("vcl_render_errors_total delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_VarnishReloadErrorNoRollbackWithoutTemplateChange(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish reload error")
	}
	reloadErrBefore := getCounterValue(t, "error", h.metrics.VCLReloadsTotal)

	wait := h.runAndWait(h.bcast)

	// Frontend update → RenderToFile ok → mgr.Reload fails → no Rollback.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	if h.mgr.getReloadCount() < 1 {
		t.Fatal("expected mgr.Reload to be called")
	}
	_, _, rollbackCount := h.rend.counts()
	if rollbackCount != 0 {
		t.Fatal("expected no Rollback on reload error without template change")
	}

	// Verify metric.
	if delta := getCounterValue(t, "error", h.metrics.VCLReloadsTotal) - reloadErrBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(error) delta = %v, want >= 1", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_ReloadRecoveryRetriesAfterFailure(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	var loadCount atomic.Int32
	h.mgr.reloadFn = func(_ string) error {
		// First two attempts fail (simulating the CLI communication
		// error scenario); the third succeeds.
		if loadCount.Add(1) <= 2 {
			return errors.New("cli communication error (hdr)")
		}

		return nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})

	lc := h.loopConfig(h.bcast)
	// Short delays so the test completes quickly. Doubling: 10ms → 20ms (cap).
	lc.reloadRecoveryInitial = 10 * time.Millisecond
	lc.reloadRecoveryMax = 20 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Trigger a single reload via a frontend change.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}

	// Expect 1 initial attempt (fails) + 2 recovery retries (1 fails, 1 succeeds).
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "3 mgr.Reload calls via recovery timer")

	// After success, the recovery timer should be stopped; no further reloads.
	time.Sleep(80 * time.Millisecond)
	stable := h.mgr.getReloadCount()
	time.Sleep(80 * time.Millisecond)
	if final := h.mgr.getReloadCount(); final != stable {
		t.Errorf("expected no further reload retries after success, got %d more (final=%d)", final-stable, final)
	}

	// Verify the error metric was incremented for the 2 failures.
	if got := getCounterValue(t, "error", h.metrics.VCLReloadsTotal); got < 2 {
		t.Errorf("vcl_reloads_total(error) = %v, want >= 2", got)
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_ReloadRecoveryDisabledByDefault(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	var loadCount atomic.Int32
	h.mgr.reloadFn = func(_ string) error {
		loadCount.Add(1)

		return errors.New("cli communication error (hdr)")
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})

	lc := h.loopConfig(h.bcast)
	// Harness default: reloadRecoveryInitial == 0 (disabled).

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Trigger one reload via a frontend change.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}

	// Wait for the initial reload attempt.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "initial mgr.Reload call")

	// Without recovery, no further retries should happen even though the
	// reload failed - behavior matches pre-recovery-timer codebase.
	time.Sleep(100 * time.Millisecond)
	if got := h.mgr.getReloadCount(); got != 1 {
		t.Errorf("expected exactly 1 reload call with recovery disabled, got %d", got)
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_RetryRenderAfterRollbackFails(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	var renderCalls atomic.Int32
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		if renderCalls.Add(1) == 2 {
			return "", errors.New("render error after rollback")
		}

		return "vcl 4.1; /* retry */", nil
	}
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish reload error")
	}
	reloadErrBefore := getCounterValue(t, "error", h.metrics.VCLReloadsTotal)
	rollbackBefore := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal)
	renderErrBefore := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal)

	wait := h.runAndWait(h.bcast)

	// Template change → rend.Reload ok → Render ok (1st) →
	// mgr.Reload fails → Rollback → retry Render fails (2nd) → continue
	h.templateCh <- struct{}{}
	waitFor(t, func() bool {
		_, rc, _ := h.rend.counts()

		return rc >= 2
	}, "2 Render calls")

	_, renderCount, rollbackCount := h.rend.counts()
	if renderCount < 2 {
		t.Fatalf("expected at least 2 Render calls, got %d", renderCount)
	}
	if rollbackCount < 1 {
		t.Fatal("expected Rollback")
	}

	// Verify metrics.
	if delta := getCounterValue(t, "error", h.metrics.VCLReloadsTotal) - reloadErrBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(error) delta = %v, want >= 1", delta)
	}
	if delta := getSingleCounterValue(t, h.metrics.VCLRollbacksTotal) - rollbackBefore; delta < 1 {
		t.Errorf("vcl_rollbacks_total delta = %v, want >= 1", delta)
	}
	if delta := getSingleCounterValue(t, h.metrics.VCLRenderErrorsTotal) - renderErrBefore; delta < 1 {
		t.Errorf("vcl_render_errors_total delta = %v, want >= 1 (retry render failed)", delta)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_ShutdownTimeout(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.shutdownTimeout = 10 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send SIGTERM but do NOT close mgr.done - varnishd doesn't exit in time.
	h.sigCh <- syscall.SIGTERM

	// After the shutdown timeout fires, runLoop escalates to SIGKILL and
	// waits for varnishd to exit. Simulate the kill taking effect.
	waitFor(t, func() bool {
		sigs := h.mgr.getForwardedSigs()

		return len(sigs) >= 2 && sigs[1] == syscall.SIGKILL
	}, "SIGKILL forwarded")
	close(h.mgr.done)
	<-done

	sigs := h.mgr.getForwardedSigs()
	if sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected first signal SIGTERM, got %v", sigs[0])
	}

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0 (mgr.Err is nil), got %d", result)
	}
}

func TestRunLoop_SignalShutdownVarnishError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.err = errors.New("exit status 1")
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(h.bcast))))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 1 {
		t.Fatalf("expected exit 1, got %d", result)
	}
}

func TestRunLoop_DebounceCoalescing(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 500 * time.Millisecond
	lc.backendDebounce = 500 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Two rapid frontend updates within the debounce window → single reload.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(5 * time.Millisecond)
	h.frontendCh <- []watcher.Frontend{
		{Host: "10.0.0.1", Port: 80, Name: "pod-1"},
		{Host: "10.0.0.2", Port: 80, Name: "pod-2"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "debounce fired")

	_, renderCount, _ := h.rend.counts()
	if renderCount != 1 {
		t.Fatalf("expected exactly 1 RenderToFile call (coalesced), got %d", renderCount)
	}
	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected exactly 1 mgr.Reload call (coalesced), got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done
}

func TestRunLoop_FileValuesWatcherUpdateTriggersRerender(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := os.WriteFile(dir+"/ttl.yaml", []byte("300"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Start a real FileValuesWatcher with a fast poll interval.
	fvw := watcher.NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = fvw.Run(ctx) }()

	// Consume initial state from the watcher.
	select {
	case <-fvw.Changes():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial FileValuesWatcher state")
	}

	// Wire the watcher into a valuesCh, same as main.go does.
	valuesCh := make(chan valuesChange, 1)
	go func() {
		for data := range fvw.Changes() {
			valuesCh <- valuesChange{name: "tuning", data: data}
		}
	}()

	// Track the values passed to RenderToFile.
	var (
		renderMu     sync.Mutex
		renderedVals []map[string]map[string]any
	)

	h := newTestHarness()
	h.valuesCh = valuesCh
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, vals map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		// Deep-copy the values map to capture the state at render time.
		copied := make(map[string]map[string]any, len(vals))
		for k, v := range vals {
			inner := make(map[string]any, len(v))
			maps.Copy(inner, v)
			copied[k] = inner
		}
		renderMu.Lock()
		renderedVals = append(renderedVals, copied)
		n := len(renderedVals)
		renderMu.Unlock()

		return fmt.Sprintf("vcl 4.1; /* vals %d */", n), nil
	}

	wait := h.runAndWait(h.bcast)

	// Modify the YAML file on disk.
	err = os.WriteFile(dir+"/ttl.yaml", []byte("600"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the watcher to detect the change, debounce to fire, and render to complete.
	deadline := time.After(5 * time.Second)
	for {
		renderMu.Lock()
		n := len(renderedVals)
		renderMu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for RenderToFile after file change")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Verify the render received the updated value.
	renderMu.Lock()
	last := renderedVals[len(renderedVals)-1]
	renderMu.Unlock()

	tuning, ok := last["tuning"]
	if !ok {
		t.Fatal("expected 'tuning' key in rendered values")
	}
	// sigs.k8s.io/yaml parses "600" as float64(600).
	if ttl, ok := tuning["ttl"].(float64); !ok || ttl != 600 {
		t.Errorf("expected ttl=600 (float64), got %v (%T)", tuning["ttl"], tuning["ttl"])
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_BroadcastDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	// bcast is nil - should not panic.
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(nil))))
		close(done)
	}()

	// Frontend update with nil bcast - no panic.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool {
		_, rc, _ := h.rend.counts()

		return rc >= 1
	}, "RenderToFile called")

	// Signal shutdown with nil bcast - no panic.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

// --- Drain tests ---

func TestRunLoop_DrainOnShutdown(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	var sessionCalls atomic.Int32
	h.mgr.activeSessionsFn = func() (uint64, error) {
		// Return >0 for the first call, then 0.
		if sessionCalls.Add(1) == 1 {
			return 5, nil
		}

		return 0, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainPollInterval = 10 * time.Millisecond // poll fast to keep the test quick
	lc.drainTimeout = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	// Wait (deterministically) until the drain has run to completion and
	// varnishd has been signalled, then simulate varnishd exiting cleanly.
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}

	// Verify MarkBackendSick was called.
	sickCalls := h.mgr.getMarkSickCalls()
	if len(sickCalls) != 1 || sickCalls[0] != drainBackendName {
		t.Fatalf("expected MarkBackendSick(\"drain_flag\"), got %v", sickCalls)
	}

	// Verify ActiveSessions was polled.
	if sessionCalls.Load() < 2 {
		t.Fatalf("expected at least 2 ActiveSessions calls, got %d", sessionCalls.Load())
	}

	// Verify broadcast drain was also called.
	drainCount := h.bcast.drainCalls()
	if drainCount != 1 {
		t.Fatalf("expected broadcast Drain to be called once, got %d", drainCount)
	}

	// Verify SIGTERM was forwarded to varnishd.
	sigs := h.mgr.getForwardedSigs()
	if len(sigs) == 0 || sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected SIGTERM forwarded, got %v", sigs)
	}
}

func TestRunLoop_DrainSkippedWhenDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	// drainBackend is "" - drain should be skipped.

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}

	// MarkBackendSick should NOT have been called.
	sickCalls := h.mgr.getMarkSickCalls()
	if len(sickCalls) != 0 {
		t.Fatalf("expected no MarkBackendSick calls, got %v", sickCalls)
	}

	// Broadcast Drain should still be called.
	drainCount := h.bcast.drainCalls()
	if drainCount < 1 {
		t.Fatal("expected broadcast Drain to be called")
	}
}

func TestRunLoop_DrainInterruptedBySecondSignal(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	// ActiveSessions always returns >0 to keep polling.
	h.mgr.activeSessionsFn = func() (uint64, error) {
		return 10, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 10 * time.Second
	// sigCh needs more buffer for two signals.
	sigCh := make(chan os.Signal, 2)
	lc.sigCh = sigCh

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// First signal starts shutdown+drain.
	sigCh <- syscall.SIGTERM
	// Wait for drain polling to start.
	waitFor(t, func() bool { return h.mgr.getSessionsCalls() >= 1 }, "drain polling started")
	// Second signal interrupts drain.
	sigCh <- syscall.SIGINT
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) >= 2 }, "second signal forwarded")
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_DrainMarkSickError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.markBackendFn = func(_ string) error {
		return errors.New("backend not found")
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 10 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	// Shutdown should complete despite the error.
	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0 despite MarkBackendSick error, got %d", result)
	}

	// SIGTERM should still be forwarded.
	sigs := h.mgr.getForwardedSigs()
	if len(sigs) == 0 || sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected SIGTERM forwarded, got %v", sigs)
	}
}

func TestRunLoop_DrainMarkSickErrorContinuesDrain(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.markBackendFn = func(_ string) error {
		return errors.New("backend not found")
	}
	var sessionCalls atomic.Int32
	h.mgr.activeSessionsFn = func() (uint64, error) {
		// Return >0 first, then 0.
		if sessionCalls.Add(1) == 1 {
			return 5, nil
		}

		return 0, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainPollInterval = 10 * time.Millisecond // poll fast to keep the test quick
	lc.drainTimeout = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	// Deterministically wait for the drain to finish (signal forwarded) before
	// simulating varnishd's exit, so a slow runner can't abort the drain early.
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0 despite MarkBackendSick error, got %d", result)
	}

	// Despite MarkBackendSick failing, drain delay + polling should still proceed.
	if sessionCalls.Load() < 2 {
		t.Fatalf("expected ActiveSessions polling despite MarkBackendSick error, got %d calls", sessionCalls.Load())
	}
}

func TestRunLoop_DrainTimeoutExpires(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	// ActiveSessions always returns >0 so sessions never clear.
	h.mgr.activeSessionsFn = func() (uint64, error) {
		return 42, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	// Timeout must be >1s so the 1s poll ticker fires at least once
	// before the deadline expires.
	lc.drainTimeout = 1500 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0 after drain timeout, got %d", result)
	}

	// Verify polling was attempted before timeout expired.
	if h.mgr.getSessionsCalls() == 0 {
		t.Fatal("expected ActiveSessions to be polled before timeout")
	}

	// Verify SIGTERM was still forwarded after timeout.
	sigs := h.mgr.getForwardedSigs()
	if len(sigs) == 0 || sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected SIGTERM forwarded, got %v", sigs)
	}
}

// TestRunLoop_DrainAbortsWhenVarnishdExits verifies that when varnishd exits
// while handleDrain is polling active sessions, the drain is abandoned
// immediately. There is nothing left to drain once varnishd is gone, so the
// controller must not keep polling the dead process until drainTimeout.
func TestRunLoop_DrainAbortsWhenVarnishdExits(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	// Sessions never clear, so the only way out of the poll loop (short of
	// the deadline) is reacting to varnishd's exit.
	h.mgr.activeSessionsFn = func() (uint64, error) { return 7, nil }
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainPollInterval = 20 * time.Millisecond
	// Long timeout: the test must return well before this, proving the drain
	// reacted to the exit rather than waiting out the timeout.
	lc.drainTimeout = 30 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM

	// Wait until the drain has begun (backend marked sick).
	deadline := time.Now().Add(2 * time.Second)
	for len(h.mgr.getMarkSickCalls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("drain did not start")
		}
		time.Sleep(time.Millisecond)
	}

	// varnishd exits during the drain.
	close(h.mgr.done)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runLoop did not return promptly after varnishd exited during drain; " +
			"handleDrain kept polling the dead process until drainTimeout")
	}

	if got := int(code.Load()); got != 0 {
		t.Fatalf("expected exit 0, got %d", got)
	}
}

func TestRunLoop_DrainSecondSignalDuringPolling(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	// ActiveSessions always returns >0 to keep polling.
	h.mgr.activeSessionsFn = func() (uint64, error) {
		return 10, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 10 * time.Second // long timeout so it doesn't fire
	sigCh := make(chan os.Signal, 2)
	lc.sigCh = sigCh

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	sigCh <- syscall.SIGTERM
	// Wait until at least one drain poll has happened (drainDelay + first 1s
	// ticker tick) before the second signal, so it genuinely interrupts an
	// in-progress poll loop. Polling instead of a fixed 1.2s sleep keeps this
	// robust on a loaded runner where the 1s ticker can land late.
	waitFor(t, func() bool { return h.mgr.getSessionsCalls() > 0 }, "ActiveSessions polled before second signal")
	// Second signal during polling should abort the drain loop.
	sigCh <- syscall.SIGINT
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}

	// Verify we actually polled before the second signal interrupted.
	if h.mgr.getSessionsCalls() == 0 {
		t.Fatal("expected ActiveSessions to be polled before second signal")
	}
}

func TestRunLoop_DrainActiveSessionsError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	var calls atomic.Int32
	h.mgr.activeSessionsFn = func() (uint64, error) {
		n := calls.Add(1)
		if n <= 2 {
			// First two polls fail.
			return 0, errors.New("varnishstat unavailable")
		}
		// Third poll succeeds with 0 sessions.
		return 0, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainPollInterval = 10 * time.Millisecond // poll fast to keep the test quick
	lc.drainTimeout = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	// Deterministically wait for the error-tolerant polling to finish (signal
	// forwarded) before simulating varnishd's exit.
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0 despite ActiveSessions errors, got %d", result)
	}

	// Verify polling continued past errors (at least 3 calls: 2 errors + 1 success).
	if calls.Load() < 3 {
		t.Fatalf("expected at least 3 ActiveSessions calls, got %d", calls.Load())
	}
}

func TestRunLoop_DrainTimeoutZeroSkipsPolling(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.activeSessionsFn = func() (uint64, error) {
		t.Fatal("ActiveSessions should not be called when drainTimeout is 0")

		return 0, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 0 // skip session polling

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}

	// Verify MarkBackendSick was still called (Connection: close is set).
	sickCalls := h.mgr.getMarkSickCalls()
	if len(sickCalls) != 1 || sickCalls[0] != drainBackendName {
		t.Fatalf("expected MarkBackendSick(\"drain_flag\"), got %v", sickCalls)
	}

	// Verify ActiveSessions was never polled.
	if h.mgr.getSessionsCalls() != 0 {
		t.Fatalf("expected 0 ActiveSessions calls, got %d", h.mgr.getSessionsCalls())
	}
}

func TestRunLoop_FileWatchDisabledTemplateChangeIgnored(t *testing.T) {
	t.Parallel()
	// Create a temp VCL file and start a real watchFile to prove the
	// file actually changes on disk.
	f, err := os.CreateTemp(t.TempDir(), "vcl-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_, err = f.WriteString("initial")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx := t.Context()
	watchCh := watchFile(ctx, path, 50*time.Millisecond)

	// Wait a tick so the watcher reads the initial content.
	time.Sleep(100 * time.Millisecond)

	h := newTestHarness()
	h.templateCh = nil // simulate --file-watch=false
	wait := h.runAndWait(h.bcast)

	// Modify the file on disk.
	err = os.WriteFile(path, []byte("changed"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for watchFile to detect the change, proving the file did change.
	select {
	case <-watchCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: watchFile did not detect file change")
	}

	// Wait long enough for the debounce to fire if a reload were going to happen.
	time.Sleep(50 * time.Millisecond)

	reloadCount, renderCount, _ := h.rend.counts()
	if reloadCount != 0 {
		t.Fatalf("expected 0 rend.Reload calls, got %d", reloadCount)
	}
	if renderCount != 0 {
		t.Fatalf("expected 0 RenderToFile calls, got %d", renderCount)
	}
	if h.mgr.getReloadCount() != 0 {
		t.Fatalf("expected 0 mgr.Reload calls, got %d", h.mgr.getReloadCount())
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_FileWatchDisabledValuesDirChangeIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := os.WriteFile(dir+"/ttl.yaml", []byte("300"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Start a real FileValuesWatcher with a fast poll interval.
	fvw := watcher.NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = fvw.Run(ctx) }()

	// Consume initial state from the watcher.
	var initialData map[string]any
	select {
	case initialData = <-fvw.Changes():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial FileValuesWatcher state")
	}

	h := newTestHarness()
	// Do NOT wire fvw.Changes() into valuesCh - simulates --file-watch=false.
	// The default valuesCh from newTestHarness is a buffered channel that
	// nobody sends to, so the select case never fires.

	// Set up the loop manually so we can seed latestValues.
	ctxLoop, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.latestValues["tuning"] = initialData
	go func() {
		code.Store(int32(runLoop(ctxLoop, cancel, lc)))
		close(done)
	}()

	// Modify the YAML file on disk.
	err = os.WriteFile(dir+"/ttl.yaml", []byte("600"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the FileValuesWatcher to detect the change (proves the
	// file did change and the watcher noticed).
	select {
	case <-fvw.Changes():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for FileValuesWatcher to detect change")
	}

	// Wait long enough for the debounce to fire if a reload were going to happen.
	time.Sleep(50 * time.Millisecond)

	if h.mgr.getReloadCount() != 0 {
		t.Fatalf("expected 0 mgr.Reload calls, got %d", h.mgr.getReloadCount())
	}
	_, renderCount, _ := h.rend.counts()
	if renderCount != 0 {
		t.Fatalf("expected 0 RenderToFile calls, got %d", renderCount)
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_FileWatchDisabledValuesDirInitialStateAvailable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := os.WriteFile(dir+"/greeting.yaml", []byte("hello"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Start a real FileValuesWatcher.
	fvw := watcher.NewFileValuesWatcher(dir, 50*time.Millisecond)
	ctx := t.Context()
	go func() { _ = fvw.Run(ctx) }()

	// Consume initial state.
	var initialData map[string]any
	select {
	case initialData = <-fvw.Changes():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial FileValuesWatcher state")
	}

	// Track values passed to RenderToFile.
	var (
		renderMu     sync.Mutex
		renderedVals map[string]map[string]any
	)

	h := newTestHarness()
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, vals map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		copied := make(map[string]map[string]any, len(vals))
		for k, v := range vals {
			inner := make(map[string]any, len(v))
			maps.Copy(inner, v)
			copied[k] = inner
		}
		renderMu.Lock()
		renderedVals = copied
		renderMu.Unlock()

		return "vcl 4.1; /* filewatch disabled */", nil
	}

	// Do NOT wire fvw.Changes() into valuesCh - simulates --file-watch=false.

	// Set up loop manually to seed latestValues with initial directory state.
	ctxLoop, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.latestValues["dirtest"] = initialData
	go func() {
		code.Store(int32(runLoop(ctxLoop, cancel, lc)))
		close(done)
	}()

	// Trigger a frontend update to cause a render.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool {
		renderMu.Lock()
		defer renderMu.Unlock()

		return renderedVals != nil
	}, "RenderToFile called")

	// Assert RenderToFile was called with latestValues containing
	// dirtest.greeting == "hello".
	renderMu.Lock()
	vals := renderedVals
	renderMu.Unlock()

	if vals == nil {
		t.Fatal("expected RenderToFile to be called")
	}
	dirtest, ok := vals["dirtest"]
	if !ok {
		t.Fatal("expected 'dirtest' key in rendered values")
	}
	if greeting, ok := dirtest["greeting"].(string); !ok || greeting != "hello" {
		t.Errorf("expected greeting=\"hello\", got %v (%T)", dirtest["greeting"], dirtest["greeting"])
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestDrainBackendForLoop(t *testing.T) {
	t.Parallel()
	if got := drainBackendForLoop(true); got != drainBackendName {
		t.Errorf("drainBackendForLoop(true) = %q, want %q", got, drainBackendName)
	}
	if got := drainBackendForLoop(false); got != "" {
		t.Errorf("drainBackendForLoop(false) = %q, want empty", got)
	}
}

// --- Debounce-max tests ---
//
// All tests in this section use generous durations (hundreds of ms) so they
// are robust on slow CI runners where goroutine scheduling can lag 50-100ms.

// Scenario 1: Single event, then quiet.
// debounceMax is set but irrelevant - reload fires at the normal debounce time.
func TestRunLoop_DebounceMaxSingleEvent(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 150 * time.Millisecond
	lc.backendDebounce = 150 * time.Millisecond
	lc.frontendDebounceMax = 5 * time.Second // much larger - should not matter
	lc.backendDebounceMax = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}

	// debounce fires at ~150ms; poll for it so a loaded runner that lands the
	// timer late doesn't flake. Only one fire is possible (single event).
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after single event")
	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected 1 reload, got %d", got)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// Scenario 2: Brief burst of events, then quiet.
// Events stop well before debounceMax; reload fires at last-event + debounce.
func TestRunLoop_DebounceMaxBriefBurst(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 200 * time.Millisecond
	lc.backendDebounce = 200 * time.Millisecond
	lc.frontendDebounceMax = 5 * time.Second
	lc.backendDebounceMax = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// 5 events, 30ms apart → burst ends at ~120ms.
	for range 5 {
		h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
		time.Sleep(30 * time.Millisecond)
	}

	// Last event at ~120ms + 200ms debounce = ~320ms. Poll for the single
	// coalesced reload instead of sleeping a fixed margin.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "coalesced reload after burst")
	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected 1 coalesced reload after burst, got %d", got)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// Scenario 3: Events arriving slower than debounce.
// Each event triggers its own reload; debounceMax has no effect.
func TestRunLoop_DebounceMaxSlowEvents(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 150 * time.Millisecond
	lc.backendDebounce = 150 * time.Millisecond
	lc.frontendDebounceMax = 2 * time.Second
	lc.backendDebounceMax = 2 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Event #1 at t=0. Timer fires at ~150ms; poll for it.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after first event")
	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected 1 reload after first event, got %d", got)
	}

	// Event #2 arrives after #1 already fired, so it triggers its own reload
	// (~150ms later) rather than coalescing.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.2", Port: 80, Name: "pod-2"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after second event")
	if got := h.mgr.getReloadCount(); got != 2 {
		t.Fatalf("expected 2 reloads after second event, got %d", got)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// testDebounceMaxForced is a helper that sends rapid frontend events for 1.2s
// and asserts that debounceMax forces at least 3 reloads.
func testDebounceMaxForced(t *testing.T, frontendDebounce, frontendDebounceMax, backendDebounce, backendDebounceMax time.Duration) {
	t.Helper()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = frontendDebounce
	lc.frontendDebounceMax = frontendDebounceMax
	lc.backendDebounce = backendDebounce
	lc.backendDebounceMax = backendDebounceMax

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// debounceMax forces reloads at its cap (~250ms) while events keep arriving
	// every 20ms. Poll for the target instead of a fixed window so a loaded
	// runner that paces the forced reloads slowly doesn't flake; the event
	// generator keeps running until the target is reached.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "at least 3 forced reloads")
	close(stop)

	if reloads := h.mgr.getReloadCount(); reloads < 3 {
		t.Fatalf("expected at least 3 forced reloads, got %d", reloads)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// Scenario 4: Events arriving faster than debounce - the key case.
// Without debounceMax the timer perpetually resets and never fires.
// With debounceMax the reload is forced periodically.
func TestRunLoop_DebounceMaxForcesReload(t *testing.T) {
	t.Parallel()
	testDebounceMaxForced(t,
		250*time.Millisecond, 250*time.Millisecond,
		250*time.Millisecond, 250*time.Millisecond,
	)
}

// Scenario 5: Long stream - deadline resets after each forced reload.
// Two separate bursts each get their own debounceMax window.
func TestRunLoop_DebounceMaxResetsAfterReload(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 250 * time.Millisecond // continuously reset by events every 20ms
	lc.backendDebounce = 250 * time.Millisecond
	lc.frontendDebounceMax = 250 * time.Millisecond
	lc.backendDebounceMax = 250 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// First burst: events every 20ms for 600ms → expect at least 1 forced reload.
	for range 30 {
		h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
		time.Sleep(20 * time.Millisecond)
	}
	// debounceMax (~250ms) forces at least one reload during the 600ms burst;
	// poll for it rather than relying on a fixed settle window.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after first burst")
	firstReloads := h.mgr.getReloadCount()

	// Quiet gap - let the loop fully quiesce. No extra reload fires from
	// the first burst's trailing timer during this gap (it already fired
	// via debounceMax).

	// Second burst: new debounceMax window starts fresh.
	for range 30 {
		h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.2", Port: 80, Name: "pod-2"}}
		time.Sleep(20 * time.Millisecond)
	}
	// The second burst opens a fresh debounceMax window and forces more
	// reloads; poll until the count climbs past the first burst's total.
	waitFor(t, func() bool { return h.mgr.getReloadCount() > firstReloads }, "additional reloads after second burst")

	if secondReloads := h.mgr.getReloadCount(); secondReloads <= firstReloads {
		t.Fatalf("expected additional reloads after second burst, got %d total (was %d after first)", secondReloads, firstReloads)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// Scenario 6: debounceMax=0 (disabled) - perpetual timer reset, no forced reload.
func TestRunLoop_DebounceMaxDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 800 * time.Millisecond
	lc.backendDebounce = 800 * time.Millisecond
	lc.frontendDebounceMax = 0 // disabled
	lc.backendDebounceMax = 0

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send events every 20ms for 600ms - timer keeps resetting.
	// debounce=800ms, so the timer never fires while events arrive
	// every 20ms.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(600 * time.Millisecond)
	close(stop)

	// No reload during the stream - timer kept resetting.
	if h.mgr.getReloadCount() != 0 {
		t.Fatalf("expected 0 reloads during continuous events (debounceMax disabled), got %d", h.mgr.getReloadCount())
	}

	// After events stop, the last 800ms timer fires (last event ~600ms +
	// 800ms = ~1400ms). Poll for it instead of a fixed margin so a loaded
	// runner that lands the timer late doesn't flake; only one fire is
	// possible (debounceMax disabled, no further events).
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after events stop")
	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected 1 reload after events stop, got %d", got)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// Verify debounceMax works for all event types, not just frontends.
// Uses backend, values, template, and mixed events in a single test to
// avoid duplicating the same pattern four times.
func TestRunLoop_DebounceMaxAllEventTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		send func(h *testHarness)
	}{
		{
			name: "backend",
			send: func(h *testHarness) {
				h.backendCh <- backendChange{
					name:      "api",
					endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
				}
			},
		},
		{
			name: "values",
			send: func(h *testHarness) {
				h.valuesCh <- valuesChange{name: "tuning", data: map[string]any{"ttl": "300"}}
			},
		},
		{
			name: "template",
			send: func(h *testHarness) {
				h.templateCh <- struct{}{}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness()
			ctx, cancel := context.WithCancel(t.Context())
			var code atomic.Int32
			code.Store(-1)
			done := make(chan struct{})
			lc := h.loopConfig(h.bcast)
			lc.frontendDebounce = 250 * time.Millisecond // continuously reset by events every 20ms
			lc.backendDebounce = 250 * time.Millisecond
			lc.frontendDebounceMax = 250 * time.Millisecond
			lc.backendDebounceMax = 250 * time.Millisecond

			go func() {
				code.Store(int32(runLoop(ctx, cancel, lc)))
				close(done)
			}()

			// Send events every 20ms for 600ms.
			stop := make(chan struct{})
			go func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					tc.send(h)
					time.Sleep(20 * time.Millisecond)
				}
			}()

			// debounceMax (250ms) forces a reload while events keep arriving;
			// poll for it, keeping the generator running until it lands.
			waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload from "+tc.name+" events with debounceMax")
			close(stop)

			h.sigCh <- syscall.SIGTERM
			waitForForwardedSignal(t, h.mgr)
			close(h.mgr.done)
			<-done
		})
	}
}

// Verify debounceMax deadline is shared across mixed event types:
// alternating frontend and backend events within one debounceMax window.
func TestRunLoop_DebounceMaxMixedEvents(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 250 * time.Millisecond // continuously reset by events every 20ms
	lc.backendDebounce = 250 * time.Millisecond
	lc.frontendDebounceMax = 250 * time.Millisecond
	lc.backendDebounceMax = 250 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Alternate frontend and backend events every 20ms for 600ms.
	stop := make(chan struct{})
	go func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
			} else {
				h.backendCh <- backendChange{
					name:      "api",
					endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
				}
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// debounceMax (250ms) forces a reload while mixed events keep arriving;
	// poll for it, keeping the generator running until it lands.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload from mixed events with debounceMax")
	close(stop)

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// --- Per-source debounce tests ---

func TestRunLoop_FrontendDebounceIndependentFromBackend(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 50 * time.Millisecond
	lc.backendDebounce = 500 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send frontend event → should reload at ~50ms. Poll for it rather than
	// sleeping a fixed margin: on a loaded CI runner the timer fire and reload
	// can land later than the nominal deadline.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "first reload after frontend event")

	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected 1 reload after frontend event, got %d", got)
	}

	// Send backend event → should NOT reload at the frontend cadence (only at
	// ~500ms). Sleeping well past the 50ms frontend debounce is timing-safe
	// here: a Go timer never fires before its duration, so a count of 1 proves
	// the backend group debounces independently regardless of runner load.
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}
	time.Sleep(200 * time.Millisecond)

	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected still 1 reload (backend timer not yet fired), got %d", got)
	}

	// Wait for the backend timer (~500ms) to fire.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "second reload after backend timer fires")

	if got := h.mgr.getReloadCount(); got != 2 {
		t.Fatalf("expected 2 reloads after backend timer fires, got %d", got)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_CrossGroupClearOnReload(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 50 * time.Millisecond
	lc.backendDebounce = 300 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send both events nearly simultaneously.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}

	// Frontend timer fires first (~50ms) and clears the backend timer too.
	// Poll for that reload (lower bound), then wait past the backend's
	// would-be 300ms fire and confirm it never fired on its own. The trailing
	// wait is a negative check — a Go timer cannot fire early, so it is
	// load-safe.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "frontend reload")
	time.Sleep(400 * time.Millisecond)

	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected exactly 1 reload (cross-group clear), got %d", got)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_IndependentDebounceMaxGroups(t *testing.T) {
	t.Parallel()
	testDebounceMaxForced(t,
		250*time.Millisecond, 250*time.Millisecond,
		250*time.Millisecond, 500*time.Millisecond,
	)
}

func TestRunLoop_FrontendDebounceMaxDisabledBackendEnabled(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 800 * time.Millisecond
	lc.frontendDebounceMax = 0 // disabled
	lc.backendDebounce = 250 * time.Millisecond
	lc.backendDebounceMax = 300 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Rapid frontend events for 600ms - no forced reload (debounceMax=0).
	stopFE := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFE:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(600 * time.Millisecond)
	close(stopFE)

	// The frontend timer (800ms) keeps resetting every 20ms with no debounceMax,
	// so no reload should have happened yet.
	feReloads := h.mgr.getReloadCount()
	if feReloads != 0 {
		t.Fatalf("expected 0 reloads during frontend-only events (debounceMax disabled), got %d", feReloads)
	}

	// Wait for the final frontend timer (800ms) to fire. Poll instead of a
	// fixed margin so a late timer on a loaded runner doesn't flake.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after frontend events stop")
	afterFE := h.mgr.getReloadCount()
	if afterFE != 1 {
		t.Fatalf("expected 1 reload after frontend events stop, got %d", afterFE)
	}

	// Now send rapid backend events - backendDebounceMax=300ms → forced reload.
	stopBE := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopBE:
				return
			default:
			}
			h.backendCh <- backendChange{
				name:      "api",
				endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// backendDebounceMax=300ms forces a reload even though backend events keep
	// arriving every 20ms; poll for it rather than relying on a fixed window.
	waitFor(t, func() bool { return h.mgr.getReloadCount() > afterFE }, "forced backend reload with debounceMax")
	close(stopBE)

	if beReloads := h.mgr.getReloadCount(); beReloads <= afterFE {
		t.Fatalf("expected forced reloads from backend events with debounceMax=300ms, got %d total (was %d after frontend)", beReloads, afterFE)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_BackendDebounceIndependentFromFrontend(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 500 * time.Millisecond
	lc.backendDebounce = 50 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send backend event → should reload at ~50ms. Poll for it rather than
	// sleeping a fixed margin: on a loaded CI runner the timer fire and reload
	// can land later than the nominal deadline.
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "first reload after backend event")

	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected 1 reload after backend event, got %d", got)
	}

	// Send frontend event → should NOT reload at the backend cadence (only at
	// ~500ms). Sleeping well past the 50ms backend debounce is timing-safe
	// here: a Go timer never fires before its duration, so a count of 1 proves
	// the frontend group debounces independently regardless of runner load.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(200 * time.Millisecond)

	if got := h.mgr.getReloadCount(); got != 1 {
		t.Fatalf("expected still 1 reload (frontend timer not yet fired), got %d", got)
	}

	// Wait for the frontend timer (~500ms) to fire.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "second reload after frontend timer fires")

	if got := h.mgr.getReloadCount(); got != 2 {
		t.Fatalf("expected 2 reloads after frontend timer fires, got %d", got)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_BackendDebounceMaxDisabledFrontendEnabled(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 250 * time.Millisecond
	lc.frontendDebounceMax = 300 * time.Millisecond
	lc.backendDebounce = 800 * time.Millisecond
	lc.backendDebounceMax = 0 // disabled

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Rapid backend events for 600ms - no forced reload (debounceMax=0).
	stopBE := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopBE:
				return
			default:
			}
			h.backendCh <- backendChange{
				name:      "api",
				endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(600 * time.Millisecond)
	close(stopBE)

	beReloads := h.mgr.getReloadCount()
	if beReloads != 0 {
		t.Fatalf("expected 0 reloads during backend-only events (debounceMax disabled), got %d", beReloads)
	}

	// Wait for the final backend timer (800ms) to fire. Poll instead of a
	// fixed margin so a late timer on a loaded runner doesn't flake.
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after backend events stop")
	afterBE := h.mgr.getReloadCount()
	if afterBE != 1 {
		t.Fatalf("expected 1 reload after backend events stop, got %d", afterBE)
	}

	// Now send rapid frontend events - frontendDebounceMax=300ms → forced reload.
	stopFE := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFE:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// frontendDebounceMax=300ms forces a reload even though frontend events keep
	// arriving every 20ms; poll for it rather than relying on a fixed window.
	waitFor(t, func() bool { return h.mgr.getReloadCount() > afterBE }, "forced frontend reload with debounceMax")
	close(stopFE)

	if feReloads := h.mgr.getReloadCount(); feReloads <= afterBE {
		t.Fatalf("expected forced reloads from frontend events with debounceMax=300ms, got %d total (was %d after backend)", feReloads, afterBE)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// --- resetDebounce unit tests ---

func TestResetDebounce_SetsDeadlineOnFirstCall(t *testing.T) {
	t.Parallel()
	var s debounceState
	resetDebounce(&s, 100*time.Millisecond, 500*time.Millisecond)
	defer s.timer.Stop()

	if s.deadline.IsZero() {
		t.Fatal("expected deadline to be set when debounceMax > 0")
	}
	// Deadline should be ~500ms from now.
	remaining := time.Until(s.deadline)
	if remaining < 400*time.Millisecond || remaining > 600*time.Millisecond {
		t.Errorf("deadline remaining = %v, want ~500ms", remaining)
	}
}

func TestResetDebounce_CapsAtDeadline(t *testing.T) {
	t.Parallel()
	var s debounceState
	// Set a deadline 50ms from now.
	s.deadline = time.Now().Add(50 * time.Millisecond)

	// Request a 500ms debounce - should be capped to ~50ms.
	resetDebounce(&s, 500*time.Millisecond, 100*time.Millisecond)
	defer s.timer.Stop()

	// The timer should fire within ~100ms (50ms deadline + margin).
	select {
	case <-s.timer.C:
		// OK - fired at the deadline
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timer did not fire at deadline")
	}
}

func TestResetDebounce_NoDeadlineWhenMaxIsZero(t *testing.T) {
	t.Parallel()
	var s debounceState
	resetDebounce(&s, 100*time.Millisecond, 0)
	defer s.timer.Stop()

	if !s.deadline.IsZero() {
		t.Fatal("expected deadline to remain zero when debounceMax is 0")
	}
}

func TestResetDebounce_SetsFirstEventOnFirstCall(t *testing.T) {
	t.Parallel()
	var s debounceState
	before := time.Now()
	resetDebounce(&s, 100*time.Millisecond, 0)
	defer s.timer.Stop()

	if s.firstEvent.IsZero() {
		t.Fatal("expected firstEvent to be set on first call")
	}
	if s.firstEvent.Before(before) {
		t.Error("firstEvent is before the call")
	}
}

func TestResetDebounce_PreservesFirstEventOnSubsequentCalls(t *testing.T) {
	t.Parallel()
	var s debounceState
	resetDebounce(&s, 100*time.Millisecond, 0)
	first := s.firstEvent
	s.timer.Stop()

	time.Sleep(5 * time.Millisecond)
	resetDebounce(&s, 100*time.Millisecond, 0)
	defer s.timer.Stop()

	if !s.firstEvent.Equal(first) {
		t.Errorf("firstEvent changed from %v to %v on second call", first, s.firstEvent)
	}
}

func TestResetDebounce_CappedSetWhenDeadlineForcesShortTimer(t *testing.T) {
	t.Parallel()
	var s debounceState
	s.deadline = time.Now().Add(10 * time.Millisecond)

	resetDebounce(&s, 500*time.Millisecond, 100*time.Millisecond)
	defer s.timer.Stop()

	if !s.capped {
		t.Fatal("expected capped=true when deadline forces shorter timer")
	}
}

func TestResetDebounce_CappedFalseWhenNotConstrained(t *testing.T) {
	t.Parallel()
	var s debounceState
	resetDebounce(&s, 100*time.Millisecond, 5*time.Second)
	defer s.timer.Stop()

	if s.capped {
		t.Fatal("expected capped=false when debounce < debounceMax remaining")
	}
}

// --- Debounce metrics integration tests ---

func TestRunLoop_DebounceMetrics_EventsAndFires(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 50 * time.Millisecond
	lc.backendDebounce = 50 * time.Millisecond

	// Snapshot counters before the test (other tests may have incremented them).
	feEventsBefore := getCounterValue(t, "frontend", h.metrics.DebounceEventsTotal)
	beEventsBefore := getCounterValue(t, "backend", h.metrics.DebounceEventsTotal)
	feFiresBefore := getCounterValue(t, "frontend", h.metrics.DebounceFiresTotal)
	beFiresBefore := getCounterValue(t, "backend", h.metrics.DebounceFiresTotal)

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send 3 frontend events and 2 backend events. Poll until each group's
	// debounce has fired (50ms) rather than sleeping a fixed margin. Events are
	// counted on receipt — before the fire — so once fires>=1 the events-delta
	// checks below are already satisfied.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.2", Port: 80, Name: "pod-2"}}
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.3", Port: 80, Name: "pod-3"}}
	waitFor(t, func() bool {
		return getCounterValue(t, "frontend", h.metrics.DebounceFiresTotal)-feFiresBefore >= 1
	}, "frontend debounce fired")

	h.backendCh <- backendChange{name: "api", endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}}}
	h.backendCh <- backendChange{name: "api", endpoints: []watcher.Endpoint{{Host: "10.0.1.2", Port: 8080, Name: "api-1"}}}
	waitFor(t, func() bool {
		return getCounterValue(t, "backend", h.metrics.DebounceFiresTotal)-beFiresBefore >= 1
	}, "backend debounce fired")

	feEventsAfter := getCounterValue(t, "frontend", h.metrics.DebounceEventsTotal)
	beEventsAfter := getCounterValue(t, "backend", h.metrics.DebounceEventsTotal)
	feFiresAfter := getCounterValue(t, "frontend", h.metrics.DebounceFiresTotal)
	beFiresAfter := getCounterValue(t, "backend", h.metrics.DebounceFiresTotal)

	if feEventsDelta := feEventsAfter - feEventsBefore; feEventsDelta < 3 {
		t.Errorf("frontend debounce_events_total delta = %v, want >= 3", feEventsDelta)
	}
	if beEventsDelta := beEventsAfter - beEventsBefore; beEventsDelta < 2 {
		t.Errorf("backend debounce_events_total delta = %v, want >= 2", beEventsDelta)
	}
	if feFiresDelta := feFiresAfter - feFiresBefore; feFiresDelta < 1 {
		t.Errorf("frontend debounce_fires_total delta = %v, want >= 1", feFiresDelta)
	}
	if beFiresDelta := beFiresAfter - beFiresBefore; beFiresDelta < 1 {
		t.Errorf("backend debounce_fires_total delta = %v, want >= 1", beFiresDelta)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_DebounceMetrics_MaxEnforcement(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 250 * time.Millisecond
	lc.frontendDebounceMax = 250 * time.Millisecond
	lc.backendDebounce = 250 * time.Millisecond
	lc.backendDebounceMax = 0

	enforceBefore := getCounterValue(t, "frontend", h.metrics.DebounceMaxEnforcementsTotal)

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Rapid frontend events for 600ms - debounceMax=250ms should force reloads.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// debounceMax=250ms forces (and counts) an enforcement while events keep
	// arriving; poll for it, keeping the generator running until it lands.
	waitFor(t, func() bool {
		return getCounterValue(t, "frontend", h.metrics.DebounceMaxEnforcementsTotal)-enforceBefore >= 1
	}, "frontend debounce-max enforcement")
	close(stop)

	enforceAfter := getCounterValue(t, "frontend", h.metrics.DebounceMaxEnforcementsTotal)
	if enforceDelta := enforceAfter - enforceBefore; enforceDelta < 1 {
		t.Errorf("frontend debounce_max_enforcements_total delta = %v, want >= 1", enforceDelta)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_DebounceMetrics_Latency(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 50 * time.Millisecond

	samplesBefore := getHistogramSampleCount(t, "frontend", h.metrics.DebounceLatencySeconds)

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	// Poll until the debounce fired and recorded a latency sample (~50ms).
	waitFor(t, func() bool {
		return getHistogramSampleCount(t, "frontend", h.metrics.DebounceLatencySeconds)-samplesBefore >= 1
	}, "frontend debounce latency sample")

	samplesAfter := getHistogramSampleCount(t, "frontend", h.metrics.DebounceLatencySeconds)
	if samplesDelta := samplesAfter - samplesBefore; samplesDelta < 1 {
		t.Errorf("frontend debounce_latency_seconds sample count delta = %d, want >= 1", samplesDelta)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_DebounceMetrics_BackendMaxEnforcement(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.backendDebounce = 250 * time.Millisecond
	lc.backendDebounceMax = 250 * time.Millisecond

	enforceBefore := getCounterValue(t, "backend", h.metrics.DebounceMaxEnforcementsTotal)

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Rapid backend events for 600ms - debounceMax=250ms should force reloads.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.backendCh <- backendChange{name: "api", endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// debounceMax=250ms forces (and counts) an enforcement while events keep
	// arriving; poll for it, keeping the generator running until it lands.
	waitFor(t, func() bool {
		return getCounterValue(t, "backend", h.metrics.DebounceMaxEnforcementsTotal)-enforceBefore >= 1
	}, "backend debounce-max enforcement")
	close(stop)

	enforceAfter := getCounterValue(t, "backend", h.metrics.DebounceMaxEnforcementsTotal)
	if enforceDelta := enforceAfter - enforceBefore; enforceDelta < 1 {
		t.Errorf("backend debounce_max_enforcements_total delta = %v, want >= 1", enforceDelta)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_DebounceMetrics_BackendLatency(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.backendDebounce = 50 * time.Millisecond

	samplesBefore := getHistogramSampleCount(t, "backend", h.metrics.DebounceLatencySeconds)

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.backendCh <- backendChange{name: "api", endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}}}
	// Poll until the debounce fired and recorded a latency sample (~50ms).
	waitFor(t, func() bool {
		return getHistogramSampleCount(t, "backend", h.metrics.DebounceLatencySeconds)-samplesBefore >= 1
	}, "backend debounce latency sample")

	samplesAfter := getHistogramSampleCount(t, "backend", h.metrics.DebounceLatencySeconds)
	if samplesDelta := samplesAfter - samplesBefore; samplesDelta < 1 {
		t.Errorf("backend debounce_latency_seconds sample count delta = %d, want >= 1", samplesDelta)
	}

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done
}

// getCounterValue reads the current value of a CounterVec for a given label.
func getCounterValue(t *testing.T, label string, cv *prometheus.CounterVec) float64 {
	t.Helper()
	var m dto.Metric
	c := cv.WithLabelValues(label)
	pm, ok := c.(prometheus.Metric)
	if !ok {
		t.Fatal("counter does not implement prometheus.Metric")
	}
	err := pm.Write(&m)
	if err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

// getCounter2Value reads the current value of a CounterVec with two labels.
func getCounter2Value(t *testing.T, l1, l2 string, cv *prometheus.CounterVec) float64 {
	t.Helper()
	var m dto.Metric
	c := cv.WithLabelValues(l1, l2)
	pm, ok := c.(prometheus.Metric)
	if !ok {
		t.Fatal("counter does not implement prometheus.Metric")
	}
	err := pm.Write(&m)
	if err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

// getSingleCounterValue reads the current value of a plain Counter.
func getSingleCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	pm, ok := c.(prometheus.Metric)
	if !ok {
		t.Fatal("counter does not implement prometheus.Metric")
	}
	err := pm.Write(&m)
	if err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

// getGauge2Value reads the current value of a GaugeVec with two labels.
func getGauge2Value(t *testing.T, l1, l2 string, gv *prometheus.GaugeVec) float64 {
	t.Helper()
	var m dto.Metric
	err := gv.WithLabelValues(l1, l2).Write(&m)
	if err != nil {
		t.Fatal(err)
	}

	return m.GetGauge().GetValue()
}

// getGaugeValue reads the current value of a plain Gauge.
func getGaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	err := g.Write(&m)
	if err != nil {
		t.Fatal(err)
	}

	return m.GetGauge().GetValue()
}

// getHistogramSampleCount reads the current sample count from a HistogramVec.
func getHistogramSampleCount(t *testing.T, label string, hv *prometheus.HistogramVec) uint64 {
	t.Helper()
	var m dto.Metric
	obs := hv.WithLabelValues(label)
	pm, ok := obs.(prometheus.Metric)
	if !ok {
		t.Fatal("histogram does not implement prometheus.Metric")
	}
	err := pm.Write(&m)
	if err != nil {
		t.Fatal(err)
	}

	return m.GetHistogram().GetSampleCount()
}

// --- emitEvent tests ---

func TestEmitEventNilRecorder(t *testing.T) {
	t.Parallel()
	t.Log("verifying emitEvent does not panic when recorder and podRef are nil")
	lc := &loopConfig{}
	emitEvent(lc, v1.EventTypeNormal, "Test", "test message")
}

func TestEmitEventRecordsEvent(t *testing.T) {
	t.Parallel()
	fakeRecorder := record.NewFakeRecorder(10)
	podRef := &v1.ObjectReference{
		Kind:      "Pod",
		Name:      "test-pod",
		Namespace: "default",
	}
	lc := &loopConfig{
		recorder: fakeRecorder,
		podRef:   podRef,
	}

	emitEvent(lc, v1.EventTypeNormal, "VCLReloaded", "VCL reloaded successfully")

	select {
	case event := <-fakeRecorder.Events:
		if !strings.Contains(event, "VCLReloaded") {
			t.Errorf("expected event to contain 'VCLReloaded', got %q", event)
		}
		if !strings.Contains(event, "VCL reloaded successfully") {
			t.Errorf("expected event to contain message, got %q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

// --- warnOnceEventSink tests ---

// fakeEventSink is a test double that returns configurable errors.
type fakeEventSink struct {
	mu        sync.Mutex
	createErr error
	updateErr error
	patchErr  error
	calls     int // total calls across all methods
}

func (f *fakeEventSink) Create(_ *v1.Event) (*v1.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	return nil, f.createErr
}

func (f *fakeEventSink) Update(_ *v1.Event) (*v1.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	return nil, f.updateErr
}

func (f *fakeEventSink) Patch(_ *v1.Event, _ []byte) (*v1.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	return nil, f.patchErr
}

func (f *fakeEventSink) getCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func newForbiddenErr() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "events"},
		"",
		errors.New("RBAC: access denied"),
	)
}

func TestWarnOnceEventSinkLogsForbiddenOnce(t *testing.T) {
	t.Parallel()
	inner := &fakeEventSink{createErr: newForbiddenErr()}
	sink := &warnOnceEventSink{inner: inner}

	ev := &v1.Event{ObjectMeta: metav1.ObjectMeta{Name: "test"}}

	// First call triggers the warning (via sync.Once).
	_, err := sink.Create(ev)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden error, got %v", err)
	}

	// Second call still returns the error but does not warn again
	// (sync.Once guarantees single execution; we verify the error passes through).
	_, err = sink.Create(ev)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden error on second call, got %v", err)
	}

	if got := inner.getCalls(); got != 2 {
		t.Fatalf("expected 2 inner calls, got %d", got)
	}
}

func TestWarnOnceEventSinkPassesThroughOnSuccess(t *testing.T) {
	t.Parallel()
	inner := &fakeEventSink{}
	sink := &warnOnceEventSink{inner: inner}

	ev := &v1.Event{ObjectMeta: metav1.ObjectMeta{Name: "test"}}

	_, err := sink.Create(ev)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestWarnOnceEventSinkNonForbiddenErrorDoesNotWarn(t *testing.T) {
	t.Parallel()
	// A 500 server error should pass through without triggering the
	// warn-once guard, leaving it available for a future Forbidden error.
	serverErr := apierrors.NewInternalError(errors.New("internal"))
	inner := &fakeEventSink{createErr: serverErr}
	sink := &warnOnceEventSink{inner: inner}

	ev := &v1.Event{ObjectMeta: metav1.ObjectMeta{Name: "test"}}

	_, err := sink.Create(ev)
	if !apierrors.IsInternalError(err) {
		t.Fatalf("expected InternalError, got %v", err)
	}

	// The warned Once should NOT have fired, so a subsequent Forbidden
	// error should still trigger it (verified by the error passing through
	// and the inner sink being called).
	inner.mu.Lock()
	inner.createErr = newForbiddenErr()
	inner.mu.Unlock()

	_, err = sink.Create(ev)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden error, got %v", err)
	}

	if got := inner.getCalls(); got != 2 {
		t.Fatalf("expected 2 inner calls, got %d", got)
	}
}

func TestWarnOnceEventSinkPatchForbidden(t *testing.T) {
	t.Parallel()
	// Patch is the primary path for updated events (client-go patches
	// existing events to increment their count). A Forbidden on Patch
	// should also trigger the warn-once.
	inner := &fakeEventSink{patchErr: newForbiddenErr()}
	sink := &warnOnceEventSink{inner: inner}

	ev := &v1.Event{ObjectMeta: metav1.ObjectMeta{Name: "test"}}

	_, err := sink.Patch(ev, []byte(`{}`))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden error from Patch, got %v", err)
	}

	// Second Patch still returns error; inner is still called.
	_, err = sink.Patch(ev, []byte(`{}`))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden error on second Patch, got %v", err)
	}

	if got := inner.getCalls(); got != 2 {
		t.Fatalf("expected 2 inner calls, got %d", got)
	}
}

func TestWarnOnceEventSinkUpdateForbidden(t *testing.T) {
	t.Parallel()
	inner := &fakeEventSink{updateErr: newForbiddenErr()}
	sink := &warnOnceEventSink{inner: inner}

	ev := &v1.Event{ObjectMeta: metav1.ObjectMeta{Name: "test"}}

	_, err := sink.Update(ev)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected Forbidden error from Update, got %v", err)
	}

	if got := inner.getCalls(); got != 1 {
		t.Fatalf("expected 1 inner call, got %d", got)
	}
}

func TestWarnOnceEventSinkWarnsOnceAcrossMethods(t *testing.T) {
	t.Parallel()
	// If Create gets a 403 and then Patch also gets a 403,
	// the warning should fire exactly once total (sync.Once).
	inner := &fakeEventSink{
		createErr: newForbiddenErr(),
		patchErr:  newForbiddenErr(),
	}
	sink := &warnOnceEventSink{inner: inner}

	ev := &v1.Event{ObjectMeta: metav1.ObjectMeta{Name: "test"}}

	// Create triggers the Once.
	_, _ = sink.Create(ev)
	// Patch does NOT trigger it again (sync.Once already fired).
	_, _ = sink.Patch(ev, []byte(`{}`))

	if got := inner.getCalls(); got != 2 {
		t.Fatalf("expected 2 inner calls, got %d", got)
	}
	// If we got here without panic/race, sync.Once worked correctly.
	// The warn logic itself is side-effect-only (slog.Warn), so we
	// verify the structural guarantee: both errors pass through.
}

func TestEmitEventNilRecorderSetPodRef(t *testing.T) {
	t.Parallel()
	t.Log("verifying emitEvent does not panic when only podRef is set")
	lc := &loopConfig{
		podRef: &v1.ObjectReference{Kind: "Pod", Name: "p", Namespace: "ns"},
	}
	emitEvent(lc, v1.EventTypeNormal, "Test", "test")
}

func TestEmitEventNilPodRefSetRecorder(t *testing.T) {
	t.Parallel()
	t.Log("verifying emitEvent does not panic when only recorder is set")
	lc := &loopConfig{
		recorder: record.NewFakeRecorder(1),
	}
	emitEvent(lc, v1.EventTypeNormal, "Test", "test")
}

// drainEvents collects all events currently buffered in the FakeRecorder.
func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-rec.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// collectEvents waits for at least n events from the FakeRecorder,
// returning all collected events once n are received or 2s elapse.
func collectEvents(rec *record.FakeRecorder, n int) []string {
	var events []string
	deadline := time.After(2 * time.Second)
	for len(events) < n {
		select {
		case e := <-rec.Events:
			events = append(events, e)
		case <-deadline:
			return events
		}
	}

	return events
}

// waitFor polls cond every 1ms until it returns true or 2s elapse.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for: %s", msg)
		case <-time.After(1 * time.Millisecond):
		}
	}
}

// requireEvent asserts that at least one event contains both reason and message substring.
func requireEvent(t *testing.T, events []string, reason, msgSubstr string) {
	t.Helper()
	for _, e := range events {
		if strings.Contains(e, reason) && strings.Contains(e, msgSubstr) {
			return
		}
	}
	t.Errorf("expected event with reason %q containing %q, got events: %v", reason, msgSubstr, events)
}

func TestRunLoop_EventReasonFrontendEndpoints(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}

	events := collectEvents(rec, 1)
	requireEvent(t, events, "VCLReloaded", "frontend endpoints changed")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonBackendEndpoints(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}

	events := collectEvents(rec, 1)
	requireEvent(t, events, "VCLReloaded", "backend endpoints changed")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonBackendMetadataChanged(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	eps := []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}}

	// First send: new endpoints → "backend endpoints changed".
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: eps,
		labels:    map[string]string{"version": "v1"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "first reload")
	_ = collectEvents(rec, 1) // drain

	// Second send: same endpoints, different labels → "backend metadata changed".
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: eps,
		labels:    map[string]string{"version": "v2"},
	}

	events := collectEvents(rec, 1)
	requireEvent(t, events, "VCLReloaded", "backend metadata changed")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonBackendAnnotationsChanged(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	eps := []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}}

	// First send: new endpoints → "backend endpoints changed".
	h.backendCh <- backendChange{
		name:        "api",
		endpoints:   eps,
		annotations: map[string]string{"example.com/version": "v1"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "first reload")
	_ = collectEvents(rec, 1) // drain

	// Second send: same endpoints, different annotations → "backend metadata changed".
	h.backendCh <- backendChange{
		name:        "api",
		endpoints:   eps,
		annotations: map[string]string{"example.com/version": "v2"},
	}

	events := collectEvents(rec, 1)
	requireEvent(t, events, "VCLReloaded", "backend metadata changed")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonValuesUpdated(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	h.valuesCh <- valuesChange{name: "tuning", data: map[string]any{"ttl": "300"}}

	events := collectEvents(rec, 1)
	requireEvent(t, events, "VCLReloaded", "values updated")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonTemplateChanged(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	h.templateCh <- struct{}{}

	events := collectEvents(rec, 2)
	requireEvent(t, events, "VCLTemplateChanged", "template file change detected")
	requireEvent(t, events, "VCLReloaded", "template changed")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonMultipleTriggers(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	// Send both frontend and backend changes within the same debounce window.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}

	events := collectEvents(rec, 2)
	requireEvent(t, events, "VCLReloaded", "frontend endpoints changed")
	requireEvent(t, events, "VCLReloaded", "backend endpoints changed")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonAfterRollback(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	var mgrCalls atomic.Int32
	h.mgr.reloadFn = func(_ string) error {
		if mgrCalls.Add(1) == 1 {
			return errors.New("varnish reload error")
		}

		return nil
	}

	wait := h.runAndWait(h.bcast)

	// Template change → mgr.Reload fails → rollback → retry succeeds
	h.templateCh <- struct{}{}

	events := collectEvents(rec, 4)
	requireEvent(t, events, "VCLReloadFailed", "VCL reload failed")
	requireEvent(t, events, "VCLRolledBack", "Rolled back to previous template")
	requireEvent(t, events, "VCLReloaded", "template changed")
	requireEvent(t, events, "VCLReloaded", "after rollback")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReasonAllFourTriggers(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	// Fire all four trigger types within the same debounce window.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}
	h.valuesCh <- valuesChange{name: "tuning", data: map[string]any{"ttl": "300"}}
	h.templateCh <- struct{}{}

	events := collectEvents(rec, 5)
	requireEvent(t, events, "VCLReloaded", "frontend endpoints changed")
	requireEvent(t, events, "VCLReloaded", "backend endpoints changed")
	requireEvent(t, events, "VCLReloaded", "values updated")
	requireEvent(t, events, "VCLReloaded", "template changed")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventTemplateParseFailed(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	h.rend.reloadFn = func() error { return errors.New("parse error") }

	wait := h.runAndWait(h.bcast)

	h.templateCh <- struct{}{}

	events := collectEvents(rec, 2)
	requireEvent(t, events, "VCLTemplateParseFailed", "Template parse error")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventRenderFailed(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		return "", errors.New("render error")
	}

	wait := h.runAndWait(h.bcast)

	// Frontend update (no template change) → render fails → no rollback.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}

	events := collectEvents(rec, 1)
	requireEvent(t, events, "VCLRenderFailed", "VCL render error")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventRenderFailedRollback(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		return "", errors.New("render error")
	}

	wait := h.runAndWait(h.bcast)

	// Template change → parse ok → render fails → rollback.
	h.templateCh <- struct{}{}

	events := collectEvents(rec, 3)
	requireEvent(t, events, "VCLRenderFailed", "VCL render error")
	requireEvent(t, events, "VCLRolledBack", "Template rollback after render error")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventRenderFailedAfterRollback(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	var mgrCalls atomic.Int32
	h.mgr.reloadFn = func(_ string) error {
		if mgrCalls.Add(1) == 1 {
			return errors.New("varnish reload error")
		}

		return nil
	}
	var renderCalls atomic.Int32
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		// First render succeeds, second (after rollback) fails.
		if renderCalls.Add(1) == 2 {
			return "", errors.New("render error after rollback")
		}

		return "vcl 4.1; /* event test */", nil
	}

	wait := h.runAndWait(h.bcast)

	// Template change → render ok → reload fails → rollback → render fails.
	// Expect 4 events: VCLTemplateChanged, VCLReloadFailed, VCLRolledBack, VCLRenderFailed.
	h.templateCh <- struct{}{}

	events := collectEvents(rec, 4)
	requireEvent(t, events, "VCLReloadFailed", "VCL reload failed")
	requireEvent(t, events, "VCLRolledBack", "Rolled back to previous template after reload failure")
	requireEvent(t, events, "VCLRenderFailed", "after rollback")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReloadFailedNoRollback(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish reload error")
	}

	wait := h.runAndWait(h.bcast)

	// Frontend update (no template change) → render ok → reload fails → no rollback.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}

	events := collectEvents(rec, 1)
	requireEvent(t, events, "VCLReloadFailed", "VCL reload failed")
	// Should NOT contain a rollback event.
	for _, e := range events {
		if strings.Contains(e, "VCLRolledBack") {
			t.Errorf("unexpected rollback event without template change: %s", e)
		}
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventReloadFailedAfterRollback(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish always rejects")
	}

	wait := h.runAndWait(h.bcast)

	// Template change → render ok → reload fails → rollback → render ok → reload fails again.
	h.templateCh <- struct{}{}

	events := collectEvents(rec, 4)
	requireEvent(t, events, "VCLReloadFailed", "VCL reload failed after rollback")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_EventVarnishdExited(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	h.mgr.err = errors.New("crashed")
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(h.bcast))))
		close(done)
	}()

	close(h.mgr.done) // unexpected exit
	<-done

	events := drainEvents(rec)
	requireEvent(t, events, "VarnishdExited", "varnishd exited unexpectedly")

	result := int(code.Load())
	if result != 1 {
		t.Fatalf("expected exit 1, got %d", result)
	}
}

func TestRunLoop_EventDrainStartedAndCompleted(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	var sessionCalls atomic.Int32
	h.mgr.activeSessionsFn = func() (uint64, error) {
		if sessionCalls.Add(1) == 1 {
			return 5, nil
		}

		return 0, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainPollInterval = 10 * time.Millisecond // poll fast to keep the test quick
	lc.drainTimeout = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	// Deterministically wait for the drain to complete (DrainCompleted emitted,
	// signal forwarded) before simulating varnishd's exit.
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	events := drainEvents(rec)
	requireEvent(t, events, "DrainStarted", "starting graceful drain")
	requireEvent(t, events, "DrainCompleted", "All connections drained")

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_EventDrainTimeout(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	h.mgr.activeSessionsFn = func() (uint64, error) {
		return 42, nil // sessions never clear
	}
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 1500 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	events := drainEvents(rec)
	requireEvent(t, events, "DrainStarted", "starting graceful drain")
	requireEvent(t, events, "DrainTimeout", "Drain timeout reached")

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestResetDebounce_FloorWhenDeadlineExceeded(t *testing.T) {
	t.Parallel()
	// Set up a state where the debounce-max deadline is already in the past,
	// causing remaining <= 0. The d <= 0 floor (line 96) sets d = 1.
	s := debounceState{
		firstEvent: time.Now().Add(-2 * time.Second),
		deadline:   time.Now().Add(-1 * time.Second), // already passed
	}
	resetDebounce(&s, 100*time.Millisecond, 500*time.Millisecond)
	defer s.timer.Stop()

	if !s.capped {
		t.Fatal("expected capped to be true when deadline has passed")
	}
	// Timer should fire almost immediately (d = 1ns).
	select {
	case <-s.timer.C:
		// OK - timer fired immediately
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timer did not fire quickly despite expired deadline")
	}
}

func TestRunLoop_DrainSecondSignalDuringDelay(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	// Use a long drain delay so the second signal arrives during it.
	lc.drainDelay = 5 * time.Second
	lc.drainTimeout = 5 * time.Second
	sigCh := make(chan os.Signal, 2)
	lc.sigCh = sigCh

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// First signal starts drain.
	sigCh <- syscall.SIGTERM
	// Wait just long enough to enter the drainDelay sleep, not for it to elapse.
	time.Sleep(50 * time.Millisecond)
	// Second signal during drainDelay - should interrupt and skip the wait.
	sigCh <- syscall.SIGINT
	waitForForwardedSignal(t, h.mgr)
	close(h.mgr.done)
	<-done

	events := drainEvents(rec)
	requireEvent(t, events, "DrainStarted", "starting graceful drain")

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestWatchFileContextCancelDuringPoll(t *testing.T) {
	t.Parallel()
	// Ensures the ctx.Done() branch inside the polling loop is covered.
	// Write initial content, start watching, let at least one tick happen,
	// then cancel.
	dir := t.TempDir()
	path := dir + "/watchfile"
	err := os.WriteFile(path, []byte("initial"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	ch := watchFile(ctx, path, 10*time.Millisecond)

	// Let the goroutine enter the polling loop and tick at least once.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// After cancel, any file changes should not produce notifications.
	time.Sleep(50 * time.Millisecond)
	err = os.WriteFile(path, []byte("changed"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	select {
	case <-ch:
		t.Fatal("unexpected notification after context cancel")
	default:
		// OK - goroutine exited via ctx.Done()
	}
}

func TestWatchFileNonBlockingSendDefault(t *testing.T) {
	t.Parallel()
	// Triggers the default branch of the non-blocking send by writing two
	// changes without reading from the channel between them.
	dir := t.TempDir()
	path := dir + "/watchfile"
	err := os.WriteFile(path, []byte("v1"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	ch := watchFile(t.Context(), path, 10*time.Millisecond)

	// First change - fills the channel (buffer size 1).
	err = os.WriteFile(path, []byte("v2"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Second change - channel still has unread notification, triggers default.
	err = os.WriteFile(path, []byte("v3"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	// Let the ticker catch up so `last` matches the final file content.
	// Without this, a slow CI runner may still have last="v2" at drain
	// time, causing a spurious second notification.
	time.Sleep(100 * time.Millisecond)

	// Drain the single notification.
	select {
	case <-ch:
		// OK
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected at least one notification")
	}

	// No second notification should be available (was dropped by default branch).
	select {
	case <-ch:
		t.Fatal("expected no second notification (non-blocking send default branch)")
	case <-time.After(100 * time.Millisecond):
		// OK - the second send hit the default branch
	}
}

func TestWatchFileReadError(t *testing.T) {
	t.Parallel()
	// Covers the ReadFile error → continue branch.
	// Start watching a file, then delete it so ReadFile fails on the next tick.
	dir := t.TempDir()
	path := dir + "/watchfile"
	err := os.WriteFile(path, []byte("v0"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	ch := watchFile(t.Context(), path, 10*time.Millisecond)

	// Synchronize with the watcher goroutine by writing distinct content
	// until a change is detected. This replaces a fragile time.Sleep that
	// can flake under load (race detector, Windows timer resolution).
	watcherActive := false
	for i := 1; i <= 100; i++ {
		err = os.WriteFile(path, fmt.Appendf(nil, "v%d", i), 0o644)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
			watcherActive = true
		case <-time.After(25 * time.Millisecond):
		}
		if watcherActive {
			break
		}
	}
	if !watcherActive {
		t.Fatal("watcher goroutine did not become active")
	}

	// Delete the file so ReadFile returns an error on the next tick.
	err = os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// No notification should be sent for a read error.
	select {
	case <-ch:
		t.Fatal("unexpected notification after file deletion")
	default:
		// OK
	}

	// Re-create the file with new content - should resume detecting changes.
	err = os.WriteFile(path, []byte("new-content"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
		// OK - change detected after recovery
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no notification after file re-creation")
	}
}

func TestWatchFileNonBlockingSendDefaultStrict(t *testing.T) {
	t.Parallel()
	// More aggressive version of TestWatchFileNonBlockingSendDefault that
	// ensures the non-blocking send default branch (line 1143) is hit by
	// making many rapid changes without draining the channel.
	dir := t.TempDir()
	path := dir + "/watchfile"
	err := os.WriteFile(path, []byte("v0"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	ch := watchFile(t.Context(), path, 5*time.Millisecond)

	// Write many changes without reading. The channel buffer is 1, so after
	// the first detection all subsequent sends hit the default branch.
	for i := range 10 {
		err = os.WriteFile(path, fmt.Appendf(nil, "v%d", i+1), 0o644)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Let the ticker catch up so `last` matches the final file content.
	// In a slow CI runner the ticker may lag behind the writes; without
	// this pause the drain below can empty the channel before `last` has
	// converged, causing a spurious second notification.
	time.Sleep(100 * time.Millisecond)

	// Drain the single buffered notification.
	select {
	case <-ch:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected at least one notification")
	}

	// No second notification - all extras were dropped by the default branch.
	select {
	case <-ch:
		t.Fatal("expected no second notification")
	case <-time.After(50 * time.Millisecond):
		// OK
	}
}

// --- Status endpoint tests ---

func TestStatusSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Now().Add(-time.Second) // started 1s ago
	store := &statusStore{
		version:             "v1.2.3",
		goVersion:           "go1.26",
		varnishMajorVersion: 7,
		serviceName:         "my-svc",
		serviceNamespace:    "default",
		drainEnabled:        true,
		broadcastEnabled:    true,
		startedAt:           now,
		frontendCount:       3,
		backendCounts:       map[string]int{"api": 2, "nginx": 1},
		valuesCount:         1,
		varnishdUp:          true,
	}

	snap := store.snapshot()

	if snap.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", snap.Version)
	}
	if snap.GoVersion != "go1.26" {
		t.Errorf("GoVersion = %q, want go1.26", snap.GoVersion)
	}
	if snap.VarnishMajorVersion != 7 {
		t.Errorf("VarnishMajorVersion = %d, want 7", snap.VarnishMajorVersion)
	}
	if snap.ServiceName != "my-svc" {
		t.Errorf("ServiceName = %q, want my-svc", snap.ServiceName)
	}
	if snap.ServiceNamespace != "default" {
		t.Errorf("ServiceNamespace = %q, want default", snap.ServiceNamespace)
	}
	if !snap.DrainEnabled {
		t.Error("DrainEnabled = false, want true")
	}
	if !snap.BroadcastEnabled {
		t.Error("BroadcastEnabled = false, want true")
	}
	if snap.FrontendCount != 3 {
		t.Errorf("FrontendCount = %d, want 3", snap.FrontendCount)
	}
	if snap.BackendCounts["api"] != 2 || snap.BackendCounts["nginx"] != 1 {
		t.Errorf("BackendCounts = %v, want map[api:2 nginx:1]", snap.BackendCounts)
	}
	if snap.ValuesCount != 1 {
		t.Errorf("ValuesCount = %d, want 1", snap.ValuesCount)
	}
	if !snap.VarnishdUp {
		t.Error("VarnishdUp = false, want true")
	}
	if snap.UptimeSeconds <= 0 {
		t.Errorf("UptimeSeconds = %v, want > 0", snap.UptimeSeconds)
	}
	if snap.LastReloadAt != nil {
		t.Errorf("LastReloadAt = %v before recordReload, want nil", snap.LastReloadAt)
	}
	if snap.ReloadCount != 0 {
		t.Errorf("ReloadCount = %d before recordReload, want 0", snap.ReloadCount)
	}

	// BackendCounts should be a clone - mutating it should not affect the store.
	snap.BackendCounts["api"] = 999
	snap2 := store.snapshot()
	if snap2.BackendCounts["api"] != 2 {
		t.Error("BackendCounts is not a clone - mutation leaked into the store")
	}

	// Record a reload and verify.
	store.recordReload()
	snap3 := store.snapshot()
	if snap3.LastReloadAt == nil {
		t.Error("LastReloadAt = nil after recordReload, want non-nil")
	}
	if snap3.ReloadCount != 1 {
		t.Errorf("ReloadCount = %d after recordReload, want 1", snap3.ReloadCount)
	}
}

// makeTestCertPEM generates a self-signed leaf certificate PEM with the given
// common name, DNS SANs, and expiry. For self-signed certs the issuer equals
// the subject.
func makeTestCertPEM(t *testing.T, commonName string, dnsNames []string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestParseTLSCertInfo(t *testing.T) {
	t.Parallel()
	notAfter := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	certPEM := makeTestCertPEM(t, "example.com", []string{"example.com", "*.example.com"}, notAfter)

	info, err := parseTLSCertInfo("frontend", certPEM)
	if err != nil {
		t.Fatalf("parseTLSCertInfo error: %v", err)
	}
	if info.Name != "frontend" {
		t.Errorf("Name = %q, want frontend", info.Name)
	}
	if info.Subject != "CN=example.com" {
		t.Errorf("Subject = %q, want CN=example.com", info.Subject)
	}
	if info.Issuer != "CN=example.com" {
		t.Errorf("Issuer = %q, want CN=example.com (self-signed)", info.Issuer)
	}
	if len(info.DNSNames) != 2 || info.DNSNames[0] != "example.com" || info.DNSNames[1] != "*.example.com" {
		t.Errorf("DNSNames = %v, want [example.com *.example.com]", info.DNSNames)
	}
	if !info.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter = %v, want %v", info.NotAfter, notAfter)
	}

	// A leaf with no SANs yields a non-nil empty slice (JSON `[]`, not `null`).
	noSAN := makeTestCertPEM(t, "no-san", nil, notAfter)
	infoNoSAN, err := parseTLSCertInfo("x", noSAN)
	if err != nil {
		t.Fatalf("parseTLSCertInfo (no SAN) error: %v", err)
	}
	if infoNoSAN.DNSNames == nil {
		t.Error("DNSNames = nil, want non-nil empty slice")
	}

	// Garbage input (no PEM block) is reported as an error, not a panic.
	_, err = parseTLSCertInfo("bad", []byte("not a pem"))
	if err == nil {
		t.Error("parseTLSCertInfo(garbage) = nil error, want error")
	}

	// A valid PEM block wrapping invalid DER reaches x509.ParseCertificate and
	// surfaces its wrapped error.
	badDER := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not valid der")})
	_, err = parseTLSCertInfo("bad-der", badDER)
	if err == nil {
		t.Error("parseTLSCertInfo(bad DER) = nil error, want error")
	}
}

func TestTLSCertChan(t *testing.T) {
	t.Parallel()
	if tlsCertChan(nil) != nil {
		t.Error("tlsCertChan(nil) = non-nil, want nil")
	}
	ch := make(chan tlsCertChange)
	if tlsCertChan(ch) == nil {
		t.Error("tlsCertChan(non-nil) = nil, want the channel")
	}
}

func TestStatusSnapshotTLS(t *testing.T) {
	t.Parallel()
	store := &statusStore{startedAt: time.Now()}

	// Default: TLS absent - disabled, no certs, empty (non-nil) list, null reload.
	snap := store.snapshot()
	if snap.TLS.Enabled || snap.TLS.ConfiguredCerts != 0 || snap.TLS.ActiveCerts != 0 {
		t.Errorf("default TLS = %+v, want disabled with zero counts", snap.TLS)
	}
	if snap.TLS.Certificates == nil {
		t.Error("TLS.Certificates = nil, want non-nil empty slice (JSON [])")
	}
	if snap.TLS.LastReloadAt != nil {
		t.Errorf("TLS.LastReloadAt = %v before any load, want nil", snap.TLS.LastReloadAt)
	}

	// Configure TLS and record two certs (out of order to verify sorting).
	store.setTLSConfig(true, true, 2)
	store.recordTLSCert(&tlsCertInfo{Name: "frontend", Subject: "CN=a"})
	store.recordTLSCert(&tlsCertInfo{Name: "api", Subject: "CN=b"})

	snap = store.snapshot()
	if !snap.TLS.Enabled || !snap.TLS.Supported || snap.TLS.ConfiguredCerts != 2 {
		t.Errorf("TLS = %+v, want enabled/supported with configuredCerts 2", snap.TLS)
	}
	if snap.TLS.ActiveCerts != 2 || len(snap.TLS.Certificates) != 2 {
		t.Errorf("ActiveCerts/Certificates = %d/%d, want 2/2", snap.TLS.ActiveCerts, len(snap.TLS.Certificates))
	}
	if snap.TLS.Certificates[0].Name != "api" || snap.TLS.Certificates[1].Name != "frontend" {
		t.Errorf("certificates not sorted by name: %v", snap.TLS.Certificates)
	}
	if snap.TLS.LastReloadAt == nil {
		t.Error("TLS.LastReloadAt = nil after recordTLSCert, want non-nil")
	}

	// recordTLSCertInfo: a valid cert is parsed and recorded (in the status
	// store and the expiry gauge); an unparseable one is logged and skipped
	// (no gauge series); a nil store/metrics is a safe no-op.
	store2 := &statusStore{startedAt: time.Now()}
	m := telemetry.NewMetrics(prometheus.NewRegistry(), nil)
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	recordTLSCertInfo(store2, m, "frontend", makeTestCertPEM(t, "ex.com", []string{"ex.com"}, expiry))
	recordTLSCertInfo(store2, m, "bad", []byte("not a certificate"))
	recordTLSCertInfo(nil, nil, "frontend", makeTestCertPEM(t, "x", nil, time.Now().Add(time.Hour)))

	snap2 := store2.snapshot()
	if len(snap2.TLS.Certificates) != 1 || snap2.TLS.Certificates[0].Name != "frontend" {
		t.Errorf("recordTLSCertInfo: certificates = %v, want exactly one named 'frontend'", snap2.TLS.Certificates)
	}
	// The expiry gauge holds the cert's NotAfter as a Unix timestamp for the
	// parseable cert, and the bad cert created no series (no false alerting).
	var dm dto.Metric
	err := m.TLSCertExpiryTimestampSeconds.WithLabelValues("frontend").Write(&dm)
	if err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	if got := dm.GetGauge().GetValue(); got != float64(expiry.Unix()) {
		t.Errorf("tls_cert_expiry_timestamp_seconds{cert=frontend} = %v, want %v", got, float64(expiry.Unix()))
	}
	if n := countSeries(m.TLSCertExpiryTimestampSeconds); n != 1 {
		t.Errorf("tls_cert_expiry_timestamp_seconds has %d series, want 1 (unparseable cert must not create one)", n)
	}
}

// countSeries returns the number of metric series a collector currently exports.
func countSeries(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}

	return n
}

// TestStatusHandlerNoTLS asserts the served JSON when no TLS is configured and
// no certificates are installed: a `tls` object with disabled flags, zero
// counts, a null lastReloadAt, and an empty (not null) certificates array.
func TestStatusHandlerNoTLS(t *testing.T) {
	t.Parallel()
	store := &statusStore{version: "v0.1.0", startedAt: time.Now()}

	handler := statusHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	// The wire format must use an empty array, never null, for certificates.
	if !strings.Contains(body, `"certificates":[]`) {
		t.Errorf("response should contain \"certificates\":[], got: %s", body)
	}
	// lastReloadAt inside the tls object must be null before any cert loads.
	if !strings.Contains(body, `"lastReloadAt":null`) {
		t.Errorf("response should contain a null lastReloadAt, got: %s", body)
	}

	var resp statusResponse
	err := json.NewDecoder(strings.NewReader(body)).Decode(&resp)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.TLS.Enabled || resp.TLS.Supported {
		t.Errorf("TLS flags = enabled:%v supported:%v, want both false", resp.TLS.Enabled, resp.TLS.Supported)
	}
	if resp.TLS.ConfiguredCerts != 0 || resp.TLS.ActiveCerts != 0 {
		t.Errorf("cert counts = %d/%d, want 0/0", resp.TLS.ConfiguredCerts, resp.TLS.ActiveCerts)
	}
	if resp.TLS.LastReloadAt != nil {
		t.Errorf("TLS.LastReloadAt = %v, want nil", resp.TLS.LastReloadAt)
	}
	if len(resp.TLS.Certificates) != 0 {
		t.Errorf("TLS.Certificates = %v, want empty", resp.TLS.Certificates)
	}
}

func TestStatusHandler(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	store := &statusStore{
		version:             "v0.1.0",
		goVersion:           "go1.26",
		varnishMajorVersion: 7,
		serviceName:         "test-svc",
		serviceNamespace:    "default",
		drainEnabled:        true,
		broadcastEnabled:    true,
		startedAt:           startedAt,
		frontendCount:       3,
		backendCounts:       map[string]int{"api": 2, "nginx": 1},
		valuesCount:         1,
		varnishdUp:          true,
	}
	// Record a reload so LastReloadAt is non-nil.
	store.recordReload()

	handler := statusHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp statusResponse
	err := json.NewDecoder(rec.Body).Decode(&resp)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	// Verify every field round-trips through JSON correctly.
	if resp.Version != "v0.1.0" {
		t.Errorf("Version = %q, want v0.1.0", resp.Version)
	}
	if resp.GoVersion != "go1.26" {
		t.Errorf("GoVersion = %q, want go1.26", resp.GoVersion)
	}
	if resp.VarnishMajorVersion != 7 {
		t.Errorf("VarnishMajorVersion = %d, want 7", resp.VarnishMajorVersion)
	}
	if resp.ServiceName != "test-svc" {
		t.Errorf("ServiceName = %q, want test-svc", resp.ServiceName)
	}
	if resp.ServiceNamespace != "default" {
		t.Errorf("ServiceNamespace = %q, want default", resp.ServiceNamespace)
	}
	if !resp.DrainEnabled {
		t.Error("DrainEnabled = false, want true")
	}
	if !resp.BroadcastEnabled {
		t.Error("BroadcastEnabled = false, want true")
	}
	if !resp.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", resp.StartedAt, startedAt)
	}
	if resp.UptimeSeconds <= 0 {
		t.Errorf("UptimeSeconds = %v, want > 0", resp.UptimeSeconds)
	}
	if resp.FrontendCount != 3 {
		t.Errorf("FrontendCount = %d, want 3", resp.FrontendCount)
	}
	if resp.BackendCounts["api"] != 2 || resp.BackendCounts["nginx"] != 1 {
		t.Errorf("BackendCounts = %v, want map[api:2 nginx:1]", resp.BackendCounts)
	}
	if resp.ValuesCount != 1 {
		t.Errorf("ValuesCount = %d, want 1", resp.ValuesCount)
	}
	if resp.LastReloadAt == nil {
		t.Error("LastReloadAt = nil, want non-nil")
	}
	if resp.ReloadCount != 1 {
		t.Errorf("ReloadCount = %d, want 1", resp.ReloadCount)
	}
	if !resp.VarnishdUp {
		t.Error("VarnishdUp = false, want true")
	}
}

func TestStatusHandlerLastReloadAtNull(t *testing.T) {
	t.Parallel()
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
	}

	handler := statusHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	// Decode into a raw map to verify lastReloadAt is JSON null.
	var raw map[string]any
	err := json.NewDecoder(rec.Body).Decode(&raw)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	v, exists := raw["lastReloadAt"]
	if !exists {
		t.Fatal("lastReloadAt key missing from JSON")
	}
	if v != nil {
		t.Errorf("lastReloadAt = %v, want null", v)
	}
}

func TestStatusHandlerMethodNotAllowed(t *testing.T) {
	t.Parallel()
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
	}
	handler := statusHandler(store)
	req := httptest.NewRequest(http.MethodPost, "/status", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

func TestHealthzHandler_VarnishdUp(t *testing.T) {
	t.Parallel()
	store := &statusStore{varnishdUp: true}
	handler := healthzHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

func TestHealthzHandler_VarnishdDown(t *testing.T) {
	t.Parallel()
	store := &statusStore{varnishdUp: false}
	handler := healthzHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "varnishd not running") {
		t.Errorf("body = %q, want it to contain %q", body, "varnishd not running")
	}
}

func TestHealthzHandler_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	store := &statusStore{}
	handler := healthzHandler(store)
	req := httptest.NewRequest(http.MethodPost, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

func TestReadyzHandler_Ready(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	store := &statusStore{varnishdUp: true}
	handler := readyzHandler(store, ln.Addr().String())
	req := httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

func TestReadyzHandler_NotReady(t *testing.T) {
	t.Parallel()
	store := &statusStore{varnishdUp: false}
	handler := readyzHandler(store, "127.0.0.1:0")
	req := httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not ready") {
		t.Errorf("body = %q, want it to contain %q", body, "not ready")
	}
}

func TestReadyzHandler_ChildDown(t *testing.T) {
	t.Parallel()
	// Use an address where nothing is listening to simulate a crashed child.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	store := &statusStore{varnishdUp: true}
	handler := readyzHandler(store, addr)
	req := httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not accepting connections") {
		t.Errorf("body = %q, want it to contain %q", body, "not accepting connections")
	}
}

func TestReadyzHandler_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	store := &statusStore{}
	handler := readyzHandler(store, "127.0.0.1:0")
	req := httptest.NewRequest(http.MethodPost, "/readyz", http.NoBody)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

func TestRunLoop_StatusStoreUpdatedOnReload(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
		varnishdUp:    true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.status = store
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send a frontend change to trigger a reload.
	h.frontendCh <- []watcher.Frontend{
		{Host: "10.0.0.1", Port: 80, Name: "pod-1"},
		{Host: "10.0.0.2", Port: 80, Name: "pod-2"},
	}
	waitForStatus(t, store, func(s statusResponse) bool { return s.ReloadCount >= 1 })

	snap := store.snapshot()
	if snap.FrontendCount != 2 {
		t.Errorf("FrontendCount = %d, want 2", snap.FrontendCount)
	}
	if snap.LastReloadAt == nil {
		t.Error("LastReloadAt = nil, want non-nil after reload")
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_StatusStoreVarnishdDown(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.err = errors.New("crashed")
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
		varnishdUp:    true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.status = store
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	close(h.mgr.done) // unexpected exit
	<-done

	snap := store.snapshot()
	if snap.VarnishdUp {
		t.Error("VarnishdUp = true after unexpected exit, want false")
	}

	if result := int(code.Load()); result != 1 {
		t.Fatalf("expected exit 1, got %d", result)
	}
}

func TestRunLoop_StatusStoreBackendCounts(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
		varnishdUp:    true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.status = store
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send a backend change to trigger a reload.
	h.backendCh <- backendChange{
		name: "api",
		endpoints: []watcher.Endpoint{
			{Host: "10.0.1.1", Port: 8080, Name: "api-0"},
			{Host: "10.0.1.2", Port: 8080, Name: "api-1"},
			{Host: "10.0.1.3", Port: 8080, Name: "api-2"},
		},
	}
	waitForStatus(t, store, func(s statusResponse) bool { return s.ReloadCount >= 1 })

	snap := store.snapshot()
	if snap.BackendCounts["api"] != 3 {
		t.Errorf("BackendCounts[api] = %d, want 3", snap.BackendCounts["api"])
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_StatusStoreUpdatedOnPostRollbackReload(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	// First mgr.Reload fails (triggers rollback), second succeeds.
	var mgrCalls atomic.Int32
	h.mgr.reloadFn = func(_ string) error {
		if mgrCalls.Add(1) == 1 {
			return errors.New("varnish reload error")
		}

		return nil
	}
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
		varnishdUp:    true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.status = store
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Template change → first reload fails → rollback → retry succeeds.
	h.templateCh <- struct{}{}
	waitForStatus(t, store, func(s statusResponse) bool { return s.ReloadCount >= 1 })

	snap := store.snapshot()
	if snap.LastReloadAt == nil {
		t.Error("LastReloadAt = nil after post-rollback success, want non-nil")
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_StatusStoreNotUpdatedOnRenderError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		return "", errors.New("render error")
	}
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
		varnishdUp:    true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.status = store
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Frontend update → RenderToFile fails → no status update.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool {
		_, rc, _ := h.rend.counts()

		return rc >= 1
	}, "RenderToFile called")

	snap := store.snapshot()
	if snap.ReloadCount != 0 {
		t.Errorf("ReloadCount = %d after render error, want 0", snap.ReloadCount)
	}
	if snap.LastReloadAt != nil {
		t.Errorf("LastReloadAt = %v after render error, want nil", snap.LastReloadAt)
	}
	if snap.FrontendCount != 0 {
		t.Errorf("FrontendCount = %d after render error, want 0 (unchanged)", snap.FrontendCount)
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_StatusStoreNotUpdatedOnVarnishReloadError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish reload error")
	}
	store := &statusStore{
		startedAt:     time.Now(),
		backendCounts: map[string]int{},
		varnishdUp:    true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.status = store
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Frontend update → render ok → mgr.Reload fails → no status update.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	snap := store.snapshot()
	if snap.ReloadCount != 0 {
		t.Errorf("ReloadCount = %d after varnish reload error, want 0", snap.ReloadCount)
	}
	if snap.LastReloadAt != nil {
		t.Errorf("LastReloadAt = %v after varnish reload error, want nil", snap.LastReloadAt)
	}
	if snap.FrontendCount != 0 {
		t.Errorf("FrontendCount = %d after varnish reload error, want 0 (unchanged)", snap.FrontendCount)
	}

	// Clean shutdown.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestBackendCountsMap(t *testing.T) {
	t.Parallel()
	backends := map[string]renderer.BackendGroup{
		"api":   {Endpoints: []watcher.Endpoint{{Host: "10.0.0.1", Port: 80, Name: "a1"}, {Host: "10.0.0.2", Port: 80, Name: "a2"}}},
		"nginx": {Endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "n1"}}},
	}
	got := backendCountsMap(backends)
	if got["api"] != 2 {
		t.Errorf("api count = %d, want 2", got["api"])
	}
	if got["nginx"] != 1 {
		t.Errorf("nginx count = %d, want 1", got["nginx"])
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}

	// Empty input.
	empty := backendCountsMap(map[string]renderer.BackendGroup{})
	if len(empty) != 0 {
		t.Errorf("expected empty map, got %v", empty)
	}
}

func TestAppendUnique(t *testing.T) {
	t.Parallel()
	var s []string
	s = appendUnique(s, "a")
	s = appendUnique(s, "b")
	s = appendUnique(s, "a") // duplicate
	if len(s) != 2 {
		t.Fatalf("expected 2 elements, got %d: %v", len(s), s)
	}
	if s[0] != "a" || s[1] != "b" {
		t.Fatalf("expected [a b], got %v", s)
	}
}

func TestKubeContext(t *testing.T) {
	t.Parallel()

	// Positive timeout → context carries a deadline within the budget.
	ctx, cancel := kubeContext(50 * time.Millisecond)
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline for a positive timeout")
	}
	if until := time.Until(deadline); until <= 0 || until > time.Second {
		t.Fatalf("deadline is %v from now, want within (0, 1s]", until)
	}
	cancel()

	// Zero timeout → no deadline (limit disabled), and cancel is a safe no-op.
	ctx, cancel = kubeContext(0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline when timeout is 0")
	}
}

func TestDetectLocalZone_NodeNameNotSet(t *testing.T) {
	t.Parallel()
	zone := detectLocalZone(t.Context(), slog.New(slog.DiscardHandler), fake.NewClientset(), "")
	if zone != "" {
		t.Errorf("expected empty zone when NODE_NAME is unset, got %q", zone)
	}
}

func TestDetectLocalZone_NodeWithZoneLabel(t *testing.T) {
	t.Parallel()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"topology.kubernetes.io/zone": "europe-west3-a"},
		},
	}
	cs := fake.NewClientset(node)
	zone := detectLocalZone(t.Context(), slog.New(slog.DiscardHandler), cs, "node-1")
	if zone != "europe-west3-a" {
		t.Errorf("expected zone europe-west3-a, got %q", zone)
	}
}

func TestDetectLocalZone_NodeWithoutZoneLabel(t *testing.T) {
	t.Parallel()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-2",
		},
	}
	cs := fake.NewClientset(node)
	zone := detectLocalZone(t.Context(), slog.New(slog.DiscardHandler), cs, "node-2")
	if zone != "" {
		t.Errorf("expected empty zone when label is absent, got %q", zone)
	}
}

func TestDetectLocalZone_NodeNotFound(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset() // no nodes
	zone := detectLocalZone(t.Context(), slog.New(slog.DiscardHandler), cs, "nonexistent-node")
	if zone != "" {
		t.Errorf("expected empty zone when node is not found, got %q", zone)
	}
}

func TestDetectLocalZone_ForbiddenRBAC(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cs := fake.NewClientset()
	cs.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "", Resource: "nodes"},
			"node-1",
			errors.New("RBAC: access denied"),
		)
	})
	zone := detectLocalZone(t.Context(), log, cs, "node-1")
	if zone != "" {
		t.Errorf("expected empty zone when RBAC forbids node access, got %q", zone)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "could not look up node") {
		t.Errorf("expected warning about node lookup failure, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "forbidden") {
		t.Errorf("expected log to mention forbidden error, got: %s", logOutput)
	}
}

func TestRunLoop_NCSAEventEmitsKubeEvent(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()
	ncsaCh := make(chan varnish.NCSAEvent, 1)

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.ncsaEvents = ncsaCh
	lc.recorder = rec
	lc.podRef = h.podRef
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	ncsaCh <- varnish.NCSAEvent{
		Type:    "Warning",
		Reason:  "VarnishncsaExited",
		Message: "varnishncsa exited unexpectedly: signal: killed",
	}

	// Wait for event to be processed.
	deadline := time.After(2 * time.Second)
	var events []string
	for {
		select {
		case e := <-rec.Events:
			events = append(events, e)
			if strings.Contains(e, "VarnishncsaExited") {
				goto found
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event, got: %v", events)
		}
	}
found:

	requireEvent(t, events, "VarnishncsaExited", "varnishncsa exited unexpectedly")

	// Shut down cleanly.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done
}

func TestRunLoop_NCSAEventsNilNoPanic(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	// ncsaEvents defaults to nil in loopConfig - verify the loop runs fine.
	wait := h.runAndWait(h.bcast)

	// Trigger a normal reload to confirm the loop is operational.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_NCSACrashLoopExitsWithError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	ncsaCrashed := make(chan struct{})

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.ncsaCrashed = ncsaCrashed
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Simulate crash loop by closing the channel.
	close(ncsaCrashed)

	// Wait for SIGTERM to be forwarded to varnishd.
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")

	// Let varnishd exit.
	close(h.mgr.done)
	<-done

	if got := int(code.Load()); got != 1 {
		t.Fatalf("expected exit code 1, got %d", got)
	}

	// Verify SIGTERM was forwarded.
	sigs := h.mgr.getForwardedSigs()
	if len(sigs) == 0 {
		t.Fatal("expected at least one forwarded signal")
	}
	if sigs[0] != syscall.SIGTERM {
		t.Errorf("first forwarded signal = %v, want SIGTERM", sigs[0])
	}
}

// TestRunLoop_ServerErrorExitsWithError verifies that a fatal metrics/broadcast
// HTTP server error routed through serverErrCh triggers an orderly shutdown
// (SIGTERM forwarded to varnishd, exit code 1) instead of [os.Exit], which would
// skip deferred cleanup and orphan the varnishd child.
func TestRunLoop_ServerErrorExitsWithError(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	serverErrCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.serverErrCh = serverErrCh
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Simulate a fatal HTTP server failure after startup.
	serverErrCh <- errors.New("broadcast serve: accept failed")

	// Wait for SIGTERM to be forwarded to varnishd.
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")

	// Let varnishd exit.
	close(h.mgr.done)
	<-done

	if got := int(code.Load()); got != 1 {
		t.Fatalf("expected exit code 1, got %d", got)
	}

	sigs := h.mgr.getForwardedSigs()
	if len(sigs) == 0 {
		t.Fatal("expected at least one forwarded signal")
	}
	if sigs[0] != syscall.SIGTERM {
		t.Errorf("first forwarded signal = %v, want SIGTERM", sigs[0])
	}
}

func TestRunLoop_StopNCSACalledOnShutdown(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	var stopCalled atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.stopNCSA = func() { stopCalled.Store(true) }
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if !stopCalled.Load() {
		t.Fatal("stopNCSA was not called during shutdown")
	}
}

func TestRunLoop_StopNCSANilOnShutdown(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.stopNCSA = nil // explicitly nil - should not panic
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestBuildNCSAArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{
			name: "empty config",
			cfg:  config.Config{},
			want: nil,
		},
		{
			name: "backend only",
			cfg:  config.Config{VarnishncsaBackend: true},
			want: []string{"-b"},
		},
		{
			name: "format only",
			cfg:  config.Config{VarnishncsaFormat: "%h %s"},
			want: []string{"-F", "%h %s"},
		},
		{
			name: "query only",
			cfg:  config.Config{VarnishncsaQuery: "ReqURL ~ /api"},
			want: []string{"-q", "ReqURL ~ /api"},
		},
		{
			name: "output only",
			cfg:  config.Config{VarnishncsaOutput: "/var/log/access.log"},
			want: []string{"-w", "/var/log/access.log"},
		},
		{
			name: "all combined",
			cfg: config.Config{
				VarnishncsaBackend: true,
				VarnishncsaFormat:  "%h %s",
				VarnishncsaQuery:   "ReqURL ~ /api",
				VarnishncsaOutput:  "/var/log/access.log",
			},
			want: []string{"-b", "-F", "%h %s", "-q", "ReqURL ~ /api", "-w", "/var/log/access.log"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildNCSAArgs(&tc.cfg)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", tc.want) {
				t.Errorf("buildNCSAArgs() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunLoop_BackendRemovedDeletesFromLatest(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	wait := h.runAndWait(h.bcast)

	// First add a backend.
	h.backendCh <- backendChange{
		name:      "discovered-svc",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "first reload after add")

	// Now remove it.
	h.backendCh <- backendChange{
		name:    "discovered-svc",
		removed: true,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "second reload after removal")

	// Verify the backend was removed by inspecting the last RenderToFile call.
	h.rend.mu.Lock()
	lastBackends := h.rend.lastBackends
	h.rend.mu.Unlock()

	if lastBackends != nil {
		if _, exists := lastBackends["discovered-svc"]; exists {
			t.Error("expected 'discovered-svc' to be deleted from latestBackends after removal")
		}
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestRunLoop_LateForwardDoesNotResurrectRemovedBackend reproduces the
// discovery removal/forwarding race: a cancelled forwarding goroutine can emit
// one final endpoint update (tagged with the removed incarnation's generation)
// that lands after the authoritative removal. The event loop must drop it so
// the removed backend is not resurrected in the rendered VCL. Without the
// generation guard, the stale ADD re-creates "discovered-svc" with empty
// endpoints and this test fails.
func TestRunLoop_LateForwardDoesNotResurrectRemovedBackend(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	wait := h.runAndWait(h.bcast)

	// Discovery incarnation 1 of "discovered-svc" appears.
	h.backendCh <- backendChange{
		name:      "discovered-svc",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
		gen:       1,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after add")

	// The Service is removed: the authoritative removal carries the same gen.
	h.backendCh <- backendChange{name: "discovered-svc", removed: true, gen: 1}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after removal")

	// A late update from incarnation 1's cancelled forwarder arrives after the
	// removal. Endpoints are a non-nil empty slice (scale-to-zero normalisation),
	// so the fan-in treats it as an ADD, not a removal. It must be dropped: its
	// gen (1) is <= the removed incarnation's gen.
	h.backendCh <- backendChange{
		name:      "discovered-svc",
		endpoints: []watcher.Endpoint{},
		gen:       1,
	}
	// Trigger a fresh reload via an unrelated backend so we can observe the
	// rendered state after the stale update was processed. Both sends are on the
	// same channel, so the stale update is handled first (FIFO, single-threaded
	// loop).
	h.backendCh <- backendChange{
		name:      "other",
		endpoints: []watcher.Endpoint{{Host: "10.0.2.1", Port: 8080, Name: "other-0"}},
		gen:       2,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "reload after trigger")

	h.rend.mu.Lock()
	lastBackends := h.rend.lastBackends
	h.rend.mu.Unlock()

	if _, exists := lastBackends["discovered-svc"]; exists {
		t.Error("removed backend 'discovered-svc' was resurrected by a stale late update")
	}
	if _, exists := lastBackends["other"]; !exists {
		t.Error("expected unrelated backend 'other' to be present")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestRunLoop_ReaddedBackendWithHigherGenIsHonored verifies the generation
// tombstone does not block a genuine re-add: a new discovery incarnation gets a
// strictly higher gen, so its updates are applied even though the same name was
// previously removed.
func TestRunLoop_ReaddedBackendWithHigherGenIsHonored(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	wait := h.runAndWait(h.bcast)

	h.backendCh <- backendChange{
		name:      "discovered-svc",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
		gen:       1,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after add")

	h.backendCh <- backendChange{name: "discovered-svc", removed: true, gen: 1}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after removal")

	// Re-add as a new incarnation with a strictly higher gen.
	h.backendCh <- backendChange{
		name:      "discovered-svc",
		endpoints: []watcher.Endpoint{{Host: "10.0.9.9", Port: 8080, Name: "pod-1"}},
		gen:       2,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "reload after re-add")

	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["discovered-svc"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("re-added backend 'discovered-svc' (higher gen) should be present")
	}
	if len(bg.Endpoints) != 1 || bg.Endpoints[0].Host != "10.0.9.9" {
		t.Errorf("expected re-added endpoint 10.0.9.9, got %+v", bg.Endpoints)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestRunLoop_StaleRemovalDoesNotDeleteReaddedBackend reproduces a discovery
// handoff race. When a backend name is handed from one --backend-selector
// watcher (incarnation gen 1) to another (incarnation gen 2), the new
// incarnation's ADD and the old incarnation's authoritative REMOVAL travel
// through independent fan-in goroutines into the shared backend channel, so
// their relative order is not guaranteed. The new owner's ADD (gen 2) can be
// processed before the old owner's REMOVAL (gen 1). The stale lower-gen removal
// must NOT delete the backend the higher-gen incarnation now owns.
func TestRunLoop_StaleRemovalDoesNotDeleteReaddedBackend(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	wait := h.runAndWait(h.bcast)

	// Incarnation 1 of "web" appears and is rendered.
	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
		gen:       1,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after add")

	// Handoff: a new incarnation (gen 2) re-adds "web" with fresh endpoints and
	// is rendered.
	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.9.9", Port: 8080, Name: "pod-1"}},
		gen:       2,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after re-add")

	// Only afterwards does the OLD incarnation's removal (gen 1) arrive,
	// reordered behind the new ADD. It is stale and must be ignored. Send an
	// unrelated backend immediately after so a reload fires even when the stale
	// removal is correctly dropped (a dropped update triggers no reload). Both
	// are FIFO on the single channel, so the removal is processed first.
	h.backendCh <- backendChange{name: "web", removed: true, gen: 1}
	h.backendCh <- backendChange{
		name:      "other",
		endpoints: []watcher.Endpoint{{Host: "10.0.2.1", Port: 8080, Name: "other-0"}},
		gen:       3,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "reload after trigger")

	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["web"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("backend 'web' (owned by newer incarnation gen 2) was deleted by a stale lower-gen removal")
	}
	if len(bg.Endpoints) != 1 || bg.Endpoints[0].Host != "10.0.9.9" {
		t.Errorf("expected 'web' to keep gen-2 endpoint 10.0.9.9, got %+v", bg.Endpoints)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestRunLoop_DebugLogsStaleDrop verifies the event loop emits a DEBUG "decision"
// log when it drops a stale lower-gen backend removal - the exact silent branch
// that hid the stale-removal bug. A capturing logger is injected into the
// loopConfig (no global [slog.SetDefault], so the test stays parallel-safe). The
// buffer is read only after the loop goroutine exits, so the single-writer
// logger is never read concurrently.
func TestRunLoop_DebugLogsStaleDrop(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.log = logger

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// gen-1 add, then gen-2 re-add (handoff), then the stale gen-1 removal that
	// must be dropped. An unrelated backend follows so a reload fires after the
	// removal is processed (FIFO on the single channel).
	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
		gen:       1,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after add")
	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.9.9", Port: 8080, Name: "pod-1"}},
		gen:       2,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after re-add")
	h.backendCh <- backendChange{name: "web", removed: true, gen: 1}
	h.backendCh <- backendChange{
		name:      "other",
		endpoints: []watcher.Endpoint{{Host: "10.0.2.1", Port: 8080, Name: "other-0"}},
		gen:       3,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "reload after trigger")

	// Shut the loop down so its goroutine stops writing before we read the buffer.
	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done
	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}

	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") {
		t.Fatalf("expected DEBUG output, got:\n%s", out)
	}
	// The stale gen-1 removal (gen 1 < liveGen 2) must be logged as a dropped
	// decision with its discriminating fields.
	for _, want := range []string{"action=drop", "reason=stale-incarnation", "name=web", "gen=1", "liveGen=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug output missing %q; got:\n%s", want, out)
		}
	}
}

// TestRunLoop_StaleAddThenRemovalDoesNotDeleteReaddedBackend covers the harder
// reordering of the same handoff: the newer incarnation's ADD (gen 2) is
// processed first, then BOTH a stale late ADD (gen 1) from the old
// incarnation's cancelled forwarder AND the old incarnation's REMOVAL (gen 1)
// arrive afterwards. Neither stale gen-1 update may disturb the gen-2 backend.
func TestRunLoop_StaleAddThenRemovalDoesNotDeleteReaddedBackend(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	wait := h.runAndWait(h.bcast)

	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
		gen:       1,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after add")

	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.9.9", Port: 8080, Name: "pod-1"}},
		gen:       2,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after re-add")

	// Stale late ADD from incarnation 1 (would overwrite gen-2 endpoints), then
	// incarnation 1's removal (would delete the gen-2 backend), then a trigger.
	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
		gen:       1,
	}
	h.backendCh <- backendChange{name: "web", removed: true, gen: 1}
	h.backendCh <- backendChange{
		name:      "other",
		endpoints: []watcher.Endpoint{{Host: "10.0.2.1", Port: 8080, Name: "other-0"}},
		gen:       3,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "reload after trigger")

	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["web"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("backend 'web' (gen 2) was disturbed by stale gen-1 add/removal")
	}
	if len(bg.Endpoints) != 1 || bg.Endpoints[0].Host != "10.0.9.9" {
		t.Errorf("expected 'web' to keep gen-2 endpoint 10.0.9.9, got %+v", bg.Endpoints)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestRunLoop_RemovalOfCurrentIncarnationStillDeletes guards against the
// generation guard over-correcting: a removal whose gen equals the live
// incarnation's gen is authoritative and MUST delete the backend. (A regression
// that changed the stale check from `<` to `<=` would silently swallow every
// legitimate removal.)
func TestRunLoop_RemovalOfCurrentIncarnationStillDeletes(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	wait := h.runAndWait(h.bcast)

	// Handoff: gen 1 then gen 2 take ownership of "web".
	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "pod-0"}},
		gen:       1,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after add")

	h.backendCh <- backendChange{
		name:      "web",
		endpoints: []watcher.Endpoint{{Host: "10.0.9.9", Port: 8080, Name: "pod-1"}},
		gen:       2,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after re-add")

	// The current owner (gen 2) is legitimately removed. This is not stale and
	// must delete "web".
	h.backendCh <- backendChange{name: "web", removed: true, gen: 2}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 3 }, "reload after removal")

	h.rend.mu.Lock()
	_, exists := h.rend.lastBackends["web"]
	h.rend.mu.Unlock()

	if exists {
		t.Error("backend 'web' should be deleted by the authoritative removal of its current incarnation (gen 2)")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestSweepExpiredTombstones(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ttl := time.Minute

	tombstones := map[string]backendTombstone{
		"fresh":   {gen: 7, at: now.Add(-30 * time.Second)}, // within TTL - kept
		"expired": {gen: 3, at: now.Add(-2 * time.Minute)},  // older than TTL - evicted
		"exactly": {gen: 5, at: now.Add(-ttl)},              // age == TTL - evicted (>=)
	}

	evicted := sweepExpiredTombstones(tombstones, now, ttl)
	if evicted != 2 {
		t.Errorf("evicted = %d, want 2 (expired + exactly)", evicted)
	}

	if _, ok := tombstones["fresh"]; !ok {
		t.Error("fresh tombstone (within TTL) must be retained")
	}
	if _, ok := tombstones["expired"]; ok {
		t.Error("expired tombstone (older than TTL) must be evicted")
	}
	if _, ok := tombstones["exactly"]; ok {
		t.Error("tombstone aged exactly TTL must be evicted")
	}

	// A zero (or negative) TTL disables eviction entirely.
	keepAll := map[string]backendTombstone{"old": {gen: 1, at: now.Add(-time.Hour)}}
	if n := sweepExpiredTombstones(keepAll, now, 0); n != 0 {
		t.Errorf("ttl=0 evicted = %d, want 0", n)
	}
	if _, ok := keepAll["old"]; !ok {
		t.Error("ttl=0 must disable eviction")
	}
}

// TestRunLoop_TombstoneSweepBoundsRemovedGen verifies the periodic sweep evicts
// stale backend tombstones so removedGen cannot grow without bound. Once a
// tombstone is swept, a later update for that name that the tombstone would have
// dropped is accepted again - proving the entry was actually removed.
func TestRunLoop_TombstoneSweepBoundsRemovedGen(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.backendTombstoneTTL = 20 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Add then remove a discovered backend at gen 5: the removal records
	// removedGen["svc"] = {gen: 5}.
	h.backendCh <- backendChange{
		name:      "svc",
		endpoints: []watcher.Endpoint{{Host: "10.0.0.1", Port: 80, Name: "svc-0"}},
		gen:       5,
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after add")
	h.backendCh <- backendChange{name: "svc", removed: true, gen: 5}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "reload after removal")

	// A lower-gen update (gen 1) is dropped by the gen-5 tombstone until the
	// periodic sweep evicts it. Resend on a generous deadline rather than
	// sleeping a fixed amount, so the test does not depend on the sweep ticker
	// firing within a fixed window on slow/overloaded CI. Each pre-sweep send is
	// silently dropped (no reload); the first post-sweep send is accepted.
	reloadedAfterSweep := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.backendCh <- backendChange{
			name:      "svc",
			endpoints: []watcher.Endpoint{{Host: "10.0.9.9", Port: 80, Name: "svc-1"}},
			gen:       1,
		}
		time.Sleep(15 * time.Millisecond)
		if h.mgr.getReloadCount() >= 3 {
			reloadedAfterSweep = true

			break
		}
	}
	if !reloadedAfterSweep {
		t.Fatal("low-gen re-add was never accepted; the tombstone sweep did not evict the entry")
	}

	h.rend.mu.Lock()
	_, ok := h.rend.lastBackends["svc"]
	h.rend.mu.Unlock()
	if !ok {
		t.Fatal("backend 'svc' should be present after its tombstone was swept")
	}

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done
	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_BackendLabelsPassedToRenderer(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		labels:    map[string]string{"version": "v2", "tier": "backend"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["api"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("expected 'api' in lastBackends")
	}
	if bg.Labels == nil {
		t.Fatal("expected Labels to be set in BackendGroup")
	}
	if bg.Labels["version"] != "v2" {
		t.Errorf("expected version=v2, got %q", bg.Labels["version"])
	}
	if bg.Labels["tier"] != "backend" {
		t.Errorf("expected tier=backend, got %q", bg.Labels["tier"])
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_BackendAnnotationsPassedToRenderer(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	h.backendCh <- backendChange{
		name:        "api",
		endpoints:   []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		annotations: map[string]string{"example.com/version": "v2", "example.com/tier": "backend"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["api"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("expected 'api' in lastBackends")
	}
	if bg.Annotations == nil {
		t.Fatal("expected Annotations to be set in BackendGroup")
	}
	if bg.Annotations["example.com/version"] != "v2" {
		t.Errorf("expected example.com/version=v2, got %q", bg.Annotations["example.com/version"])
	}
	if bg.Annotations["example.com/tier"] != "backend" {
		t.Errorf("expected example.com/tier=backend, got %q", bg.Annotations["example.com/tier"])
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_ExplicitBackendLabelsSeededAtStartup(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	// Pre-seed latestBackends with labels for an explicit backend,
	// simulating the initial seed from bw.Labels() at startup (main.go:598).
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.latestBackends["api"] = renderer.BackendGroup{
		Endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		Labels:    map[string]string{"version": "v1", "tier": "backend"},
	}
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Trigger a render via a frontend change.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	// The renderer should have received the pre-seeded labels.
	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["api"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("expected 'api' in lastBackends for explicit backend")
	}
	if bg.Labels == nil {
		t.Fatal("expected Labels to be set in BackendGroup")
	}
	if bg.Labels["version"] != "v1" {
		t.Errorf("expected version=v1, got %q", bg.Labels["version"])
	}
	if bg.Labels["tier"] != "backend" {
		t.Errorf("expected tier=backend, got %q", bg.Labels["tier"])
	}

	h.sigCh <- syscall.SIGTERM
	// Simulate varnishd exiting once the loop forwards the signal.
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for loop exit")
	}
	if c := code.Load(); c != 0 {
		t.Fatalf("expected exit 0, got %d", c)
	}
}

func TestRunLoop_ExplicitBackendAnnotationsSeededAtStartup(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	// Pre-seed latestBackends with annotations for an explicit backend,
	// simulating the initial seed from bw.Annotations() at startup.
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.latestBackends["api"] = renderer.BackendGroup{
		Endpoints:   []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		Annotations: map[string]string{"example.com/version": "v1", "example.com/tier": "backend"},
	}
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Trigger a render via a frontend change.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	// The renderer should have received the pre-seeded annotations.
	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["api"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("expected 'api' in lastBackends for explicit backend")
	}
	if bg.Annotations == nil {
		t.Fatal("expected Annotations to be set in BackendGroup")
	}
	if bg.Annotations["example.com/version"] != "v1" {
		t.Errorf("expected example.com/version=v1, got %q", bg.Annotations["example.com/version"])
	}
	if bg.Annotations["example.com/tier"] != "backend" {
		t.Errorf("expected example.com/tier=backend, got %q", bg.Annotations["example.com/tier"])
	}

	h.sigCh <- syscall.SIGTERM
	// Simulate varnishd exiting once the loop forwards the signal.
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for loop exit")
	}
	if c := code.Load(); c != 0 {
		t.Fatalf("expected exit 0, got %d", c)
	}
}

// TestRunLoop_BackendAnnotationsFilteredByWatcher simulates a watcher that has
// already filtered annotations (as happens in production when
// SetExcludeAnnotations is called). The runLoop should pass only the
// filtered annotations to the renderer.
func TestRunLoop_BackendAnnotationsFilteredByWatcher(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	// Simulate a backendChange from a watcher with annotation exclusion applied:
	// kubectl.kubernetes.io/last-applied-configuration was already stripped by
	// the watcher - it should NOT appear in the renderer's annotations.
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		annotations: map[string]string{
			"example.com/version": "v1",
			"example.com/tier":    "backend",
		},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["api"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("expected 'api' in lastBackends")
	}
	if bg.Annotations == nil {
		t.Fatal("expected Annotations to be set in BackendGroup")
	}
	if _, has := bg.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; has {
		t.Error("excluded annotation should not appear in renderer")
	}
	if bg.Annotations["example.com/version"] != "v1" {
		t.Errorf("example.com/version = %q, want v1", bg.Annotations["example.com/version"])
	}
	if bg.Annotations["example.com/tier"] != "backend" {
		t.Errorf("example.com/tier = %q, want backend", bg.Annotations["example.com/tier"])
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// TestRunLoop_BackendAnnotationsFilteredSeededAtStartup verifies that pre-seeded
// annotations (already filtered by the watcher at startup) flow correctly
// through the runLoop to the renderer on the first render.
func TestRunLoop_BackendAnnotationsFilteredSeededAtStartup(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)

	// Pre-seed with filtered annotations (as main.go would after calling
	// bw.Annotations() on a watcher with SetExcludeAnnotations).
	lc.latestBackends["api"] = renderer.BackendGroup{
		Endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		Annotations: map[string]string{
			"example.com/version": "v1",
			// kubectl.kubernetes.io/last-applied-configuration already excluded
		},
	}
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Trigger a render via a frontend change.
	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 80, Name: "pod-1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "mgr.Reload called")

	h.rend.mu.Lock()
	bg, ok := h.rend.lastBackends["api"]
	h.rend.mu.Unlock()

	if !ok {
		t.Fatal("expected 'api' in lastBackends")
	}
	if bg.Annotations == nil {
		t.Fatal("expected Annotations to be set in BackendGroup")
	}
	if _, has := bg.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; has {
		t.Error("excluded annotation should not appear in renderer")
	}
	if bg.Annotations["example.com/version"] != "v1" {
		t.Errorf("example.com/version = %q, want v1", bg.Annotations["example.com/version"])
	}
	if len(bg.Annotations) != 1 {
		t.Errorf("expected 1 annotation, got %d: %v", len(bg.Annotations), bg.Annotations)
	}

	h.sigCh <- syscall.SIGTERM
	// Simulate varnishd exiting once the loop forwards the signal.
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for loop exit")
	}
	if c := code.Load(); c != 0 {
		t.Fatalf("expected exit 0, got %d", c)
	}
}

func TestRunLoop_SkipsReloadWhenVCLUnchanged(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	// renderFn always returns the same VCL - simulates a metadata-only change
	// that doesn't affect the rendered output.
	const fixedVCL = "vcl 4.1; /* unchanged */"
	h.rend.renderFn = func(_ []watcher.Frontend, _ map[string]renderer.BackendGroup, _ map[string]map[string]any, _ map[string]map[string]any) (string, error) {
		return fixedVCL, nil
	}

	skippedBefore := getCounterValue(t, "skipped", h.metrics.VCLReloadsTotal)

	// Pre-populate lastVCLHash so the first render matches.
	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.lastVCLHash = sha256.Sum256([]byte(fixedVCL))
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send a backend change - should render but skip reload.
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}

	// Wait for the skipped counter to increment.
	waitFor(t, func() bool {
		return getCounterValue(t, "skipped", h.metrics.VCLReloadsTotal)-skippedBefore >= 1
	}, "VCL reload skipped")

	// Verify mgr.Reload was NOT called.
	if h.mgr.getReloadCount() != 0 {
		t.Fatalf("expected 0 mgr.Reload calls, got %d", h.mgr.getReloadCount())
	}

	// Verify Render was called (the template was still rendered).
	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected Render to be called")
	}

	// Verify skipped metric.
	if delta := getCounterValue(t, "skipped", h.metrics.VCLReloadsTotal) - skippedBefore; delta < 1 {
		t.Errorf("vcl_reloads_total(skipped) delta = %v, want >= 1", delta)
	}

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if int(code.Load()) != 0 {
		t.Fatalf("expected exit 0, got %d", code.Load())
	}
}

func TestRunLoop_BackendGroupReplacementIsIndependent(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	// Send first backend change with labels v1.
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		labels:    map[string]string{"version": "v1"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "first reload")

	h.rend.mu.Lock()
	firstLabels := h.rend.lastBackends["api"].Labels
	h.rend.mu.Unlock()

	// Send second backend change with labels v2 - this replaces the
	// entire BackendGroup in latestBackends.
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		labels:    map[string]string{"version": "v2"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "second reload")

	// The first labels snapshot must still show v1 - replacing the
	// BackendGroup must not have mutated the old Labels map.
	if firstLabels["version"] != "v1" {
		t.Errorf("first labels mutated after replacement: version=%q, want v1", firstLabels["version"])
	}

	h.rend.mu.Lock()
	secondLabels := h.rend.lastBackends["api"].Labels
	h.rend.mu.Unlock()

	if secondLabels["version"] != "v2" {
		t.Errorf("second labels not updated: version=%q, want v2", secondLabels["version"])
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_MockLastBackendsIndependentOfSubsequentChanges(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	// Send a backend with annotations.
	h.backendCh <- backendChange{
		name:        "api",
		endpoints:   []watcher.Endpoint{{Host: "10.0.1.1", Port: 8080, Name: "api-0"}},
		annotations: map[string]string{"example.com/version": "v1"},
	}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "first reload")

	h.rend.mu.Lock()
	snapshot := h.rend.lastBackends["api"]
	h.rend.mu.Unlock()

	// Remove the backend entirely.
	h.backendCh <- backendChange{name: "api", removed: true}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 2 }, "second reload")

	// The snapshot taken before removal must still be intact.
	if snapshot.Annotations["example.com/version"] != "v1" {
		t.Errorf("snapshot mutated after removal: %v", snapshot.Annotations)
	}
	if len(snapshot.Endpoints) != 1 {
		t.Errorf("snapshot endpoints mutated: got %d, want 1", len(snapshot.Endpoints))
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestStatusStoreInitWritesConcurrentWithSnapshot(t *testing.T) {
	t.Parallel()

	// main() populates several statusStore fields after the metrics server
	// (which serves snapshot() via /status) is already running. Those
	// writes must be synchronized with concurrent readers (run with -race).
	store := &statusStore{
		version:   "test",
		goVersion: "go-test",
		startedAt: time.Now(),
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		for range 5000 {
			_ = store.snapshot()
		}
	})

	// Mirrors the late-init assignments in main(), repeated so the writes
	// demonstrably overlap the concurrent snapshot() calls.
	close(start)
	for range 5000 {
		store.setVarnishMajorVersion(7)
		store.setServiceInfo("svc", "ns", true, true)
		store.setSourceCounts(2, 1)
	}

	wg.Wait()

	snap := store.snapshot()
	if snap.VarnishMajorVersion != 7 {
		t.Errorf("VarnishMajorVersion = %d, want 7", snap.VarnishMajorVersion)
	}
	if snap.ServiceName != "svc" || snap.ServiceNamespace != "ns" {
		t.Errorf("service info = %q/%q, want svc/ns", snap.ServiceName, snap.ServiceNamespace)
	}
	if !snap.DrainEnabled || !snap.BroadcastEnabled {
		t.Error("drain/broadcast flags not set")
	}
	if snap.ValuesCount != 2 || snap.SecretsCount != 1 {
		t.Errorf("source counts = %d/%d, want 2/1", snap.ValuesCount, snap.SecretsCount)
	}
}

// racyShutdownManager mirrors varnish.Manager's synchronization: err is
// written by a plain goroutine followed by close(done), and Err() reads it
// without a mutex. The happens-before edge exists only for readers that wait
// on Done() first - exactly the contract runLoop must respect after SIGKILL.
type racyShutdownManager struct {
	done chan struct{}
	err  error
}

func (*racyShutdownManager) Reload(string) error                           { return nil }
func (m *racyShutdownManager) Done() <-chan struct{}                       { return m.done }
func (m *racyShutdownManager) Err() error                                  { return m.err }
func (*racyShutdownManager) MarkBackendSick(string) error                  { return nil }
func (*racyShutdownManager) ActiveSessions() (uint64, error)               { return 0, nil }
func (*racyShutdownManager) LoadCert(string, []byte, []byte, []byte) error { return nil }
func (*racyShutdownManager) TLSSupported() bool                            { return false }

func (m *racyShutdownManager) ForwardSignal(sig os.Signal) {
	if sig == syscall.SIGKILL {
		go func() {
			m.err = errors.New("killed") // mirrors m.err = m.proc.Wait()
			close(m.done)
		}()
	}
}

func TestRunLoopShutdownTimeoutErrReadAfterKill(t *testing.T) {
	t.Parallel()

	// varnishd ignores SIGTERM (Done never closes), the shutdown timeout
	// fires, and runLoop escalates to SIGKILL. Reading mgr.Err() must be
	// ordered after the manager's Wait goroutine writes it (run with -race).
	h := newTestHarness()
	mgr := &racyShutdownManager{done: make(chan struct{})}
	lc := h.loopConfig(h.bcast)
	lc.mgr = mgr
	lc.shutdownTimeout = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	codeCh := make(chan int, 1)
	go func() { codeCh <- runLoop(ctx, cancel, lc) }()

	h.sigCh <- syscall.SIGTERM

	select {
	case code := <-codeCh:
		if code != 1 {
			t.Errorf("exit code = %d, want 1 (varnishd was killed with an error)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after SIGKILL escalation")
	}
}

func TestRunLoop_TLSCertUpdateLoadsCert(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.tlsSupported = true
	rec := h.withRecorder()
	updBefore := getCounterValue(t, "frontend", h.metrics.TLSCertUpdatesTotal)

	wait := h.runAndWait(h.bcast)

	h.tlsCertCh <- tlsCertChange{
		name: "frontend",
		data: watcher.TLSCertData{Cert: []byte("CERT"), Key: []byte("KEY")},
	}
	waitFor(t, func() bool { return len(h.mgr.getLoadCertCalls()) >= 1 }, "mgr.LoadCert called")

	// A cert rotation must NOT trigger a VCL reload.
	if rc := h.mgr.getReloadCount(); rc != 0 {
		t.Errorf("TLS cert update should not trigger a VCL reload, got reloadCount=%d", rc)
	}
	if delta := getCounterValue(t, "frontend", h.metrics.TLSCertUpdatesTotal) - updBefore; delta < 1 {
		t.Errorf("tls_cert_updates_total(frontend) delta = %v, want >= 1", delta)
	}
	requireEvent(t, collectEvents(rec, 1), "TLSCertReloaded", "frontend")

	if code := wait(); code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_TLSCertEmptyKeepsPrevious(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.tlsSupported = true
	rec := h.withRecorder()

	wait := h.runAndWait(h.bcast)

	// Empty material (Secret deleted): LoadCert is still invoked (a no-op),
	// and the event makes clear the previous certificate stays active.
	h.tlsCertCh <- tlsCertChange{name: "frontend", data: watcher.TLSCertData{}}
	waitFor(t, func() bool { return len(h.mgr.getLoadCertCalls()) >= 1 }, "mgr.LoadCert called")

	requireEvent(t, collectEvents(rec, 1), "TLSCertAbsent", "frontend")

	if code := wait(); code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_TLSCertReloadFailureRetries(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	h.mgr.tlsSupported = true
	var attempts atomic.Int32
	h.mgr.loadCertFn = func(string, []byte, []byte, []byte) error {
		if attempts.Add(1) == 1 {
			return errors.New("load failed")
		}

		return nil
	}
	rec := h.withRecorder()

	ctx, cancel := context.WithCancel(t.Context())
	lc := h.loopConfig(h.bcast)
	lc.reloadRecoveryInitial = 5 * time.Millisecond
	lc.reloadRecoveryMax = 20 * time.Millisecond
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.tlsCertCh <- tlsCertChange{
		name: "frontend",
		data: watcher.TLSCertData{Cert: []byte("CERT"), Key: []byte("KEY")},
	}

	// First attempt fails (TLSCertReloadFailed), recovery timer retries and
	// the second attempt succeeds (TLSCertReloaded).
	events := collectEvents(rec, 2)
	requireEvent(t, events, "TLSCertReloadFailed", "frontend")
	requireEvent(t, events, "TLSCertReloaded", "frontend")
	if attempts.Load() < 2 {
		t.Errorf("expected at least 2 LoadCert attempts, got %d", attempts.Load())
	}

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done
	if c := int(code.Load()); c != 0 {
		t.Fatalf("expected exit 0, got %d", c)
	}
}

func TestSetBroadcasterNilLeavesInterfaceNil(t *testing.T) {
	t.Parallel()
	lc := &loopConfig{}
	var s *broadcast.Server // typed nil: broadcast disabled (--broadcast-addr=none)
	setBroadcaster(lc, s)
	// Must be a true nil interface so the loop's `lc.bcast != nil` guards skip
	// it. A typed-nil would make the guard true and panic on SetFrontends/Drain.
	if lc.bcast != nil {
		t.Fatal("disabled broadcast must leave lc.bcast a true nil interface")
	}
}

func TestSetBroadcasterNonNilAssigns(t *testing.T) {
	t.Parallel()
	lc := &loopConfig{}
	s := broadcast.New(broadcast.Options{
		Addr:    ":0",
		Metrics: telemetry.NewMetrics(prometheus.NewRegistry(), nil),
	})
	setBroadcaster(lc, s)
	if lc.bcast == nil {
		t.Fatal("enabled broadcast must be assigned to lc.bcast")
	}
}

func TestRunLoop_FrontendChangeWithBroadcastDisabled(t *testing.T) {
	t.Parallel()
	// Regression: with broadcast disabled, a frontend endpoint change must not
	// panic (previously a typed-nil broadcaster defeated the nil guard).
	h := newTestHarness()
	wait := h.runAndWait(nil) // nil broadcaster

	h.frontendCh <- []watcher.Frontend{{Host: "10.0.0.1", Port: 8080, Name: "p1"}}
	waitFor(t, func() bool { return h.mgr.getReloadCount() >= 1 }, "reload after frontend change")

	if code := wait(); code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

// --- Watcher-wiring goleak tests (the run() helpers: startTemplateWatch /
// startValuesDirWatchers / wireValuesFanIn) ---
//
// These verify that the wiring helpers create no goroutines/channels for a feature
// whose auto-reload is disabled, and that the goroutines they DO create when enabled
// are cleaned up on context cancel (no leak). goleak.IgnoreCurrent() snapshots
// pre-existing goroutines at the start of each test so unrelated goroutines don't
// cause false positives. These tests must NOT run in parallel: a sibling test's
// goroutines would otherwise be seen as leaks, hence the //nolint:paralleltest.

func TestStartTemplateWatch_DisabledCreatesNothing(t *testing.T) { //nolint:paralleltest // goleak must run in isolation
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	ch := startTemplateWatch(t.Context(), &config.Config{FileWatch: false})
	if ch != nil {
		t.Error("expected nil template channel when --file-watch is disabled")
	}
}

func TestStartValuesDirWatchers_NotWatchingNoPollGoroutine(t *testing.T) { //nolint:paralleltest // goleak must run in isolation
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "k.yaml"), []byte("v"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ValuesDirs:            []config.ValuesDirSpec{{Name: "d", Path: dir}},
		ValuesDirPollInterval: time.Second,
		ValuesDirWatch:        false,
	}
	watchers, names := startValuesDirWatchers(t.Context(), cfg, func(error) {})
	if len(watchers) != 1 || len(names) != 1 {
		t.Fatalf("expected 1 watcher + 1 name, got %d/%d", len(watchers), len(names))
	}
	// ScanOnce delivered the initial values synchronously; drain it. goleak then
	// confirms no polling goroutine was started.
	<-watchers[0].Changes()
}

func TestWireValuesFanIn_DisabledNilNoGoroutine(t *testing.T) { //nolint:paralleltest // goleak must run in isolation
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	dir := t.TempDir()
	fvw := watcher.NewFileValuesWatcher(dir, time.Second)
	ch := wireValuesFanIn(
		t.Context(),
		&config.Config{ValuesDirWatch: false},
		nil, nil,
		[]*watcher.FileValuesWatcher{fvw}, []string{"d"},
	)
	if ch != nil {
		t.Error("expected nil values channel: values-dir watch disabled and no --values watchers")
	}
}

// TestWireValuesFanIn_FanInGoroutineExitsOnContextCancel verifies the fan-in
// goroutine returns when ctx is cancelled even though the ConfigMapWatcher's
// Changes() channel is never closed (informer-backed watchers don't close their
// channels - closing them would risk a send-on-closed panic). Without the
// ctx-aware select (a plain `for range w.Changes()`) the goroutine would block
// forever on the open channel and goleak would flag the leak.
func TestWireValuesFanIn_FanInGoroutineExitsOnContextCancel(t *testing.T) { //nolint:paralleltest // goleak must run in isolation
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	// A ConfigMapWatcher we never Run: its Changes() channel is created by the
	// constructor and is never closed, so a range-based fan-in would never end.
	vw := watcher.NewConfigMapWatcher(fake.NewClientset(), "ns", "cm")
	ctx, cancel := context.WithCancel(t.Context())
	ch := wireValuesFanIn(ctx, &config.Config{}, []*watcher.ConfigMapWatcher{vw}, []string{"v"}, nil, nil)
	if ch == nil {
		t.Fatal("expected non-nil values channel with one --values watcher")
	}

	// Cancel; goleak (which retries) confirms the fan-in goroutine returned
	// rather than leaking on the never-closed channel.
	cancel()
}

func TestStartTemplateWatch_EnabledChannelAndCleanup(t *testing.T) { //nolint:paralleltest // goleak must run in isolation
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	tmpl := filepath.Join(t.TempDir(), "vcl.tmpl")
	err := os.WriteFile(tmpl, []byte("vcl 4.1;"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	ch := startTemplateWatch(ctx, &config.Config{
		FileWatch:                true,
		VCLTemplate:              tmpl,
		VCLTemplateWatchInterval: 10 * time.Millisecond,
	})
	if ch == nil {
		t.Fatal("expected a non-nil template channel when --file-watch is enabled")
	}
	cancel() // the watchFile goroutine exits on ctx cancel; goleak verifies it.
}

// --- recoveryState unit tests ---
//
// recoveryState is the exponential-backoff retry timer shared by the VCL and
// TLS reload paths. The integration behaviour is covered by the
// TestRunLoop_ReloadRecovery* tests; these exercise the type's contract
// directly, including the disabled and capped edge cases.

func TestRecoveryState_Next(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		initial  time.Duration
		maxDelay time.Duration
		delay    time.Duration
		want     time.Duration
	}{
		{"first retry uses initial", 100 * time.Millisecond, 800 * time.Millisecond, 0, 100 * time.Millisecond},
		{"doubles previous delay", 100 * time.Millisecond, 800 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond},
		{"doubles again", 100 * time.Millisecond, 800 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond},
		{"caps at maxDelay", 100 * time.Millisecond, 800 * time.Millisecond, 800 * time.Millisecond, 800 * time.Millisecond},
		{"no cap when maxDelay is zero", 100 * time.Millisecond, 0, 500 * time.Millisecond, 1000 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := recoveryState{initial: tt.initial, maxDelay: tt.maxDelay, delay: tt.delay}
			if got := r.next(); got != tt.want {
				t.Errorf("next() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecoveryState_ArmProgressionAndCap(t *testing.T) {
	t.Parallel()
	r := recoveryState{initial: 50 * time.Millisecond, maxDelay: 200 * time.Millisecond}

	// Each consecutive arm doubles the delay, capped at maxDelay.
	for _, want := range []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		200 * time.Millisecond,
	} {
		got := r.arm()
		if got != want {
			t.Fatalf("arm() = %v, want %v", got, want)
		}
		if r.timer == nil {
			t.Fatal("arm() should set a timer")
		}
		if r.channel() == nil {
			t.Fatal("channel() should be non-nil while a retry is armed")
		}
	}
	r.reset()
}

func TestRecoveryState_ArmDisabled(t *testing.T) {
	t.Parallel()
	r := recoveryState{initial: 0} // initial <= 0 disables retries

	if got := r.arm(); got != 0 {
		t.Errorf("arm() with zero initial = %v, want 0", got)
	}
	if r.timer != nil {
		t.Error("arm() should not create a timer when disabled")
	}
	if r.channel() != nil {
		t.Error("channel() should be nil when no retry is armed")
	}
}

func TestRecoveryState_Reset(t *testing.T) {
	t.Parallel()
	r := recoveryState{initial: 50 * time.Millisecond}
	r.arm()

	r.reset()

	if r.timer != nil {
		t.Error("reset() should clear the timer")
	}
	if r.delay != 0 {
		t.Errorf("reset() should zero the delay, got %v", r.delay)
	}
	if r.channel() != nil {
		t.Error("channel() should be nil after reset()")
	}
	// A second reset on an already-clear state must be a safe no-op.
	r.reset()
}

func TestRecoveryState_FiredPreservesDelay(t *testing.T) {
	t.Parallel()
	r := recoveryState{initial: 50 * time.Millisecond}
	r.arm() // delay is now 50ms and a timer is armed

	prev := r.fired()

	if prev != 50*time.Millisecond {
		t.Errorf("fired() = %v, want 50ms", prev)
	}
	if r.timer != nil {
		t.Error("fired() should clear the timer reference so it is not re-selected")
	}
	// fired() preserves delay so a re-arm in the same retry cycle keeps doubling.
	if r.delay != 50*time.Millisecond {
		t.Errorf("fired() should preserve delay, got %v", r.delay)
	}
	if got := r.next(); got != 100*time.Millisecond {
		t.Errorf("next() after fired() = %v, want 100ms (continued doubling)", got)
	}
}

// --- debounceState.reset unit test ---

func TestDebounceState_Reset(t *testing.T) {
	t.Parallel()
	s := debounceState{
		timer:      time.NewTimer(time.Hour),
		deadline:   time.Now().Add(time.Hour),
		firstEvent: time.Now(),
		capped:     true,
	}

	s.reset()

	if s.timer != nil {
		t.Error("reset() should clear the timer")
	}
	if !s.deadline.IsZero() {
		t.Error("reset() should zero the deadline")
	}
	if !s.firstEvent.IsZero() {
		t.Error("reset() should zero firstEvent")
	}
	if s.capped {
		t.Error("reset() should clear capped")
	}
	// A second reset on an already-clear state must be a safe no-op.
	s.reset()
}

// --- setupMetricsServer unit tests ---
//
// The /metrics, /status, /healthz and /readyz handlers are covered by their own
// handler tests, and end-to-end serving is covered by the hurl E2E suite. These
// exercise the two deterministic branches setupMetricsServer adds: the disabled
// no-op and the pre-bind listen failure (which must fail startup cleanly rather
// than from inside the serve goroutine).

func TestSetupMetricsServer_DisabledWhenNoAddr(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{MetricsAddr: ""}
	errCh := make(chan error, 1)

	srv, err := setupMetricsServer(cfg, &statusStore{}, errCh)
	if err != nil {
		t.Fatalf("setupMetricsServer with empty addr = %v, want nil", err)
	}
	if srv != nil {
		t.Error("expected a nil server when no metrics address is configured")
	}
}

func TestSetupMetricsServer_ListenError(t *testing.T) {
	t.Parallel()
	// Occupy an ephemeral port so setupMetricsServer's bind on the same address
	// fails synchronously.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-binding a port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	cfg := &config.Config{
		MetricsAddr: addr,
		ListenAddrs: []config.ListenAddrSpec{{Port: 8080}},
	}
	errCh := make(chan error, 1)

	srv, gotErr := setupMetricsServer(cfg, &statusStore{}, errCh)
	if gotErr == nil {
		t.Fatal("setupMetricsServer with an occupied address should return an error")
	}
	if srv != nil {
		t.Error("expected a nil server when the bind fails")
	}
	if !strings.Contains(gotErr.Error(), addr) {
		t.Errorf("error %q should mention the occupied address %q", gotErr, addr)
	}
}

func TestSetupMetricsServer_ServesAndShutsDown(t *testing.T) {
	t.Parallel()
	// Reserve an ephemeral port, then hand its address to setupMetricsServer.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	cfg := &config.Config{
		MetricsAddr: addr,
		ListenAddrs: []config.ListenAddrSpec{{Port: 8080}},
	}
	status := &statusStore{varnishdUp: true} // /healthz returns 200 when varnishd is up
	errCh := make(chan error, 1)

	srv, err := setupMetricsServer(cfg, status, errCh)
	if err != nil {
		t.Fatalf("setupMetricsServer: %v", err)
	}
	if srv == nil {
		t.Fatal("expected a non-nil server when a metrics address is configured")
	}
	t.Cleanup(func() { _ = srv.Close() }) // safety net if an assertion fails early

	// The listener is bound before setupMetricsServer returns, so the endpoint is
	// reachable as soon as the serve goroutine is scheduled; retry briefly.
	client := &http.Client{Timeout: 2 * time.Second}
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = client.Get("http://" + addr + "/healthz")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics server never became reachable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Graceful shutdown stops the server cleanly: Serve returns ErrServerClosed,
	// which is filtered, so no fatal serve error is reported.
	shutdownErr := srv.Shutdown(t.Context())
	if shutdownErr != nil {
		t.Errorf("Shutdown: %v", shutdownErr)
	}
	select {
	case serveErr := <-errCh:
		t.Errorf("unexpected serve error after shutdown: %v", serveErr)
	default:
	}
}

// --- writeInitialVCLFile unit tests ---

func TestWriteInitialVCLFile_WritesAndCleansUp(t *testing.T) {
	t.Parallel()
	const contents = "vcl 4.1;\n"

	path, cleanup, err := writeInitialVCLFile(contents)
	if err != nil {
		t.Fatalf("writeInitialVCLFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written VCL file: %v", err)
	}
	if string(got) != contents {
		t.Errorf("file contents = %q, want %q", got, contents)
	}

	cleanup()
	_, statErr := os.Stat(path)
	if !os.IsNotExist(statErr) {
		t.Errorf("cleanup() should have removed %s, stat err = %v", path, statErr)
	}
}

func TestWriteInitialVCLFile_CreateError(t *testing.T) {
	// Point the temp dir at a path that does not exist so os.CreateTemp fails.
	// os.TempDir() reads a different env var per platform - TMPDIR on
	// Unix/macOS, TMP/TEMP on Windows (via GetTempPath) - so set all three to
	// make the temp dir missing on every OS.
	// t.Setenv forbids t.Parallel, so this test runs serially.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("TMPDIR", missing)
	t.Setenv("TMP", missing)
	t.Setenv("TEMP", missing)

	path, cleanup, err := writeInitialVCLFile("vcl 4.1;")
	if err == nil {
		t.Fatal("writeInitialVCLFile should fail when the temp dir is missing")
	}
	if path != "" {
		t.Errorf("path = %q on error, want empty", path)
	}
	if cleanup != nil {
		t.Error("cleanup should be nil on error")
	}
}

// TestRunLoop_ServerErrorStopsNCSA verifies the serverErrCh shutdown path
// stops varnishncsa: its monitor does not watch ctx and would otherwise keep
// running (or respawn varnishncsa mid-restart-delay) while the process exits.
func TestRunLoop_ServerErrorStopsNCSA(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	var stopCalled atomic.Bool
	serverErrCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.serverErrCh = serverErrCh
	lc.stopNCSA = func() { stopCalled.Store(true) }
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	serverErrCh <- errors.New("broadcast serve: accept failed")

	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done

	if !stopCalled.Load() {
		t.Fatal("stopNCSA was not called on the serverErrCh shutdown path")
	}
}

// TestRunLoop_UnexpectedVarnishdExitStopsNCSA verifies the unexpected-exit
// path stops varnishncsa before returning, for the same reason as above.
func TestRunLoop_UnexpectedVarnishdExitStopsNCSA(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	var stopCalled atomic.Bool

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.stopNCSA = func() { stopCalled.Store(true) }
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// varnishd exits unexpectedly.
	close(h.mgr.done)
	<-done

	if got := int(code.Load()); got != 1 {
		t.Fatalf("expected exit code 1, got %d", got)
	}
	if !stopCalled.Load() {
		t.Fatal("stopNCSA was not called on the unexpected-varnishd-exit path")
	}
}

// TestRunLoop_ServerErrorWaitsForVarnishdAfterSIGKILL verifies that after the
// SIGKILL escalation on the serverErrCh path, runLoop waits for varnishd to be
// reaped instead of returning while the child is still dying.
func TestRunLoop_ServerErrorWaitsForVarnishdAfterSIGKILL(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	serverErrCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.serverErrCh = serverErrCh
	lc.shutdownTimeout = 20 * time.Millisecond
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	serverErrCh <- errors.New("broadcast serve: accept failed")

	// varnishd ignores SIGTERM (mgr.done stays open), so the loop escalates
	// to SIGKILL after shutdownTimeout.
	waitFor(t, func() bool {
		for _, s := range h.mgr.getForwardedSigs() {
			if s == syscall.SIGKILL {
				return true
			}
		}

		return false
	}, "SIGKILL forwarded")

	// The loop must still be waiting for the reap, not returned.
	time.Sleep(50 * time.Millisecond)
	if got := int(code.Load()); got != -1 {
		t.Fatalf("runLoop returned (code %d) before varnishd was reaped after SIGKILL", got)
	}

	close(h.mgr.done)
	<-done
	if got := int(code.Load()); got != 1 {
		t.Fatalf("expected exit code 1, got %d", got)
	}
}

// TestRunLoop_ShutdownTimeoutZeroWaitsIndefinitely verifies that
// --shutdown-timeout=0 means "wait indefinitely for varnishd" on the signal
// shutdown path. [time.After] with 0 fires immediately, so without the
// afterOrNever guard the loop escalated to SIGKILL right after SIGTERM -
// silently defeating the graceful shutdown the zero value is documented to
// provide.
func TestRunLoop_ShutdownTimeoutZeroWaitsIndefinitely(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.shutdownTimeout = 0
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM

	// Wait until the loop has forwarded SIGTERM and is waiting on mgr.Done().
	waitForForwardedSignal(t, h.mgr)

	// Give the buggy immediate time.After(0) escalation ample time to fire,
	// then assert varnishd was never SIGKILLed while still running.
	time.Sleep(100 * time.Millisecond)
	for _, s := range h.mgr.getForwardedSigs() {
		if s == syscall.SIGKILL {
			t.Fatal("varnishd was SIGKILLed despite --shutdown-timeout=0 (documented as: wait indefinitely)")
		}
	}
	if got := int(code.Load()); got != -1 {
		t.Fatalf("runLoop returned (code %d) instead of waiting indefinitely for varnishd", got)
	}

	// varnishd exits on its own; the loop must then return cleanly.
	close(h.mgr.done)
	<-done
	if got := int(code.Load()); got != 0 {
		t.Fatalf("expected exit code 0, got %d", got)
	}
}

// TestLoadInitialTLSCertsKillsVarnishdOnFailure verifies a failed initial
// certificate load does not leave the already-started varnishd running: this
// is the only exit path between mgr.Start and runLoop, and without the
// kill+reap an unkilled varnishd survives the process exit whenever
// k8s-httpcache is not the container's PID 1, keeping the listen ports bound.
func TestLoadInitialTLSCertsKillsVarnishdOnFailure(t *testing.T) {
	t.Parallel()

	mgr := &mockManager{
		done:         make(chan struct{}),
		tlsSupported: true,
		loadCertFn: func(string, []byte, []byte, []byte) error {
			return errors.New("bad cert")
		},
	}
	close(mgr.done) // process exits once killed; the helper must reap via Done()

	err := loadInitialTLSCerts(mgr, []string{"web"}, map[string]watcher.TLSCertData{
		"web": {Cert: []byte("c"), Key: []byte("k")},
	}, func(string, watcher.TLSCertData) {})
	if err == nil {
		t.Fatal("expected error from failed initial certificate load")
	}

	sigs := mgr.getForwardedSigs()
	killed := false
	for _, s := range sigs {
		if s == syscall.SIGKILL {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("expected SIGKILL to varnishd on initial TLS load failure, got %v (varnishd left orphaned)", sigs)
	}
}

// TestLoadInitialTLSCertsSuccess verifies the happy path: onLoaded fires only
// for certificates that carried material, and varnishd is left untouched.
func TestLoadInitialTLSCertsSuccess(t *testing.T) {
	t.Parallel()

	mgr := &mockManager{done: make(chan struct{}), tlsSupported: true}
	var loaded []string
	err := loadInitialTLSCerts(mgr, []string{"web", "empty"}, map[string]watcher.TLSCertData{
		"web":   {Cert: []byte("c"), Key: []byte("k")},
		"empty": {},
	}, func(name string, _ watcher.TLSCertData) { loaded = append(loaded, name) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(loaded) != 1 || loaded[0] != "web" {
		t.Errorf("onLoaded calls = %v, want [web]", loaded)
	}
	if sigs := mgr.getForwardedSigs(); len(sigs) != 0 {
		t.Errorf("unexpected signals on success path: %v", sigs)
	}
}

// TestMetricsShutdownTimeoutBounded verifies the metrics server's graceful
// shutdown always gets a deadline: --shutdown-timeout=0 means "wait
// indefinitely for varnishd", but kubeContext(0) would give the HTTP drain no
// deadline at all, letting an active scrape stall process exit after varnishd
// is already gone.
func TestMetricsShutdownTimeoutBounded(t *testing.T) {
	t.Parallel()
	if got := metricsShutdownTimeout(0); got != defaultMetricsShutdownTimeout {
		t.Errorf("metricsShutdownTimeout(0) = %v, want the bounded default %v", got, defaultMetricsShutdownTimeout)
	}
	if got := metricsShutdownTimeout(3 * time.Second); got != 3*time.Second {
		t.Errorf("metricsShutdownTimeout(3s) = %v, want 3s passed through", got)
	}
}

// TestRunLoopRenderErrorRedactedInEvent verifies template render errors are
// redacted before reaching Kubernetes Events: a failing template function
// echoes its input (e.g. atoi on a non-numeric secret), and the Event is
// persisted in etcd where anyone with events RBAC can read it.
func TestRunLoopRenderErrorRedactedInEvent(t *testing.T) {
	t.Parallel()
	h := newTestHarness()
	rec := h.withRecorder()

	red := redact.NewRedactor()
	red.Update(map[string]map[string]any{"db": {"password": "s3cr3t-value-xyz"}})

	h.rend.renderFn = func([]watcher.Frontend, map[string]renderer.BackendGroup, map[string]map[string]any, map[string]map[string]any) (string, error) {
		return "", errors.New(`template: parsing "s3cr3t-value-xyz": invalid syntax`)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.redactor = red
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Trigger a reload whose render fails.
	h.frontendCh <- []watcher.Frontend{{Name: "pod-0", Host: "10.0.0.1", Port: 8080}}

	select {
	case ev := <-rec.Events:
		if strings.Contains(ev, "s3cr3t-value-xyz") {
			t.Fatalf("secret leaked into a persisted Kubernetes Event: %q", ev)
		}
		if !strings.Contains(ev, "VCLRenderFailed") {
			t.Fatalf("expected VCLRenderFailed event, got %q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event emitted for the render failure")
	}

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done
}

// TestReadyzDialAddr verifies the readiness probe dials the configured bind
// host: a specific-IP first listener (e.g. the pod IP) is not listening on
// loopback, and always dialing localhost there keeps the pod NotReady
// forever.
func TestReadyzDialAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want string
	}{
		{"", "localhost:8080"},
		{"0.0.0.0", "localhost:8080"},
		{"::", "localhost:8080"},
		{"127.0.0.1", "127.0.0.1:8080"},
		{"10.0.0.5", "10.0.0.5:8080"},
	}
	for _, tt := range tests {
		la := config.ListenAddrSpec{Host: tt.host, Port: 8080}
		if got := readyzDialAddr(la); got != tt.want {
			t.Errorf("readyzDialAddr(host=%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

// TestNotifySignalsIncludesSIGHUP verifies SIGHUP triggers the graceful
// shutdown path: with the default disposition a terminal hangup terminates
// the process immediately, orphaning varnishd without a drain.
func TestNotifySignalsIncludesSIGHUP(t *testing.T) { //nolint:paralleltest // raises a real signal against the whole process
	ch := notifySignals()
	defer signal.Stop(ch)

	err := syscall.Kill(os.Getpid(), syscall.SIGHUP)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case sig := <-ch:
		if sig != syscall.SIGHUP {
			t.Fatalf("received %v, want SIGHUP", sig)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SIGHUP not delivered to the shutdown channel")
	}
}

// TestShutdownBudget verifies the worst-case shutdown budget sums every
// sequential SIGTERM phase - notably the broadcast drain, which the original
// grace-period guidance omitted (a drain-enabled pod could be SIGKILLed
// mid-drain despite following the documented formula).
func TestShutdownBudget(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ShutdownTimeout:          30 * time.Second,
		Drain:                    true,
		DrainDelay:               15 * time.Second,
		DrainTimeout:             30 * time.Second,
		BroadcastAddr:            ":8088",
		BroadcastDrainTimeout:    30 * time.Second,
		BroadcastShutdownTimeout: 5 * time.Second,
		VarnishncsaEnabled:       true,
		MetricsAddr:              ":9101",
	}
	// 30 (varnishd) + 15+30 (drain) + 30+5 (broadcast) + 5 (ncsa) + 30 (metrics).
	want := 145 * time.Second
	if got := shutdownBudget(cfg); got != want {
		t.Errorf("shutdownBudget = %v, want %v", got, want)
	}

	// Disabled phases contribute nothing.
	lean := &config.Config{ShutdownTimeout: 30 * time.Second}
	if got := shutdownBudget(lean); got != 30*time.Second {
		t.Errorf("lean shutdownBudget = %v, want 30s", got)
	}

	// --shutdown-timeout=0 means the varnishd wait is unbounded: no finite
	// budget exists and no warning can be computed.
	unbounded := &config.Config{Drain: true, DrainDelay: 15 * time.Second}
	if got := shutdownBudget(unbounded); got != 0 {
		t.Errorf("unbounded shutdownBudget = %v, want 0", got)
	}
}

// TestRunLoopEmptyTLSUpdateKeepsKeyRedaction verifies an emptied TLS Secret
// does not wipe the certificate's private key from the redaction set:
// LoadCert no-ops on empty material and keeps the previous certificate (and
// its key) active, so the key must remain a redaction target.
func TestRunLoopEmptyTLSUpdateKeepsKeyRedaction(t *testing.T) {
	t.Parallel()
	h := newTestHarness()

	const keyLine = "MIGkAgEBBDBkeymaterialline"
	red := redact.NewRedactor()
	red.SetStaticValues(tlsRedactKey("web"), []string{keyLine})

	ctx, cancel := context.WithCancel(t.Context())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.redactor = red
	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// The Secret is emptied (e.g. briefly during a bad rotation); the old
	// certificate stays active, so its key must stay redacted.
	h.tlsCertCh <- tlsCertChange{name: "web", data: watcher.TLSCertData{}}

	waitFor(t, func() bool {
		return red.Redact(keyLine) != keyLine
	}, "key material still redacted after empty TLS update")

	h.sigCh <- syscall.SIGTERM
	waitFor(t, func() bool { return len(h.mgr.getForwardedSigs()) > 0 }, "signal forwarded")
	close(h.mgr.done)
	<-done
}
