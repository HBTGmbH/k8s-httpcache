package varnish

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- mock types ---

type mockRunner struct {
	mu      sync.Mutex
	startFn func(name string, args []string) (proc, error)
	runFn   func(name string, args []string) (string, error)
	calls   [][]string // records [name, arg1, arg2, ...] for each Run call
}

func (r *mockRunner) Start(name string, args []string) (proc, error) {
	return r.startFn(name, args)
}

func (r *mockRunner) Run(name string, args []string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	return r.runFn(name, args)
}

type mockProc struct {
	mu        sync.Mutex
	waitErr   error
	waitCh    chan struct{} // if non-nil, Wait blocks until closed
	signalled []os.Signal
	pid       int
}

func (p *mockProc) Wait() error {
	if p.waitCh != nil {
		<-p.waitCh
	}
	return p.waitErr
}

func (p *mockProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signalled = append(p.signalled, sig)
	p.mu.Unlock()
	return nil
}

func (p *mockProc) Pid() int { return p.pid }

// newTestManager creates a Manager wired with a mock runner for testing.
func newTestManager(r *mockRunner) *Manager {
	return &Manager{
		varnishdPath:   "/usr/sbin/varnishd",
		varnishadmPath: "/usr/bin/varnishadm",
		listenAddrs:    []string{":8080"},
		run:            r,
		done:           make(chan struct{}),
		AdminTimeout:   30 * time.Second,
	}
}

const varnishdVersionOutput = "varnishd (varnish-7.6.1 revision abc123)"

// --- tests ---

func TestStartArgs(t *testing.T) {
	var gotName string
	var gotArgs []string

	mp := &mockProc{pid: 42, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(name string, args []string) (proc, error) {
			gotName = name
			gotArgs = args
			return mp, nil
		},
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.listenAddrs = []string{":8080", ":8443"}
	m.extraArgs = []string{"-p", "default_ttl=3600"}
	err := m.Start("/etc/varnish/default.vcl")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if gotName != "/usr/sbin/varnishd" {
		t.Errorf("command name = %q, want /usr/sbin/varnishd", gotName)
	}

	// Expected: -F -a :8080 -a :8443 -f /etc/varnish/default.vcl -p default_ttl=3600
	want := []string{
		"-F",
		"-a", ":8080",
		"-a", ":8443",
		"-f", "/etc/varnish/default.vcl",
		"-p", "default_ttl=3600",
	}

	if len(gotArgs) != len(want) {
		t.Fatalf("args length = %d, want %d\ngot:  %v\nwant: %v", len(gotArgs), len(want), gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, gotArgs[i], want[i])
		}
	}
}

func TestStartAdminWait(t *testing.T) {
	pingCount := 0
	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}
			if slices.Contains(args, "ping") {
				pingCount++
				if pingCount < 3 {
					return "", errors.New("connection refused")
				}
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	err := m.Start("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if pingCount < 3 {
		t.Errorf("pingCount = %d, want >= 3", pingCount)
	}
}

