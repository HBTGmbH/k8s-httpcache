package main

import (
	"context"
	"errors"
	"maps"
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
	renderToFileFn func([]watcher.Frontend, map[string][]watcher.Endpoint, map[string]map[string]any) (string, error)
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

func (m *mockRenderer) RenderToFile(fe []watcher.Frontend, be map[string][]watcher.Endpoint, vals map[string]map[string]any) (string, error) {
	m.mu.Lock()
	m.renderCount++
	fn := m.renderToFileFn
	m.mu.Unlock()
	if fn != nil {
		return fn(fe, be, vals)
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
	mu               sync.Mutex
	reloadFn         func(string) error
	markBackendFn    func(string) error
	activeSessionsFn func() (int64, error)
	done             chan struct{}
	err              error
	reloadCount      int
	forwardedSigs    []os.Signal
	markSickCalls    []string
	sessionsCalls    int
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

func (m *mockManager) ActiveSessions() (int64, error) {
	m.mu.Lock()
	m.sessionsCalls++
	fn := m.activeSessionsFn
	m.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return 0, nil
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
	rend  *mockRenderer
	mgr   *mockManager
	bcast *mockBroadcast

	frontendCh chan []watcher.Frontend
	backendCh  chan backendChange
	valuesCh   chan valuesChange
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
		valuesCh:   make(chan valuesChange, 1),
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
		valuesCh:   h.valuesCh,
		templateCh: h.templateCh,
		sigCh:      h.sigCh,

		serviceName:           "test-svc",
		frontendDebounce:      1 * time.Millisecond,
		frontendDebounceMax:   0,
		backendDebounce:       1 * time.Millisecond,
		backendDebounceMax:    0,
		shutdownTimeout:       1 * time.Second,
		broadcastDrainTimeout: 1 * time.Second,

		drainPollInterval: 1 * time.Second,

		latestFrontends: nil,
		latestBackends:  make(map[string][]watcher.Endpoint),
		latestValues:    make(map[string]map[string]any),
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
	h.rend.renderToFileFn = func(fe []watcher.Frontend, _ map[string][]watcher.Endpoint, _ map[string]map[string]any) (string, error) {
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

func TestRunLoop_ValuesUpdateTriggersReload(t *testing.T) {
	h := newTestHarness()
	wait := h.runAndWait(h.bcast)

	h.valuesCh <- valuesChange{
		name: "tuning",
		data: map[string]any{"ttl": "300"},
	}
	time.Sleep(20 * time.Millisecond) // debounce

	_, renderCount, _ := h.rend.counts()
	if renderCount < 1 {
		t.Fatal("expected RenderToFile after values update")
	}
	if h.mgr.getReloadCount() < 1 {
		t.Fatal("expected mgr.Reload after values update")
	}

	code := wait()
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestValuesChanNil(t *testing.T) {
	ch := valuesChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil input")
	}
}

func TestValuesChanNonNil(t *testing.T) {
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
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint, _ map[string]map[string]any) (string, error) {
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
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint, _ map[string]map[string]any) (string, error) {
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
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint, _ map[string]map[string]any) (string, error) {
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
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint, _ map[string]map[string]any) (string, error) {
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
	lc.frontendDebounce = 50 * time.Millisecond
	lc.backendDebounce = 50 * time.Millisecond

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

func TestRunLoop_FileValuesWatcherUpdateTriggersRerender(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ttl.yaml", []byte("300"), 0o644); err != nil {
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
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint, vals map[string]map[string]any) (string, error) {
		// Deep-copy the values map to capture the state at render time.
		copied := make(map[string]map[string]any, len(vals))
		for k, v := range vals {
			inner := make(map[string]any, len(v))
			maps.Copy(inner, v)
			copied[k] = inner
		}
		renderMu.Lock()
		renderedVals = append(renderedVals, copied)
		renderMu.Unlock()
		return "test.vcl", nil
	}

	wait := h.runAndWait(h.bcast)

	// Modify the YAML file on disk.
	if err := os.WriteFile(dir+"/ttl.yaml", []byte("600"), 0o644); err != nil {
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

// --- Drain tests ---

func TestRunLoop_DrainOnShutdown(t *testing.T) {
	h := newTestHarness()
	var sessionCalls atomic.Int32
	h.mgr.activeSessionsFn = func() (int64, error) {
		// Return >0 for the first call, then 0.
		if sessionCalls.Add(1) == 1 {
			return 5, nil
		}
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	// Wait for the drain to complete and varnishd to be signalled.
	time.Sleep(100 * time.Millisecond)
	close(h.mgr.done) // varnishd exits cleanly
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
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	// drainBackend is "" — drain should be skipped.

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	time.Sleep(20 * time.Millisecond)
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
	h := newTestHarness()
	// ActiveSessions always returns >0 to keep polling.
	h.mgr.activeSessionsFn = func() (int64, error) {
		return 10, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	// Let it enter the poll loop.
	time.Sleep(50 * time.Millisecond)
	// Second signal interrupts drain.
	sigCh <- syscall.SIGINT
	time.Sleep(20 * time.Millisecond)
	close(h.mgr.done)
	<-done

	result := int(code.Load())
	if result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_DrainMarkSickError(t *testing.T) {
	h := newTestHarness()
	h.mgr.markBackendFn = func(_ string) error {
		return errors.New("backend not found")
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	time.Sleep(100 * time.Millisecond)
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
	h := newTestHarness()
	h.mgr.markBackendFn = func(_ string) error {
		return errors.New("backend not found")
	}
	var sessionCalls atomic.Int32
	h.mgr.activeSessionsFn = func() (int64, error) {
		// Return >0 first, then 0.
		if sessionCalls.Add(1) == 1 {
			return 5, nil
		}
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	time.Sleep(100 * time.Millisecond)
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
	h := newTestHarness()
	// ActiveSessions always returns >0 so sessions never clear.
	h.mgr.activeSessionsFn = func() (int64, error) {
		return 42, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	time.Sleep(2 * time.Second)
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

func TestRunLoop_DrainSecondSignalDuringPolling(t *testing.T) {
	h := newTestHarness()
	// ActiveSessions always returns >0 to keep polling.
	h.mgr.activeSessionsFn = func() (int64, error) {
		return 10, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	// Wait past drainDelay + first 1s ticker tick so polling has happened.
	time.Sleep(1200 * time.Millisecond)
	// Second signal during polling should abort the drain loop.
	sigCh <- syscall.SIGINT
	time.Sleep(50 * time.Millisecond)
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
	h := newTestHarness()
	var calls atomic.Int32
	h.mgr.activeSessionsFn = func() (int64, error) {
		n := calls.Add(1)
		if n <= 2 {
			// First two polls fail.
			return 0, errors.New("varnishstat unavailable")
		}
		// Third poll succeeds with 0 sessions.
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.drainBackend = drainBackendName
	lc.drainDelay = 10 * time.Millisecond
	lc.drainTimeout = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.sigCh <- syscall.SIGTERM
	time.Sleep(200 * time.Millisecond)
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
	h := newTestHarness()
	h.mgr.activeSessionsFn = func() (int64, error) {
		t.Fatal("ActiveSessions should not be called when drainTimeout is 0")
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	time.Sleep(100 * time.Millisecond)
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
	// Create a temp VCL file and start a real watchFile to prove the
	// file actually changes on disk.
	f, err := os.CreateTemp(t.TempDir(), "vcl-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	if _, err := f.WriteString("initial"); err != nil {
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
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
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
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ttl.yaml", []byte("300"), 0o644); err != nil {
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
	// Do NOT wire fvw.Changes() into valuesCh — simulates --file-watch=false.
	// The default valuesCh from newTestHarness is a buffered channel that
	// nobody sends to, so the select case never fires.

	// Set up the loop manually so we can seed latestValues.
	ctxLoop, cancel := context.WithCancel(context.Background())
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
	if err := os.WriteFile(dir+"/ttl.yaml", []byte("600"), 0o644); err != nil {
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
	time.Sleep(20 * time.Millisecond)
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestRunLoop_FileWatchDisabledValuesDirInitialStateAvailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/greeting.yaml", []byte("hello"), 0o644); err != nil {
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
	h.rend.renderToFileFn = func(_ []watcher.Frontend, _ map[string][]watcher.Endpoint, vals map[string]map[string]any) (string, error) {
		copied := make(map[string]map[string]any, len(vals))
		for k, v := range vals {
			inner := make(map[string]any, len(v))
			maps.Copy(inner, v)
			copied[k] = inner
		}
		renderMu.Lock()
		renderedVals = copied
		renderMu.Unlock()
		return "test.vcl", nil
	}

	// Do NOT wire fvw.Changes() into valuesCh — simulates --file-watch=false.

	// Set up loop manually to seed latestValues with initial directory state.
	ctxLoop, cancel := context.WithCancel(context.Background())
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
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(20 * time.Millisecond) // debounce

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
	time.Sleep(20 * time.Millisecond)
	close(h.mgr.done)
	<-done

	if result := int(code.Load()); result != 0 {
		t.Fatalf("expected exit 0, got %d", result)
	}
}

func TestDrainBackendForLoop(t *testing.T) {
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
// debounceMax is set but irrelevant — reload fires at the normal debounce time.
func TestRunLoop_DebounceMaxSingleEvent(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 150 * time.Millisecond
	lc.backendDebounce = 150 * time.Millisecond
	lc.frontendDebounceMax = 5 * time.Second // much larger — should not matter
	lc.backendDebounceMax = 5 * time.Second

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}

	// debounce fires at ~150ms; check at 500ms with generous margin.
	time.Sleep(500 * time.Millisecond)
	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected 1 reload, got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// Scenario 2: Brief burst of events, then quiet.
// Events stop well before debounceMax; reload fires at last-event + debounce.
func TestRunLoop_DebounceMaxBriefBurst(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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
		h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
		time.Sleep(30 * time.Millisecond)
	}

	// Last event at ~120ms + 200ms debounce = ~320ms.
	// Check at 600ms — well after expected fire.
	time.Sleep(500 * time.Millisecond)
	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected 1 coalesced reload after burst, got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// Scenario 3: Events arriving slower than debounce.
// Each event triggers its own reload; debounceMax has no effect.
func TestRunLoop_DebounceMaxSlowEvents(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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

	// Event #1 at t=0. Timer fires at ~150ms.
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(400 * time.Millisecond) // well past 150ms

	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected 1 reload after first event, got %d", h.mgr.getReloadCount())
	}

	// Event #2 at t=400ms. Timer fires at ~550ms.
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.2", Port: 80, Name: "pod-2"}}
	time.Sleep(400 * time.Millisecond)

	if h.mgr.getReloadCount() != 2 {
		t.Fatalf("expected 2 reloads after second event, got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// Scenario 4: Events arriving faster than debounce — the key case.
// Without debounceMax the timer perpetually resets and never fires.
// With debounceMax the reload is forced periodically.
func TestRunLoop_DebounceMaxForcesReload(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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

	// Send events every 20ms for 1.2s. debounce=250ms is perpetually reset.
	// debounceMax=250ms → forced reloads at ~250ms, ~500ms, ~750ms, ~1000ms.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(1200 * time.Millisecond)
	close(stop)

	// 1200ms / 250ms ≈ 4.8 windows; expect at least 3 reloads.
	reloads := h.mgr.getReloadCount()
	if reloads < 3 {
		t.Fatalf("expected at least 3 forced reloads, got %d", reloads)
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// Scenario 5: Long stream — deadline resets after each forced reload.
// Two separate bursts each get their own debounceMax window.
func TestRunLoop_DebounceMaxResetsAfterReload(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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
		h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let pending timer fire

	firstReloads := h.mgr.getReloadCount()
	if firstReloads < 1 {
		t.Fatal("expected at least 1 reload after first burst")
	}

	// Quiet gap — let the loop fully quiesce. No extra reload fires from
	// the first burst's trailing timer during this gap (it already fired
	// via debounceMax).

	// Second burst: new debounceMax window starts fresh.
	for range 30 {
		h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.2", Port: 80, Name: "pod-2"}}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	secondReloads := h.mgr.getReloadCount()
	if secondReloads <= firstReloads {
		t.Fatalf("expected additional reloads after second burst, got %d total (was %d after first)", secondReloads, firstReloads)
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// Scenario 6: debounceMax=0 (disabled) — perpetual timer reset, no forced reload.
func TestRunLoop_DebounceMaxDisabled(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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

	// Send events every 20ms for 600ms — timer keeps resetting.
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
			h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(600 * time.Millisecond)
	close(stop)

	// No reload during the stream — timer kept resetting.
	if h.mgr.getReloadCount() != 0 {
		t.Fatalf("expected 0 reloads during continuous events (debounceMax disabled), got %d", h.mgr.getReloadCount())
	}

	// After events stop, the last 800ms timer fires.
	// Last event at ~600ms + 800ms debounce = ~1400ms. Wait 1s from now (t≈1600ms).
	time.Sleep(1000 * time.Millisecond)
	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected 1 reload after events stop, got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// Verify debounceMax works for all event types, not just frontends.
// Uses backend, values, template, and mixed events in a single test to
// avoid duplicating the same pattern four times.
func TestRunLoop_DebounceMaxAllEventTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(h *testHarness)
	}{
		{
			name: "backend",
			send: func(h *testHarness) {
				h.backendCh <- backendChange{
					name:      "api",
					endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
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
			h := newTestHarness()
			ctx, cancel := context.WithCancel(context.Background())
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

			time.Sleep(600 * time.Millisecond)
			close(stop)

			if h.mgr.getReloadCount() < 1 {
				t.Fatalf("expected at least 1 reload from %s events with debounceMax", tc.name)
			}

			h.sigCh <- syscall.SIGTERM
			time.Sleep(50 * time.Millisecond)
			close(h.mgr.done)
			<-done
		})
	}
}

// Verify debounceMax deadline is shared across mixed event types:
// alternating frontend and backend events within one debounceMax window.
func TestRunLoop_DebounceMaxMixedEvents(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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
				h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
			} else {
				h.backendCh <- backendChange{
					name:      "api",
					endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
				}
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(600 * time.Millisecond)
	close(stop)

	if h.mgr.getReloadCount() < 1 {
		t.Fatal("expected at least 1 reload from mixed events with debounceMax")
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// --- Per-source debounce tests ---

func TestRunLoop_FrontendDebounceIndependentFromBackend(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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

	// Send frontend event → should reload at ~50ms.
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(200 * time.Millisecond)

	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected 1 reload after frontend event, got %d", h.mgr.getReloadCount())
	}

	// Send backend event → should NOT reload at ~200ms (only at ~500ms).
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}
	time.Sleep(200 * time.Millisecond)

	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected still 1 reload (backend timer not yet fired), got %d", h.mgr.getReloadCount())
	}

	// Wait for backend timer to fire.
	time.Sleep(400 * time.Millisecond)

	if h.mgr.getReloadCount() != 2 {
		t.Fatalf("expected 2 reloads after backend timer fires, got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_CrossGroupClearOnReload(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}

	// Frontend timer fires first (50ms), which clears the backend timer too.
	// Wait long enough for both timers to have fired if they were independent.
	time.Sleep(600 * time.Millisecond)

	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected exactly 1 reload (cross-group clear), got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_IndependentDebounceMaxGroups(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
	var code atomic.Int32
	code.Store(-1)
	done := make(chan struct{})
	lc := h.loopConfig(h.bcast)
	lc.frontendDebounce = 250 * time.Millisecond
	lc.frontendDebounceMax = 250 * time.Millisecond
	lc.backendDebounce = 250 * time.Millisecond
	lc.backendDebounceMax = 500 * time.Millisecond

	go func() {
		code.Store(int32(runLoop(ctx, cancel, lc)))
		close(done)
	}()

	// Send only frontend events rapidly for 1.2s.
	// frontendDebounceMax=250ms → forced reloads at ~250ms, ~500ms, etc.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(1200 * time.Millisecond)
	close(stop)

	// Frontend debounceMax=250ms → expect at least 3 reloads in 1.2s.
	reloads := h.mgr.getReloadCount()
	if reloads < 3 {
		t.Fatalf("expected at least 3 forced reloads from frontend events, got %d", reloads)
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_FrontendDebounceMaxDisabledBackendEnabled(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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

	// Rapid frontend events for 600ms — no forced reload (debounceMax=0).
	stopFE := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFE:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
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

	// Wait for the final frontend timer to fire.
	time.Sleep(1000 * time.Millisecond)
	afterFE := h.mgr.getReloadCount()
	if afterFE != 1 {
		t.Fatalf("expected 1 reload after frontend events stop, got %d", afterFE)
	}

	// Now send rapid backend events — backendDebounceMax=300ms → forced reload.
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
				endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(700 * time.Millisecond)
	close(stopBE)

	beReloads := h.mgr.getReloadCount()
	if beReloads <= afterFE {
		t.Fatalf("expected forced reloads from backend events with debounceMax=300ms, got %d total (was %d after frontend)", beReloads, afterFE)
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_BackendDebounceIndependentFromFrontend(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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

	// Send backend event → should reload at ~50ms.
	h.backendCh <- backendChange{
		name:      "api",
		endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
	}
	time.Sleep(200 * time.Millisecond)

	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected 1 reload after backend event, got %d", h.mgr.getReloadCount())
	}

	// Send frontend event → should NOT reload at ~200ms (only at ~500ms).
	h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	time.Sleep(200 * time.Millisecond)

	if h.mgr.getReloadCount() != 1 {
		t.Fatalf("expected still 1 reload (frontend timer not yet fired), got %d", h.mgr.getReloadCount())
	}

	// Wait for frontend timer to fire.
	time.Sleep(400 * time.Millisecond)

	if h.mgr.getReloadCount() != 2 {
		t.Fatalf("expected 2 reloads after frontend timer fires, got %d", h.mgr.getReloadCount())
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

func TestRunLoop_BackendDebounceMaxDisabledFrontendEnabled(t *testing.T) {
	h := newTestHarness()
	ctx, cancel := context.WithCancel(context.Background())
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

	// Rapid backend events for 600ms — no forced reload (debounceMax=0).
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
				endpoints: []watcher.Endpoint{{IP: "10.0.1.1", Port: 8080, Name: "api-0"}},
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

	// Wait for the final backend timer to fire.
	time.Sleep(1000 * time.Millisecond)
	afterBE := h.mgr.getReloadCount()
	if afterBE != 1 {
		t.Fatalf("expected 1 reload after backend events stop, got %d", afterBE)
	}

	// Now send rapid frontend events — frontendDebounceMax=300ms → forced reload.
	stopFE := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFE:
				return
			default:
			}
			h.frontendCh <- []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(700 * time.Millisecond)
	close(stopFE)

	feReloads := h.mgr.getReloadCount()
	if feReloads <= afterBE {
		t.Fatalf("expected forced reloads from frontend events with debounceMax=300ms, got %d total (was %d after backend)", feReloads, afterBE)
	}

	h.sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(h.mgr.done)
	<-done
}

// --- resetDebounce unit tests ---

func TestResetDebounce_SetsDeadlineOnFirstCall(t *testing.T) {
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
	var s debounceState
	// Set a deadline 50ms from now.
	s.deadline = time.Now().Add(50 * time.Millisecond)

	// Request a 500ms debounce — should be capped to ~50ms.
	resetDebounce(&s, 500*time.Millisecond, 100*time.Millisecond)
	defer s.timer.Stop()

	// The timer should fire within ~100ms (50ms deadline + margin).
	select {
	case <-s.timer.C:
		// OK — fired at the deadline
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timer did not fire at deadline")
	}
}

func TestResetDebounce_NoDeadlineWhenMaxIsZero(t *testing.T) {
	var s debounceState
	resetDebounce(&s, 100*time.Millisecond, 0)
	defer s.timer.Stop()

	if !s.deadline.IsZero() {
		t.Fatal("expected deadline to remain zero when debounceMax is 0")
	}
}
