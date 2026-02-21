package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"k8s-httpcache/internal/watcher"
)

func TestBackendChanNil(t *testing.T) {
	ch := backendChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil input")
	}
}

func TestBackendChanNonNil(t *testing.T) {
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
	ch := timerChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil timer")
	}
}

func TestTimerChanNonNil(t *testing.T) {
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
	f, err := os.CreateTemp(t.TempDir(), "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	if _, err := f.WriteString("initial"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx := t.Context()

	ch := watchFile(ctx, path, 50*time.Millisecond)

	// Wait a tick so the watcher reads the initial content.
	time.Sleep(100 * time.Millisecond)

	// Modify the file.
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
		// OK — change detected
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for file change notification")
	}
}

func TestWatchFileNoChangeNoNotification(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	if _, err := f.WriteString("stable"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx := t.Context()

	ch := watchFile(ctx, path, 50*time.Millisecond)

	// No modification — should not receive anything.
	select {
	case <-ch:
		t.Fatal("unexpected file change notification")
	case <-time.After(300 * time.Millisecond):
		// OK — no notification
	}
}

func TestWatchFileStopsOnContextCancel(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := watchFile(ctx, path, 50*time.Millisecond)

	// Cancel the context immediately.
	cancel()

	// The goroutine should exit. We verify by writing a change and
	// confirming no notification arrives (goroutine has stopped polling).
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(path, []byte("changed-after-cancel"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
		t.Fatal("unexpected notification after context cancel")
	case <-time.After(300 * time.Millisecond):
		// OK — goroutine stopped
	}
}

// --- Mock types for runLoop tests ---

type mockRenderer struct {
	mu             sync.Mutex
	reloadFn       func() error
	renderToFileFn func([]watcher.Frontend, map[string][]watcher.Endpoint) (string, error)
	rollbackFn     func()
	reloadCount    int
	renderCount    int
	rollbackCount  int
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

func (m *mockRenderer) RenderToFile(fe []watcher.Frontend, be map[string][]watcher.Endpoint) (string, error) {
	m.mu.Lock()
	m.renderCount++
	fn := m.renderToFileFn
	m.mu.Unlock()
	if fn != nil {
		return fn(fe, be)
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

func (m *mockRenderer) counts() (reload, render, rollback int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadCount, m.renderCount, m.rollbackCount
}

type mockManager struct {
	mu            sync.Mutex
	reloadFn      func(string) error
	done          chan struct{}
	err           error
	reloadCount   int
	forwardedSigs []os.Signal
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

func (m *mockManager) getReloadCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadCount
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

func (m *mockBroadcast) counts() (set, drain int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setCount, m.drainCount
}

// testHarness bundles mocks and channels for a runLoop test.
type testHarness struct {
	rend  *mockRenderer
	mgr   *mockManager
	bcast *mockBroadcast

	frontendCh chan []watcher.Frontend
	backendCh  chan backendChange
	templateCh chan struct{}
	sigCh      chan os.Signal
}

func newTestHarness() *testHarness {
	return &testHarness{
		rend:       &mockRenderer{},
		mgr:        &mockManager{done: make(chan struct{})},
		bcast:      &mockBroadcast{},
		frontendCh: make(chan []watcher.Frontend, 1),
		backendCh:  make(chan backendChange, 1),
		templateCh: make(chan struct{}, 1),
		sigCh:      make(chan os.Signal, 1),
	}
}

func (h *testHarness) loopConfig(bcast broadcaster) loopConfig {
	return loopConfig{
		rend:  h.rend,
		mgr:   h.mgr,
		bcast: bcast,

		frontendCh: h.frontendCh,
		backendCh:  h.backendCh,
		templateCh: h.templateCh,
		sigCh:      h.sigCh,

		serviceName:           "test-svc",
		debounce:              1 * time.Millisecond,
		shutdownTimeout:       1 * time.Second,
		broadcastDrainTimeout: 1 * time.Second,

		latestFrontends: nil,
		latestBackends:  make(map[string][]watcher.Endpoint),
	}
}

// runAndWait runs the loop in a goroutine and returns a function that
// sends SIGTERM and waits for the exit code.
func (h *testHarness) runAndWait(bcast broadcaster) (wait func() int) {
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
		// Give the loop time to enter the signal handler before closing
		// done, so the outer select picks the signal case, not the
		// unexpected-exit case.
		time.Sleep(20 * time.Millisecond)
		close(h.mgr.done) // simulate varnishd exiting after signal
		<-done
		return int(code.Load())
	}
}

// --- runLoop tests ---

func TestRunLoop_ReloadZeroFrontends(t *testing.T) {
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	h.frontendCh <- []watcher.Frontend{}
	time.Sleep(20 * time.Millisecond) // debounce

	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile to be called for zero frontends")
	}
	if h.mgr.getReloadCount() < 1 {
		t.Fatal("expected mgr.Reload to be called for zero frontends")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_ReloadWithFrontends(t *testing.T) {
	h := newTestHarness()
	var mu sync.Mutex
	var gotFrontends []watcher.Frontend
	h.rend.renderToFileFn = func(fe []watcher.Frontend, _ map[string][]watcher.Endpoint) (string, error) {
		mu.Lock()
		gotFrontends = fe
		mu.Unlock()
		return "test.vcl", nil
	}
	wait := h.runAndWait(h.bcast)

	pods := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 80, Name: "pod-1"},
		{IP: "10.0.0.2", Port: 80, Name: "pod-2"},
	}
	h.frontendCh <- pods
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	n := len(gotFrontends)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("expected 2 frontends, got %d", n)
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_BackendUpdateTriggersReload(t *testing.T) {
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}
	time.Sleep(20 * time.Millisecond)

	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile after backend update")
	}
	if h.mgr.getReloadCount() < 1 {
		t.Fatal("expected mgr.Reload after backend update")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_TemplateChangeTriggersReparse(t *testing.T) {
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	h.templateCh <- struct{}{}
	time.Sleep(20 * time.Millisecond)

	reloadCount, renderCount, _ := h.rend.counts()
	if reloadCount < 1 {
		t.Fatal("expected rend.Reload() to be called on template change")
	}
	if renderCount < 1 {
		t.Fatal("expected RenderToFile after template change")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_TemplateParseErrorKeepsOld(t *testing.T) {
	h := newTestHarness()
	h.rend.reloadFn = func() error { return errors.New("parse error") }
	wait := h.runAndWait(h.bcast)

	h.templateCh <- struct{}{}
	time.Sleep(20 * time.Millisecond)

	_, renderCount, rollbackCount := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile to still be called with old template")
	}
	if rollbackCount != 0 {
		t.Fatal("expected no Rollback when template parse fails (old template kept)")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_RenderErrorTriggersRollback(t *testing.T) {
	h := newTestHarness()
	var renderCalls atomic.Int32
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint) (string, error) {
		if renderCalls.Add(1) == 1 {
			return "", errors.New("render error")
		}
		return "test.vcl", nil
	}
	wait := h.runAndWait(h.bcast)

	// Template change → Reload succeeds → RenderToFile fails → Rollback
	h.templateCh <- struct{}{}
	time.Sleep(20 * time.Millisecond)

	_, _, rollbackCount := h.rend.counts()
	if rollbackCount < 1 {
		t.Fatal("expected Rollback after render error with new template")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_VarnishReloadErrorTriggersRollback(t *testing.T) {
	h := newTestHarness()
	var mgrCalls atomic.Int32
	h.mgr.reloadFn = func(_ string) error {
		if mgrCalls.Add(1) == 1 {
			return errors.New("varnish reload error")
		}
		return nil
	}
	wait := h.runAndWait(h.bcast)

	// Template change → Reload succeeds → RenderToFile succeeds →
	// mgr.Reload fails → Rollback → retry render → retry mgr.Reload
	h.templateCh <- struct{}{}
	time.Sleep(20 * time.Millisecond)

	_, _, rollbackCount := h.rend.counts()
	if rollbackCount < 1 {
		t.Fatal("expected Rollback after varnish reload error with new template")
	}
	// Retry should have called mgr.Reload a second time
	if h.mgr.getReloadCount() < 2 {
		t.Fatal("expected retry mgr.Reload after rollback")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_RollbackRenderError(t *testing.T) {
	h := newTestHarness()
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint) (string, error) {
		return "", errors.New("render always fails")
	}
	wait := h.runAndWait(h.bcast)

	// Template change → rend.Reload ok → RenderToFile fails → Rollback →
	// (no retry render path in this case, since the first render failed)
	// Actually the first render error with reloadedTemplate → Rollback, continue.
	// The loop continues without crashing.
	h.templateCh <- struct{}{}
	time.Sleep(20 * time.Millisecond)

	_, _, rollbackCount := h.rend.counts()
	if rollbackCount < 1 {
		t.Fatal("expected Rollback")
	}

	// Loop still alive — send another event and terminate normally.
	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_RollbackReloadError(t *testing.T) {
	h := newTestHarness()
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish always rejects")
	}
	wait := h.runAndWait(h.bcast)

	// Template change → rend.Reload ok → RenderToFile ok → mgr.Reload fails →
	// Rollback → retry RenderToFile ok → retry mgr.Reload fails again → continue
	h.templateCh <- struct{}{}
	time.Sleep(20 * time.Millisecond)

	// Loop still alive — terminate normally.
	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_SignalShutdown(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(h.bcast))))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	// Let the loop enter the signal handler before closing done.
	time.Sleep(20 * time.Millisecond)
	close(h.mgr.done) // varnishd exits cleanly
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}

	setCount, drainCount := h.bcast.counts()
	if drainCount < 1 {
		t.Fatalf("expected Drain to be called, got %d", drainCount)
	}
	_ = setCount

	sigs := h.mgr.getForwardedSigs()
	if len(sigs) == 0 {
		t.Fatal("expected signal to be forwarded to varnishd")
	}
	if sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected SIGTERM forwarded, got %v", sigs[0])
	}
}

func TestRunLoop_VarnishdUnexpectedExit(t *testing.T) {
	h := newTestHarness()
	h.mgr.err = errors.New("crashed")
	ctx, cancel := context.WithCancel(context.Background())
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
}

func TestRunLoop_RenderErrorNoRollbackWithoutTemplateChange(t *testing.T) {
	h := newTestHarness()
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint) (string, error) {
		return "", errors.New("render error")
	}
	wait := h.runAndWait(h.bcast)

	// Frontend update (not a template change) → RenderToFile fails → no Rollback.
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(20 * time.Millisecond)

	_, renderCount, rollbackCount := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile to be called")
	}
	if rollbackCount != 0 {
		t.Fatal("expected no Rollback on render error without template change")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_VarnishReloadErrorNoRollbackWithoutTemplateChange(t *testing.T) {
	h := newTestHarness()
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish reload error")
	}
	wait := h.runAndWait(h.bcast)

	// Frontend update → RenderToFile ok → mgr.Reload fails → no Rollback.
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(20 * time.Millisecond)

	if h.mgr.getReloadCount() < 1 {
		t.Fatal("expected mgr.Reload to be called")
	}
	_, _, rollbackCount := h.rend.counts()
	if rollbackCount != 0 {
		t.Fatal("expected no Rollback on reload error without template change")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_RetryRenderAfterRollbackFails(t *testing.T) {
	h := newTestHarness()
	var renderCalls atomic.Int32
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint) (string, error) {
		if renderCalls.Add(1) == 2 {
			return "", errors.New("render error after rollback")
		}
		return "test.vcl", nil
	}
	h.mgr.reloadFn = func(_ string) error {
		return errors.New("varnish reload error")
	}
	wait := h.runAndWait(h.bcast)

	// Template change → rend.Reload ok → RenderToFile ok (1st) →
	// mgr.Reload fails → Rollback → retry RenderToFile fails (2nd) → continue
	h.templateCh <- struct{}{}
	time.Sleep(20 * time.Millisecond)

	_, renderCount, rollbackCount := h.rend.counts()
	if renderCount < 2 {
		t.Fatalf("expected at least 2 RenderToFile calls, got %d", renderCount)
	}
	if rollbackCount < 1 {
		t.Fatal("expected Rollback")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestRunLoop_ShutdownTimeout(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.shutdownTimeout = 10 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send SIGTERM but do NOT close mgr.done — varnishd doesn't exit in time.
	h.sigCh <- syscall.SIGTERM
	<-done

	sigs := h.mgr.getForwardedSigs()
	if len(sigs) < 2 {
		t.Fatalf("expected at least 2 forwarded signals (SIGTERM + SIGKILL), got %v", sigs)
	}
	if sigs[0] != syscall.SIGTERM {
		t.Fatalf("expected first signal SIGTERM, got %v", sigs[0])
	}
	if sigs[1] != syscall.SIGKILL {
		t.Fatalf("expected second signal SIGKILL, got %v", sigs[1])
	}

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0 (mgr.Err is nil), got %d", result)
	}
}

func TestRunLoop_SignalShutdownVarnishError(t *testing.T) {
	h := newTestHarness()
	h.mgr.err = errors.New("exit status 1")
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(h.bcast))))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	time.Sleep(20 * time.Millisecond)
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 1 {
		t.Fatalf("expected exit 1, got %d", result)
	}
}

func TestRunLoop_DebounceCoalescing(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.debounce = 50 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Two rapid frontend updates within the debounce window → single reload.
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(5 * time.Millisecond)
	h.frontendCh <- []watcher.Frontend{
		{IP: "10.0.0.1", Port: 80, Name: "pod-1"},
		{IP: "10.0.0.2", Port: 80, Name: "pod-2"},
	}
	time.Sleep(100 * time.Millisecond) // wait for debounce to fire

	_, renderCount, _ := h.rend.counts()
	if renderCount != 1 {
		t.Fatalf("expected exactly 1 RenderToFile call (coalesced), got %d", renderCount)
	}
	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected exactly 1 mgr.Reload call (coalesced), got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(20 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_BroadcastDisabled(t *testing.T) {
	h := newTestHarness()
	// bcast is nil — should not panic.
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	go func() {
		code.Store(int32(runLoop(ctx, cancel, h.loopConfig(nil))))
		close(done)
	}()

	// Frontend update with nil bcast — no panic.
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(20 * time.Millisecond)

	// Signal shutdown with nil bcast — no panic.
	h.sigCh <- syscall.SIGTERM
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}