func TestStartAdminTimeout(t *testing.T) {
	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "ping") {
				return "", errors.New("connection refused")
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	// Use a very short timeout by calling waitForAdmin directly.
	p, err := m.run.Start(m.varnishdPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.proc = p
	go func() {
		m.err = m.proc.Wait()
		close(m.done)
	}()

	err = m.waitForAdmin(500 * time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want substring 'timeout'", err.Error())
	}
}

func TestReloadSequence(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "200", nil },
	}

	m := newTestManager(r)

	// First reload
	if err := m.Reload("/tmp/vcl1.vcl"); err != nil {
		t.Fatalf("Reload 1 error: %v", err)
	}

	// Second reload
	if err := m.Reload("/tmp/vcl2.vcl"); err != nil {
		t.Fatalf("Reload 2 error: %v", err)
	}

	// Verify adm calls: each Reload does vcl.load, vcl.use, vcl.list, and possibly vcl.discard.
	// We check that the first two calls for each reload are vcl.load and vcl.use with correct names.
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()

	// Find vcl.load and vcl.use calls in order.
	var loadUseCalls [][]string
	for _, c := range calls {
		for i, arg := range c {
			if arg == "vcl.load" || arg == "vcl.use" {
				loadUseCalls = append(loadUseCalls, c[i:])
				break
			}
		}
	}

	expected := [][]string{
		{"vcl.load", "kv_reload_1", "/tmp/vcl1.vcl"},
		{"vcl.use", "kv_reload_1"},
		{"vcl.load", "kv_reload_2", "/tmp/vcl2.vcl"},
		{"vcl.use", "kv_reload_2"},
	}

	if len(loadUseCalls) != len(expected) {
		t.Fatalf("got %d vcl.load/vcl.use calls, want %d\ncalls: %v", len(loadUseCalls), len(expected), loadUseCalls)
	}
	for i, want := range expected {
		got := loadUseCalls[i]
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("call[%d] = %v, want %v", i, got, want)
		}
	}
}

func TestReloadLoadError(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				return "VCL compilation failed", errors.New("exit status 1")
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	err := m.Reload("/tmp/bad.vcl")
	if err == nil {
		t.Fatal("expected error from Reload, got nil")
	}
	if !strings.Contains(err.Error(), "vcl.load") {
		t.Errorf("error = %q, want substring 'vcl.load'", err.Error())
	}

	// Verify vcl.use was NOT called.
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		for _, a := range c {
			if a == "vcl.use" {
				t.Error("vcl.use was called after vcl.load failed")
			}
		}
	}
}

func TestReloadUseError(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.use") {
				return "VCL in use", errors.New("exit status 1")
			}
			return "200", nil
		},
	}

	m := newTestManager(r)

	err := m.Reload("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error from Reload, got nil")
	}
	if !strings.Contains(err.Error(), "vcl.use") {
		t.Errorf("error = %q, want substring 'vcl.use'", err.Error())
	}
}

func TestDiscardOldVCLs(t *testing.T) {
	vclListOutput := strings.Join([]string{
		"active      0 warm          0 boot",
		"available   0 warm          0 kv_reload_1",
		"available   0 warm          0 kv_reload_2",
		"available   0 warm          0 kv_reload_3",
	}, "\n")

	var discarded []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			for i, a := range args {
				if a == "vcl.list" {
					return vclListOutput, nil
				}
				if a == "vcl.discard" && i+1 < len(args) {
					discarded = append(discarded, args[i+1])
					return "", nil
				}
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	// Discard with kv_reload_3 as the current VCL.
	m.discardOldVCLs("kv_reload_3")

	// Should discard kv_reload_1 and kv_reload_2 but NOT boot (active) or kv_reload_3 (current).
	if len(discarded) != 2 {
		t.Fatalf("discarded %d VCLs, want 2: %v", len(discarded), discarded)
	}
	want := map[string]bool{"kv_reload_1": true, "kv_reload_2": true}
	for _, name := range discarded {
		if !want[name] {
			t.Errorf("unexpectedly discarded %q", name)
		}
	}
}

func TestForwardSignal(t *testing.T) {
	p := &mockProc{pid: 99}
	m := &Manager{proc: p}

	m.ForwardSignal(os.Interrupt)
	m.ForwardSignal(os.Kill)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.signalled) != 2 {
		t.Fatalf("signalled %d times, want 2", len(p.signalled))
	}
	if p.signalled[0] != os.Interrupt {
		t.Errorf("signal[0] = %v, want Interrupt", p.signalled[0])
	}
	if p.signalled[1] != os.Kill {
		t.Errorf("signal[1] = %v, want Kill", p.signalled[1])
	}
}

func TestForwardSignalNilProc(_ *testing.T) {
	m := &Manager{}
	// Should not panic.
	m.ForwardSignal(os.Interrupt)
}

func TestDebugLogging(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}
			return "200", nil
		},
	}

	m := newTestManager(r)

	// Test varnishd start logging.
	err := m.Start("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "/usr/sbin/varnishd") {
		t.Errorf("expected varnishd exec log, got: %s", output)
	}
	if !strings.Contains(output, "-F") {
		t.Errorf("expected -F flag in log, got: %s", output)
	}

	// Test varnishadm logging via Reload.
	buf.Reset()
	if err := m.Reload("/tmp/vcl1.vcl"); err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	output = buf.String()
	if !strings.Contains(output, "/usr/bin/varnishadm") {
		t.Errorf("expected varnishadm exec log, got: %s", output)
	}
	if !strings.Contains(output, "vcl.load") {
		t.Errorf("expected vcl.load in log, got: %s", output)
	}
	if !strings.Contains(output, "vcl.use") {
		t.Errorf("expected vcl.use in log, got: %s", output)
	}
}

func TestDebugLoggingDisabled(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}
			return "200", nil
		},
	}

	m := newTestManager(r)

	if err := m.Start("/tmp/test.vcl"); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := m.Reload("/tmp/vcl1.vcl"); err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, "level=DEBUG") {
		t.Errorf("expected no debug log lines when level is Info, got: %s", output)
	}
}

func TestNew(t *testing.T) {
	m := New("/usr/sbin/varnishd", "/usr/bin/varnishadm",
		[]string{":8080", ":8443"}, []string{"-p", "default_ttl=3600"}, "/usr/bin/varnishstat")

	if m.varnishdPath != "/usr/sbin/varnishd" {
		t.Errorf("varnishdPath = %q, want /usr/sbin/varnishd", m.varnishdPath)
	}
	if m.varnishadmPath != "/usr/bin/varnishadm" {
		t.Errorf("varnishadmPath = %q, want /usr/bin/varnishadm", m.varnishadmPath)
	}
	if m.varnishstatPath != "/usr/bin/varnishstat" {
		t.Errorf("varnishstatPath = %q, want /usr/bin/varnishstat", m.varnishstatPath)
	}
	if len(m.listenAddrs) != 2 {
		t.Errorf("listenAddrs length = %d, want 2", len(m.listenAddrs))
	}
	if len(m.extraArgs) != 2 {
		t.Errorf("extraArgs length = %d, want 2", len(m.extraArgs))
	}
	if m.done == nil {
		t.Error("done channel should not be nil")
	}
	if m.AdminTimeout != 30*time.Second {
		t.Errorf("AdminTimeout = %v, want 30s", m.AdminTimeout)
	}
}

func TestNewDefaultVarnishstatPath(t *testing.T) {
	m := New("/usr/sbin/varnishd", "/usr/bin/varnishadm",
		[]string{":8080"}, nil, "")

	if m.varnishstatPath != "varnishstat" {
		t.Errorf("varnishstatPath = %q, want default 'varnishstat'", m.varnishstatPath)
	}
}

func TestDoneAndErr(t *testing.T) {
	m := &Manager{done: make(chan struct{}), err: errors.New("test error")}

	// Done should return the channel.
	select {
	case <-m.Done():
		t.Fatal("done channel should not be closed yet")
	default:
	}

	// Err should return the stored error.
	if m.Err() == nil || m.Err().Error() != "test error" {
		t.Errorf("Err() = %v, want 'test error'", m.Err())
	}
}

func TestStartRunnerError(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			return nil, errors.New("exec failed")
		},
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	err := m.Start("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error from Start, got nil")
	}
	if !strings.Contains(err.Error(), "starting varnishd") {
		t.Errorf("error = %q, want substring 'starting varnishd'", err.Error())
	}
}

func TestWaitForAdminProcessExited(t *testing.T) {
	mp := &mockProc{pid: 1, waitErr: errors.New("exit status 1")}

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "ping") {
				return "", errors.New("connection refused")
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	m.proc = mp
	go func() {
		m.err = mp.Wait()
		close(m.done)
	}()

	err := m.waitForAdmin(5 * time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exited before admin") {
		t.Errorf("error = %q, want substring 'exited before admin'", err.Error())
	}
}

func TestDiscardOldVCLsListError(_ *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.list") {
				return "", errors.New("admin error")
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	// Should not panic — just return silently on error.
	m.discardOldVCLs("kv_reload_1")
}

func TestDiscardOldVCLsMalformedLines(t *testing.T) {
	vclListOutput := strings.Join([]string{
		"",                                    // empty line
		"   ",                                 // whitespace-only
		"short line",                          // fewer than 4 fields
		"a b",                                 // only 2 fields
		"available   0 warm          0 old_1", // valid
		"active      0 warm          0 boot",  // active, should be skipped
		"available",                           // single field
	}, "\n")

	var discarded []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			for i, a := range args {
				if a == "vcl.list" {
					return vclListOutput, nil
				}
				if a == "vcl.discard" && i+1 < len(args) {
					discarded = append(discarded, args[i+1])
					return "", nil
				}
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	// Should not panic despite malformed lines.
	m.discardOldVCLs("current")

	if len(discarded) != 1 {
		t.Fatalf("discarded %d VCLs, want 1: %v", len(discarded), discarded)
	}
	if discarded[0] != "old_1" {
		t.Errorf("discarded %q, want old_1", discarded[0])
	}
}

func TestDiscardOldVCLsEmptyOutput(t *testing.T) {
	var discardCalls int
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			for _, a := range args {
				if a == "vcl.list" {
					return "", nil
				}
				if a == "vcl.discard" {
					discardCalls++
					return "", nil
				}
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	m.discardOldVCLs("kv_reload_1")

	if discardCalls != 0 {
		t.Fatalf("expected 0 discard calls, got %d", discardCalls)
	}
}

func TestMarkBackendSick(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "200", nil },
	}

	m := newTestManager(r)

	if err := m.MarkBackendSick("drain_flag"); err != nil {
		t.Fatalf("MarkBackendSick() error: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Verify the correct varnishadm command was called.
	found := false
	for _, c := range r.calls {
		for i, arg := range c {
			if arg == "backend.set_health" && i+2 < len(c) && c[i+1] == "drain_flag" && c[i+2] == "sick" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("expected varnishadm backend.set_health drain_flag sick call, got %v", r.calls)
	}
}

func TestMarkBackendSickError(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "backend.set_health") {
				return "", errors.New("backend not found")
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	err := m.MarkBackendSick("nonexistent")
	if err == nil {
		t.Fatal("expected error from MarkBackendSick, got nil")
	}
}

func TestActiveSessions(t *testing.T) {
	varnishstatOutput := `{
		"version": 1,
		"counters": {
			"MEMPOOL.sess0.live": {"value": 5},
			"MEMPOOL.sess1.live": {"value": 3},
			"MEMPOOL.sess0.pool": {"value": 100},
			"MAIN.uptime": {"value": 12345}
		}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7

	sessions, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if sessions != 8 {
		t.Errorf("ActiveSessions() = %d, want 8", sessions)
	}
}

func TestActiveSessionsZero(t *testing.T) {
	varnishstatOutput := `{
		"version": 1,
		"counters": {
			"MEMPOOL.sess0.live": {"value": 0},
			"MEMPOOL.sess1.live": {"value": 0}
		}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7

	sessions, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if sessions != 0 {
		t.Errorf("ActiveSessions() = %d, want 0", sessions)
	}
}

func TestActiveSessionsNoCounters(t *testing.T) {
	varnishstatOutput := `{
		"version": 1,
		"counters": {
			"MAIN.uptime": {"value": 12345}
		}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7

	sessions, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if sessions != 0 {
		t.Errorf("ActiveSessions() = %d, want 0", sessions)
	}
}

func TestActiveSessionsError(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return "", errors.New("command failed")
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7

	_, err := m.ActiveSessions()
	if err == nil {
		t.Fatal("expected error from ActiveSessions, got nil")
	}
	if !strings.Contains(err.Error(), "varnishstat") {
		t.Errorf("error = %q, want substring 'varnishstat'", err.Error())
	}
}

func TestActiveSessionsInvalidJSON(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return "not valid json", nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7

	_, err := m.ActiveSessions()
	if err == nil {
		t.Fatal("expected error from ActiveSessions for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parsing varnishstat JSON") {
		t.Errorf("error = %q, want substring 'parsing varnishstat JSON'", err.Error())
	}
}

func TestActiveSessionsMalformedValue(t *testing.T) {
	// The "value" field is a string instead of a number.
	varnishstatOutput := `{
		"version": 1,
		"counters": {
			"MEMPOOL.sess0.live": {"value": "not_a_number"}
		}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7

	_, err := m.ActiveSessions()
	if err == nil {
		t.Fatal("expected error from ActiveSessions for malformed value, got nil")
	}
	if !strings.Contains(err.Error(), "parsing varnishstat JSON") {
		t.Errorf("error = %q, want substring 'parsing varnishstat JSON'", err.Error())
	}
}

func TestDiscardOldVCLsDiscardError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	vclListOutput := strings.Join([]string{
		"available   0 warm          0 old_1",
		"available   0 warm          0 old_2",
		"active      0 warm          0 boot",
	}, "\n")

	var discarded []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			for i, a := range args {
				if a == "vcl.list" {
					return vclListOutput, nil
				}
				if a == "vcl.discard" && i+1 < len(args) {
					name := args[i+1]
					discarded = append(discarded, name)
					if name == "old_1" {
						return "", errors.New("discard failed")
					}
					return "", nil
				}
			}
			return "", nil
		},
	}

	m := newTestManager(r)

	m.discardOldVCLs("current")

	// Should have attempted to discard both old_1 and old_2, not stopping on error.
	if len(discarded) != 2 {
		t.Fatalf("expected 2 discard attempts, got %d: %v", len(discarded), discarded)
	}

	// Verify a warning was logged for the failed discard.
	output := buf.String()
	if !strings.Contains(output, "failed to discard VCL") {
		t.Errorf("expected warning log for discard failure, got: %s", output)
	}
}

func TestDetectVersion(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		err     error
		wantVer int
		wantErr bool
	}{
		{
			name:    "varnish 7",
			output:  "varnishd (varnish-7.7.3 revision abc123)",
			wantVer: 7,
		},
		{
			name:    "varnish 6",
			output:  "varnishd (varnish-6.0.16 revision abc123)",
			wantVer: 6,
		},
		{
			name:    "varnish 8",
			output:  "varnishd (varnish-8.0.0 revision abc123)",
			wantVer: 8,
		},
		{
			name:    "unparseable output",
			output:  "some random output",
			wantErr: true,
		},
		{
			name:    "command failure",
			err:     errors.New("exec failed"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &mockRunner{
				startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
				runFn: func(string, []string) (string, error) {
					return tt.output, tt.err
				},
			}
			m := newTestManager(r)
			err := m.DetectVersion()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("detectVersion() error: %v", err)
			}
			if m.majorVersion != tt.wantVer {
				t.Errorf("majorVersion = %d, want %d", m.majorVersion, tt.wantVer)
			}
		})
	}
}

func TestActiveSessionsV6(t *testing.T) {
	varnishstatOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MEMPOOL.sess0.live": {"value": 5},
		"MEMPOOL.sess1.live": {"value": 3},
		"MEMPOOL.sess0.pool": {"value": 100},
		"MAIN.uptime": {"value": 12345}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 6

	sessions, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if sessions != 8 {
		t.Errorf("ActiveSessions() = %d, want 8", sessions)
	}
}

func TestActiveSessionsV6Zero(t *testing.T) {
	varnishstatOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MEMPOOL.sess0.live": {"value": 0},
		"MEMPOOL.sess1.live": {"value": 0}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == "varnishstat" {
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 6

	sessions, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if sessions != 0 {
		t.Errorf("ActiveSessions() = %d, want 0", sessions)
	}
}

func TestExtractWorkDir(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "separate args",
			args: []string{"-p", "default_ttl=3600", "-n", "/var/lib/varnish/myname"},
			want: "/var/lib/varnish/myname",
		},
		{
			name: "concatenated",
			args: []string{"-p", "default_ttl=3600", "-n/var/lib/varnish/myname"},
			want: "/var/lib/varnish/myname",
		},
		{
			name: "missing -n",
			args: []string{"-p", "default_ttl=3600"},
			want: "",
		},
		{
			name: "-n as last arg without value",
			args: []string{"-p", "default_ttl=3600", "-n"},
			want: "",
		},
		{
			name: "-n mixed with other flags",
			args: []string{"-s", "malloc,256m", "-n", "myinst", "-p", "thread_pool_min=5"},
			want: "myinst",
		},
		{
			name: "nil args",
			args: nil,
			want: "",
		},
		{
			name: "empty args",
			args: []string{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractWorkDir(tt.args)
			if got != tt.want {
				t.Errorf("extractWorkDir(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestActiveSessionsWithWorkDir(t *testing.T) {
	varnishstatOutput := `{
		"version": 1,
		"counters": {
			"MEMPOOL.sess0.live": {"value": 2}
		}
	}`

	var gotArgs []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, args []string) (string, error) {
			if name == "varnishstat" {
				gotArgs = args
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7
	m.workDir = "/var/lib/varnish/myname"

	_, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}

	want := []string{"-n", "/var/lib/varnish/myname", "-1", "-j"}
	if !slices.Equal(gotArgs, want) {
		t.Errorf("varnishstat args = %v, want %v", gotArgs, want)
	}
}

func TestActiveSessionsWithoutWorkDir(t *testing.T) {
	varnishstatOutput := `{
		"version": 1,
		"counters": {
			"MEMPOOL.sess0.live": {"value": 2}
		}
	}`

	var gotArgs []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, args []string) (string, error) {
			if name == "varnishstat" {
				gotArgs = args
				return varnishstatOutput, nil
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = "varnishstat"
	m.majorVersion = 7

	_, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}

	want := []string{"-1", "-j"}
	if !slices.Equal(gotArgs, want) {
		t.Errorf("varnishstat args = %v, want %v", gotArgs, want)
	}
}

func TestAdmArgsWithWorkDir(t *testing.T) {
	var gotArgs []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				gotArgs = args
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.workDir = "/var/lib/varnish/myinst"

	_ = m.Reload("/tmp/test.vcl")

	if len(gotArgs) < 3 {
		t.Fatalf("expected at least 3 args, got %v", gotArgs)
	}
	if gotArgs[0] != "-n" || gotArgs[1] != "/var/lib/varnish/myinst" {
		t.Errorf("expected args to start with [-n /var/lib/varnish/myinst], got %v", gotArgs[:2])
	}
	if !slices.Contains(gotArgs, "vcl.load") {
		t.Errorf("expected args to contain vcl.load, got %v", gotArgs)
	}
}

func TestAdmArgsWithoutWorkDir(t *testing.T) {
	var gotArgs []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				gotArgs = args
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	// workDir left empty

	_ = m.Reload("/tmp/test.vcl")

	if slices.Contains(gotArgs, "-n") {
		t.Errorf("expected no -n flag when workDir is empty, got %v", gotArgs)
	}
	if slices.Contains(gotArgs, "-T") || slices.Contains(gotArgs, "-S") {
		t.Errorf("expected no -T/-S flags, got %v", gotArgs)
	}
	if !slices.Contains(gotArgs, "vcl.load") {
		t.Errorf("expected args to contain vcl.load, got %v", gotArgs)
	}
}
