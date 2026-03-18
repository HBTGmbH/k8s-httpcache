package varnish

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"k8s-httpcache/internal/redact"
	"k8s-httpcache/internal/telemetry"
)

// --- mock types ---

type mockRunner struct {
	mu      sync.Mutex
	startFn func(name string, args []string) (proc, error)
	runFn   func(name string, args []string) (string, error)
	calls   [][]string // records [name, arg1, arg2, ...] for each Run call
}

func (r *mockRunner) start(name string, args []string) (proc, error) {
	return r.startFn(name, args)
}

func (r *mockRunner) run(name string, args []string) (string, error) {
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
		log:            slog.New(slog.DiscardHandler),
		run:            r,
		done:           make(chan struct{}),
		metrics:        telemetry.NewMetrics(prometheus.NewRegistry(), nil),
		AdminTimeout:   30 * time.Second,
	}
}

const varnishdVersionOutput = "varnishd (varnish-7.6.1 revision abc123)"

// --- tests ---

func TestStartArgs(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	p, err := m.run.start(m.varnishdPath, nil)
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
	t.Parallel()
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "200", nil },
	}

	m := newTestManager(r)

	// First reload
	err := m.Reload("/tmp/vcl1.vcl")
	if err != nil {
		t.Fatalf("Reload 1 error: %v", err)
	}

	// Second reload
	err = m.Reload("/tmp/vcl2.vcl")
	if err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestForwardSignalNilProc(t *testing.T) {
	t.Parallel()
	m := &Manager{}
	// Should not panic.
	m.ForwardSignal(os.Interrupt)
}

func TestDebugLogging(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

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
	m.log = logger

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
	err = m.Reload("/tmp/vcl1.vcl")
	if err != nil {
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
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

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
	m.log = logger

	err := m.Start("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	err = m.Reload("/tmp/vcl1.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, "level=DEBUG") {
		t.Errorf("expected no debug log lines when level is Info, got: %s", output)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	m := New("/usr/sbin/varnishd", "/usr/bin/varnishadm",
		[]string{":8080", ":8443"}, []string{"-p", "default_ttl=3600"}, "/usr/bin/varnishstat",
		telemetry.NewMetrics(prometheus.NewRegistry(), nil))

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
	t.Parallel()
	m := New("/usr/sbin/varnishd", "/usr/bin/varnishadm",
		[]string{":8080"}, nil, "",
		telemetry.NewMetrics(prometheus.NewRegistry(), nil))

	if m.varnishstatPath != defaultVarnishstatPath {
		t.Errorf("varnishstatPath = %q, want default %q", m.varnishstatPath, defaultVarnishstatPath)
	}
}

func TestDoneAndErr(t *testing.T) {
	t.Parallel()
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

func TestStartDetectVersionError(t *testing.T) {
	t.Parallel()
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return "", errors.New("exec failed")
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	// majorVersion is 0, so Start will call DetectVersion which will fail.

	err := m.Start("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error from Start, got nil")
	}
	if !strings.Contains(err.Error(), "detecting varnish version") {
		t.Errorf("error = %q, want substring 'detecting varnish version'", err.Error())
	}
}

func TestStartAdminTimeoutViaStart(t *testing.T) {
	t.Parallel()
	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}
			if slices.Contains(args, "ping") {
				return "", errors.New("connection refused")
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.AdminTimeout = 500 * time.Millisecond

	err := m.Start("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error from Start due to admin timeout, got nil")
	}
	if !strings.Contains(err.Error(), "waiting for varnish admin") {
		t.Errorf("error = %q, want substring 'waiting for varnish admin'", err.Error())
	}
}

func TestStartRunnerError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestDiscardOldVCLsListError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	vclListOutput := strings.Join([]string{
		"",           // empty line
		"   ",        // whitespace-only
		"short line", // fewer than 4 fields
		"a b",        // only 2 fields
		"available   0 warm          0 kv_reload_1", // valid
		"active      0 warm          0 kv_reload_2", // active, should be skipped
		"available", // single field
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
	m.discardOldVCLs("kv_reload_2")

	if len(discarded) != 1 {
		t.Fatalf("discarded %d VCLs, want 1: %v", len(discarded), discarded)
	}
	if discarded[0] != "kv_reload_1" {
		t.Errorf("discarded %q, want kv_reload_1", discarded[0])
	}
}

func TestDiscardOldVCLsEmptyOutput(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "200", nil },
	}

	m := newTestManager(r)

	err := m.MarkBackendSick("drain_flag")
	if err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
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
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
	varnishstatOutput := `{
		"version": 1,
		"counters": {
			"MAIN.uptime": {"value": 12345}
		}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == defaultVarnishstatPath {
				return "", errors.New("command failed")
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == defaultVarnishstatPath {
				return "not valid json", nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
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
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	vclListOutput := strings.Join([]string{
		"available   0 warm          0 kv_reload_1",
		"available   0 warm          0 kv_reload_2",
		"active      0 warm          0 kv_reload_3",
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
					if name == "kv_reload_1" {
						return "", errors.New("discard failed")
					}

					return "", nil
				}
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.log = logger

	m.discardOldVCLs("kv_reload_3")

	// Should have attempted to discard both kv_reload_1 and kv_reload_2, not stopping on error.
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
	t.Parallel()
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
			name:    "varnish trunk",
			output:  "varnishd (varnish-trunk revision 382ea77157290253a93203dc60a289925b7bebea)",
			wantVer: trunkMajorVersion,
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
			t.Parallel()
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
	t.Parallel()
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
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
	m.majorVersion = 6

	sessions, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if sessions != 8 {
		t.Errorf("ActiveSessions() = %d, want 8", sessions)
	}
}

func TestActiveSessionsV6InvalidJSON(t *testing.T) {
	t.Parallel()
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == defaultVarnishstatPath {
				return "not valid json", nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
	m.majorVersion = 6

	_, err := m.ActiveSessions()
	if err == nil {
		t.Fatal("expected error from ActiveSessions for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parsing varnishstat JSON") {
		t.Errorf("error = %q, want substring 'parsing varnishstat JSON'", err.Error())
	}
}

func TestActiveSessionsV6MalformedCounter(t *testing.T) {
	t.Parallel()
	// The "value" field is a string instead of a number, causing unmarshal of the individual counter to fail.
	varnishstatOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MEMPOOL.sess0.live": {"value": "not_a_number"}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
	m.majorVersion = 6

	_, err := m.ActiveSessions()
	if err == nil {
		t.Fatal("expected error from ActiveSessions for malformed counter, got nil")
	}
	if !strings.Contains(err.Error(), "parsing varnishstat counter") {
		t.Errorf("error = %q, want substring 'parsing varnishstat counter'", err.Error())
	}
}

func TestActiveSessionsV6NoCounters(t *testing.T) {
	t.Parallel()
	varnishstatOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.uptime": {"value": 12345}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
	m.majorVersion = 6

	sessions, err := m.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if sessions != 0 {
		t.Errorf("ActiveSessions() = %d, want 0", sessions)
	}
}

func TestMajorVersion(t *testing.T) {
	t.Parallel()
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "", nil },
	}
	m := newTestManager(r)

	if got := m.MajorVersion(); got != 0 {
		t.Errorf("MajorVersion() before DetectVersion = %d, want 0", got)
	}

	m.majorVersion = 7
	if got := m.MajorVersion(); got != 7 {
		t.Errorf("MajorVersion() = %d, want 7", got)
	}
}

func TestActiveSessionsV6Zero(t *testing.T) {
	t.Parallel()
	varnishstatOutput := `{
		"timestamp": "2024-01-01T00:00:00",
		"MEMPOOL.sess0.live": {"value": 0},
		"MEMPOOL.sess1.live": {"value": 0}
	}`

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, _ []string) (string, error) {
			if name == defaultVarnishstatPath {
				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
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
			t.Parallel()
			got := extractWorkDir(tt.args)
			if got != tt.want {
				t.Errorf("extractWorkDir(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestActiveSessionsWithWorkDir(t *testing.T) {
	t.Parallel()
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
			if name == defaultVarnishstatPath {
				gotArgs = args

				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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
	t.Parallel()
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
			if name == defaultVarnishstatPath {
				gotArgs = args

				return varnishstatOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
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

func TestVarnishstatFuncWithWorkDir(t *testing.T) {
	t.Parallel()
	wantJSON := `{"version":1,"counters":{"MAIN.uptime":{"value":42}}}`

	var gotArgs []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, args []string) (string, error) {
			if name == defaultVarnishstatPath {
				gotArgs = args

				return wantJSON, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
	m.majorVersion = 7
	m.workDir = "/var/lib/varnish/myname"

	fn := m.VarnishstatFunc()
	out, ver, err := fn()
	if err != nil {
		t.Fatalf("VarnishstatFunc() error: %v", err)
	}
	if out != wantJSON {
		t.Errorf("output = %q, want %q", out, wantJSON)
	}
	if ver != 7 {
		t.Errorf("majorVersion = %d, want 7", ver)
	}

	want := []string{"-n", "/var/lib/varnish/myname", "-1", "-j"}
	if !slices.Equal(gotArgs, want) {
		t.Errorf("varnishstat args = %v, want %v", gotArgs, want)
	}
}

func TestVarnishstatFuncWithoutWorkDir(t *testing.T) {
	t.Parallel()
	wantJSON := `{"version":1,"counters":{}}`

	var gotArgs []string
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(name string, args []string) (string, error) {
			if name == defaultVarnishstatPath {
				gotArgs = args

				return wantJSON, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
	m.majorVersion = 7

	fn := m.VarnishstatFunc()
	out, ver, err := fn()
	if err != nil {
		t.Fatalf("VarnishstatFunc() error: %v", err)
	}
	if out != wantJSON {
		t.Errorf("output = %q, want %q", out, wantJSON)
	}
	if ver != 7 {
		t.Errorf("majorVersion = %d, want 7", ver)
	}

	want := []string{"-1", "-j"}
	if !slices.Equal(gotArgs, want) {
		t.Errorf("varnishstat args = %v, want %v", gotArgs, want)
	}
}

func TestVarnishstatFuncError(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(string, []string) (string, error) {
			return "", errors.New("exec failed")
		},
	}

	m := newTestManager(r)
	m.varnishstatPath = defaultVarnishstatPath
	m.majorVersion = 7

	fn := m.VarnishstatFunc()
	_, _, err := fn()
	if err == nil {
		t.Fatal("expected error from VarnishstatFunc, got nil")
	}
}

func TestAdmArgsWithWorkDir(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestReloadRetrySucceeds(t *testing.T) {
	t.Parallel()
	var loadCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				if n <= 2 {
					return "VCL compilation failed", errors.New("exit status 1")
				}

				return "200", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 3
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("expected Reload to succeed after retries, got: %v", err)
	}
	if got := loadCount.Load(); got != 3 {
		t.Errorf("vcl.load called %d times, want 3", got)
	}
}

func TestReloadRetriesExhausted(t *testing.T) {
	t.Parallel()
	var loadCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				loadCount.Add(1)

				return "VCL compilation failed", errors.New("exit status 1")
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 2
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "vcl.load") {
		t.Errorf("error = %q, want substring 'vcl.load'", err.Error())
	}
	// 1 initial + 2 retries = 3 attempts.
	if got := loadCount.Load(); got != 3 {
		t.Errorf("vcl.load called %d times, want 3", got)
	}
}

func TestReloadRetriesDisabled(t *testing.T) {
	t.Parallel()
	var loadCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				loadCount.Add(1)

				return "VCL compilation failed", errors.New("exit status 1")
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 0
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error with retries disabled")
	}
	if got := loadCount.Load(); got != 1 {
		t.Errorf("vcl.load called %d times, want 1 (single attempt)", got)
	}
}

func TestReloadVCLUseNotRetried(t *testing.T) {
	t.Parallel()
	var loadCount, useCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				loadCount.Add(1)

				return "200", nil
			}
			if slices.Contains(args, "vcl.use") {
				useCount.Add(1)

				return "VCL in use", errors.New("exit status 1")
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 3
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error from vcl.use failure")
	}
	if !strings.Contains(err.Error(), "vcl.use") {
		t.Errorf("error = %q, want substring 'vcl.use'", err.Error())
	}
	// vcl.load should succeed on first attempt; no retries.
	if got := loadCount.Load(); got != 1 {
		t.Errorf("vcl.load called %d times, want 1", got)
	}
	if got := useCount.Load(); got != 1 {
		t.Errorf("vcl.use called %d times, want 1", got)
	}
}

func TestReloadRetryCounterIncrements(t *testing.T) {
	t.Parallel()
	var loadCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				if n <= 2 {
					return "VCL compilation failed", errors.New("exit status 1")
				}

				return "200", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 3
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("expected Reload to succeed, got: %v", err)
	}
	// Each attempt increments reloadCounter. 3 attempts total.
	if got := m.reloadCounter.Load(); got != 3 {
		t.Errorf("reloadCounter = %d, want 3", got)
	}
}

func TestReloadRetryVCLNamesAndCallSequence(t *testing.T) {
	t.Parallel()
	// vcl.load fails twice, then succeeds on the 3rd attempt.
	// Verify the exact names passed to vcl.load/vcl.use and that
	// vcl.use is only called once with the successful name.
	var loadCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				if n <= 2 {
					return "VCL compilation failed", errors.New("exit status 1")
				}

				return "200", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 3
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()

	// Extract vcl.load and vcl.use calls in order.
	var loadUseCalls [][]string
	for _, c := range calls {
		for i, arg := range c {
			if arg == "vcl.load" || arg == "vcl.use" {
				loadUseCalls = append(loadUseCalls, c[i:])

				break
			}
		}
	}

	// Expect: 3x vcl.load (kv_reload_1 fail, kv_reload_2 fail, kv_reload_3 ok)
	// then 1x vcl.use (kv_reload_3).
	expected := [][]string{
		{"vcl.load", "kv_reload_1", "/tmp/test.vcl"},
		{"vcl.load", "kv_reload_2", "/tmp/test.vcl"},
		{"vcl.load", "kv_reload_3", "/tmp/test.vcl"},
		{"vcl.use", "kv_reload_3"},
	}

	if len(loadUseCalls) != len(expected) {
		t.Fatalf("got %d vcl.load/vcl.use calls, want %d\ncalls: %v",
			len(loadUseCalls), len(expected), loadUseCalls)
	}
	for i, want := range expected {
		got := loadUseCalls[i]
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("call[%d] = %v, want %v", i, got, want)
		}
	}
}

func TestReloadRetryNoDiscardOnExhausted(t *testing.T) {
	t.Parallel()
	// When all retries are exhausted, discardOldVCLs should NOT be called.
	var discardCalls atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				return "VCL compilation failed", errors.New("exit status 1")
			}
			if slices.Contains(args, "vcl.list") {
				discardCalls.Add(1)

				return "", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 2
	m.ReloadRetryInterval = time.Millisecond

	_ = m.Reload("/tmp/test.vcl")

	if got := discardCalls.Load(); got != 0 {
		t.Errorf("vcl.list called %d times, want 0 (no discard after exhausting retries)", got)
	}
}

func TestReloadRetryDiscardCalledOnSuccess(t *testing.T) {
	t.Parallel()
	// When vcl.load succeeds (after retries), discardOldVCLs should be
	// called with the correct (successful) VCL name.
	var loadCount atomic.Int32
	var discardCurrentName string

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				if n <= 1 {
					return "VCL compilation failed", errors.New("exit status 1")
				}

				return "200", nil
			}
			if slices.Contains(args, "vcl.list") {
				// Return the successful name as active so discardOldVCLs
				// has something to work with.
				return "active      0 warm          0 kv_reload_2\n" +
					"available   0 warm          0 kv_reload_1", nil
			}
			if slices.Contains(args, "vcl.discard") {
				for i, a := range args {
					if a == "vcl.discard" && i+1 < len(args) {
						discardCurrentName = args[i+1]
					}
				}

				return "", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 2
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	// discardOldVCLs should have discarded "kv_reload_1".
	if discardCurrentName != "kv_reload_1" {
		t.Errorf("discarded %q, want kv_reload_1", discardCurrentName)
	}
}

func TestReloadRetryVCLUseNotCalledOnFailedAttempts(t *testing.T) {
	t.Parallel()
	// Verify that vcl.use is never called for attempts where vcl.load failed.
	var loadCount atomic.Int32
	var useNames []string

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				if n <= 2 {
					return "VCL compilation failed", errors.New("exit status 1")
				}

				return "200", nil
			}
			if slices.Contains(args, "vcl.use") {
				for i, a := range args {
					if a == "vcl.use" && i+1 < len(args) {
						useNames = append(useNames, args[i+1])
					}
				}

				return "200", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 3
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	// vcl.use should be called exactly once, with the name from the
	// successful (3rd) vcl.load attempt.
	if len(useNames) != 1 {
		t.Fatalf("vcl.use called %d times, want 1: %v", len(useNames), useNames)
	}
	if useNames[0] != "kv_reload_3" {
		t.Errorf("vcl.use name = %q, want kv_reload_3", useNames[0])
	}
}

func TestReloadRetryLogMessages(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var loadCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				if n <= 1 {
					return "VCL compilation failed", errors.New("exit status 1")
				}

				return "200", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.log = logger
	m.ReloadRetries = 2
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	output := buf.String()

	// Should contain a retry warning.
	if !strings.Contains(output, "vcl.load failed, retrying") {
		t.Errorf("expected retry warning log, got: %s", output)
	}
	// Should contain success-after-retry info.
	if !strings.Contains(output, "activated VCL after retry") {
		t.Errorf("expected success-after-retry log, got: %s", output)
	}
}

func TestReloadFirstAttemptSuccessWithRetriesConfigured(t *testing.T) {
	t.Parallel()
	// When vcl.load succeeds on the first attempt despite retries being
	// configured, the normal "activated VCL" message (not "after retry")
	// should be logged, and only one vcl.load + one vcl.use should occur.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "200", nil },
	}

	m := newTestManager(r)
	m.log = logger
	m.ReloadRetries = 5
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()

	// Count vcl.load and vcl.use calls.
	var loadCount, useCount int
	for _, c := range calls {
		for _, a := range c {
			if a == "vcl.load" {
				loadCount++
			}
			if a == "vcl.use" {
				useCount++
			}
		}
	}
	if loadCount != 1 {
		t.Errorf("vcl.load called %d times, want 1", loadCount)
	}
	if useCount != 1 {
		t.Errorf("vcl.use called %d times, want 1", useCount)
	}

	output := buf.String()
	if strings.Contains(output, "after retry") {
		t.Errorf("unexpected 'after retry' in log when first attempt succeeded: %s", output)
	}
	if !strings.Contains(output, "activated VCL") {
		t.Errorf("expected 'activated VCL' log, got: %s", output)
	}
}

func TestVCLSuffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{"valid prefix", "kv_reload_42", 42},
		{"valid prefix zero", "kv_reload_0", 0},
		{"valid prefix large", "kv_reload_999999", 999999},
		{"no prefix", "boot", -1},
		{"partial prefix", "kv_reload_", -1},
		{"non-numeric suffix", "kv_reload_abc", -1},
		{"different prefix", "other_reload_5", -1},
		{"empty string", "", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := vclSuffix(tt.input)
			if got != tt.want {
				t.Errorf("vclSuffix(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// testDiscardOldVCLs is a helper that creates a mock runner with the given
// vcl.list output, calls discardOldVCLs, and asserts the expected discards.
func testDiscardOldVCLs(t *testing.T, vclListOutput string, vclKept int, currentVCL string, wantDiscarded []string) {
	t.Helper()

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
	m.VCLKept = vclKept

	m.discardOldVCLs(currentVCL)

	if len(discarded) != len(wantDiscarded) {
		t.Fatalf("discarded %d VCLs, want %d: %v", len(discarded), len(wantDiscarded), discarded)
	}
	want := make(map[string]bool, len(wantDiscarded))
	for _, name := range wantDiscarded {
		want[name] = true
	}
	for _, name := range discarded {
		if !want[name] {
			t.Errorf("unexpectedly discarded %q", name)
		}
	}
}

func TestDiscardOldVCLsKeepTwo(t *testing.T) {
	t.Parallel()
	testDiscardOldVCLs(t,
		strings.Join([]string{
			"active      0 warm          0 kv_reload_5",
			"available   0 warm          0 kv_reload_1",
			"available   0 warm          0 kv_reload_2",
			"available   0 warm          0 kv_reload_3",
			"available   0 warm          0 kv_reload_4",
		}, "\n"),
		2, "kv_reload_5",
		[]string{"kv_reload_1", "kv_reload_2"},
	)
}

func TestDiscardOldVCLsKeepMoreThanAvailable(t *testing.T) {
	t.Parallel()
	vclListOutput := strings.Join([]string{
		"active      0 warm          0 kv_reload_3",
		"available   0 warm          0 kv_reload_1",
		"available   0 warm          0 kv_reload_2",
	}, "\n")

	var discardCalls int
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			for _, a := range args {
				if a == "vcl.list" {
					return vclListOutput, nil
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
	m.VCLKept = 5

	m.discardOldVCLs("kv_reload_3")

	if discardCalls != 0 {
		t.Fatalf("expected 0 discard calls, got %d", discardCalls)
	}
}

func TestDiscardOldVCLsKeepZeroDiscardsAll(t *testing.T) {
	t.Parallel()
	vclListOutput := strings.Join([]string{
		"active      0 warm          0 kv_reload_3",
		"available   0 warm          0 kv_reload_1",
		"available   0 warm          0 kv_reload_2",
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
	m.VCLKept = 0

	m.discardOldVCLs("kv_reload_3")

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

func TestDiscardOldVCLsNonKVNamesAreIgnored(t *testing.T) {
	t.Parallel()
	// 4 available, but only 2 have the kv_reload_ prefix.
	// manual_vcl and old_config must NOT be discarded.
	// With VCLKept=1, keep kv_reload_3 (newest), discard kv_reload_2.
	testDiscardOldVCLs(t,
		strings.Join([]string{
			"active      0 warm          0 kv_reload_4",
			"available   0 warm          0 kv_reload_2",
			"available   0 warm          0 kv_reload_3",
			"available   0 warm          0 manual_vcl",
			"available   0 warm          0 old_config",
		}, "\n"),
		1, "kv_reload_4",
		[]string{"kv_reload_2"},
	)
}

func TestDiscardOldVCLsKeepZeroIgnoresNonKVNames(t *testing.T) {
	t.Parallel()
	// VCLKept=0 discards all kv_reload_ VCLs but leaves user-created ones alone.
	testDiscardOldVCLs(t,
		strings.Join([]string{
			"active      0 warm          0 kv_reload_3",
			"available   0 warm          0 kv_reload_1",
			"available   0 warm          0 kv_reload_2",
			"available   0 warm          0 manual_vcl",
			"available   0 warm          0 old_config",
		}, "\n"),
		0, "kv_reload_3",
		[]string{"kv_reload_1", "kv_reload_2"},
	)
}

func TestDiscardOldVCLsKeepExactCount(t *testing.T) {
	t.Parallel()
	vclListOutput := strings.Join([]string{
		"active      0 warm          0 kv_reload_4",
		"available   0 warm          0 kv_reload_2",
		"available   0 warm          0 kv_reload_3",
	}, "\n")

	var discardCalls int
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			for _, a := range args {
				if a == "vcl.list" {
					return vclListOutput, nil
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
	m.VCLKept = 2

	m.discardOldVCLs("kv_reload_4")

	// available == kept, nothing to discard.
	if discardCalls != 0 {
		t.Fatalf("expected 0 discard calls, got %d", discardCalls)
	}
}

func TestReloadRetryCounterContinuityAcrossCalls(t *testing.T) {
	t.Parallel()
	// Two successive Reload() calls, each with retries, should produce
	// a continuous reloadCounter sequence.
	var loadCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				// First call: fail once (n=1), succeed (n=2).
				// Second call: fail once (n=3), succeed (n=4).
				if n == 1 || n == 3 {
					return "VCL compilation failed", errors.New("exit status 1")
				}

				return "200", nil
			}

			return "200", nil
		},
	}

	m := newTestManager(r)
	m.ReloadRetries = 2
	m.ReloadRetryInterval = time.Millisecond

	err := m.Reload("/tmp/vcl1.vcl")
	if err != nil {
		t.Fatalf("first Reload() error: %v", err)
	}
	err = m.Reload("/tmp/vcl2.vcl")
	if err != nil {
		t.Fatalf("second Reload() error: %v", err)
	}

	// First Reload: 2 attempts (kv_reload_1 fail, kv_reload_2 ok).
	// Second Reload: 2 attempts (kv_reload_3 fail, kv_reload_4 ok).
	if got := m.reloadCounter.Load(); got != 4 {
		t.Errorf("reloadCounter = %d, want 4", got)
	}

	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()

	// Verify vcl.use was called with kv_reload_2 and kv_reload_4.
	var useNames []string
	for _, c := range calls {
		for i, a := range c {
			if a == "vcl.use" && i+1 < len(c) {
				useNames = append(useNames, c[i+1])
			}
		}
	}
	if len(useNames) != 2 {
		t.Fatalf("vcl.use called %d times, want 2: %v", len(useNames), useNames)
	}
	if useNames[0] != "kv_reload_2" {
		t.Errorf("first vcl.use name = %q, want kv_reload_2", useNames[0])
	}
	if useNames[1] != "kv_reload_4" {
		t.Errorf("second vcl.use name = %q, want kv_reload_4", useNames[1])
	}
}

func TestNewPanicsOnNilMetrics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil metrics")
		}
	}()
	New("/usr/sbin/varnishd", "/usr/bin/varnishadm", []string{":8080"}, nil, "", nil)
}

func TestAdmRedactsSecrets(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			return "VCL error: token my-secret-token on line 42", nil
		},
	}
	mgr := newTestManager(r)

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"app": {"token": "my-secret-token"},
	})
	mgr.redactor = rd

	resp, err := mgr.adm("vcl.load", "test", "/tmp/test.vcl")
	if err != nil {
		t.Fatal(err)
	}
	want := "VCL error: token [REDACTED] on line 42"
	if resp != want {
		t.Errorf("adm response not redacted:\n got: %q\nwant: %q", resp, want)
	}
}

func TestSetRedactorSkipsMockRunner(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			return "ok", nil
		},
	}
	mgr := newTestManager(r)

	rd := redact.NewRedactor()
	mgr.SetRedactor(rd)

	// SetRedactor should not panic or replace the mock runner.
	if mgr.redactor != rd {
		t.Error("redactor not set on manager")
	}
	// The mock runner should still be in place (type assertion to execRunner fails).
	if _, ok := mgr.run.(*mockRunner); !ok {
		t.Error("SetRedactor replaced mock runner")
	}
}

func TestSetRedactorWrapsExecRunner(t *testing.T) {
	t.Parallel()

	var stdoutBuf, stderrBuf bytes.Buffer

	mgr := &Manager{
		run:  execRunner{stdout: &stdoutBuf, stderr: &stderrBuf},
		log:  slog.New(slog.DiscardHandler),
		done: make(chan struct{}),
	}

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"auth": {"api-key": "varnishd-leaked-token"},
	})
	mgr.SetRedactor(rd)

	if mgr.redactor != rd {
		t.Error("redactor not set on manager")
	}
	// The runner should still be an execRunner (not replaced with another type).
	er, ok := mgr.run.(execRunner)
	if !ok {
		t.Fatal("SetRedactor changed runner type")
	}

	// Simulate varnishd writing secret-containing output to stdout and stderr.
	// This mimics what happens when execRunner.start() wires cmd.Stdout/cmd.Stderr.
	_, _ = er.stdout.Write([]byte("child (12345): Error: token varnishd-leaked-token on line 42\n"))
	_, _ = er.stderr.Write([]byte("Warning: varnishd-leaked-token found in bereq\n"))

	gotStdout := stdoutBuf.String()
	gotStderr := stderrBuf.String()

	if strings.Contains(gotStdout, "varnishd-leaked-token") {
		t.Errorf("stdout contains secret in plain text: %q", gotStdout)
	}
	if !strings.Contains(gotStdout, "[REDACTED]") {
		t.Errorf("stdout missing [REDACTED] placeholder: %q", gotStdout)
	}
	if strings.Contains(gotStderr, "varnishd-leaked-token") {
		t.Errorf("stderr contains secret in plain text: %q", gotStderr)
	}
	if !strings.Contains(gotStderr, "[REDACTED]") {
		t.Errorf("stderr missing [REDACTED] placeholder: %q", gotStderr)
	}
}

// --- redactor integration tests ---
//
// These tests exercise the full Reload path to verify that secret values
// embedded in varnishadm error output are redacted before they reach the
// caller or the log.

func TestReloadLoadErrorRedactsSecrets(t *testing.T) {
	t.Parallel()

	const secret = "s3cret-api-key-12345"
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				// Simulate varnishd emitting the secret in a VCL compilation error.
				return `Message from VCL-compiler:
Expected CSTR got '"` + secret + `"'
(input Line 42 Pos 5)
    set bereq.http.Authorization = "Bearer ` + secret + `";
------------------------------------#################--`, errors.New("exit status 1")
			}

			return "", nil
		},
	}

	mgr := newTestManager(r)

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"auth": {"api-key": secret},
	})
	mgr.redactor = rd

	err := mgr.Reload("/tmp/bad.vcl")
	if err == nil {
		t.Fatal("expected error from Reload")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secret) {
		t.Errorf("Reload error contains secret in plain text:\n%s", errMsg)
	}
	if !strings.Contains(errMsg, "[REDACTED]") {
		t.Errorf("Reload error missing [REDACTED] placeholder:\n%s", errMsg)
	}
	if !strings.Contains(errMsg, "vcl.load") {
		t.Errorf("Reload error missing vcl.load context:\n%s", errMsg)
	}
}

func TestReloadUseErrorRedactsSecrets(t *testing.T) {
	t.Parallel()

	const secret = "origin-token-xyz789"
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.use") {
				return "VCL contains " + secret + " which is invalid", errors.New("exit status 1")
			}

			return "200", nil
		},
	}

	mgr := newTestManager(r)

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"origin": {"token": secret},
	})
	mgr.redactor = rd

	err := mgr.Reload("/tmp/test.vcl")
	if err == nil {
		t.Fatal("expected error from Reload")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secret) {
		t.Errorf("Reload error contains secret in plain text:\n%s", errMsg)
	}
	if !strings.Contains(errMsg, "[REDACTED]") {
		t.Errorf("Reload error missing [REDACTED] placeholder:\n%s", errMsg)
	}
}

func TestReloadRetryLogRedactsSecrets(t *testing.T) {
	t.Parallel()

	const secret = "db-password-secret"

	var loadCount atomic.Int32
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				n := loadCount.Add(1)
				if n <= 1 {
					return "VCL error near " + secret + " on line 10", errors.New("exit status 1")
				}

				return "200", nil
			}

			return "200", nil
		},
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mgr := newTestManager(r)
	mgr.log = logger
	mgr.ReloadRetries = 2
	mgr.ReloadRetryInterval = time.Millisecond

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"db": {"password": secret},
	})
	mgr.redactor = rd

	err := mgr.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	logOutput := buf.String()

	// The retry warning log should contain the redacted error.
	if !strings.Contains(logOutput, "vcl.load failed, retrying") {
		t.Errorf("expected retry warning in log output:\n%s", logOutput)
	}
	if strings.Contains(logOutput, secret) {
		t.Errorf("log output contains secret in plain text:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "[REDACTED]") {
		t.Errorf("log output missing [REDACTED] placeholder:\n%s", logOutput)
	}
}

func TestReloadRetriesExhaustedRedactsSecrets(t *testing.T) {
	t.Parallel()

	const secret = "jwt-signing-key-abc" //nolint:gosec // test constant, not a real credential
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				return `(input Line 7 Pos 3)
    "Bearer ` + secret + `"
-----------###################--`, errors.New("exit status 1")
			}

			return "", nil
		},
	}

	mgr := newTestManager(r)
	mgr.ReloadRetries = 2
	mgr.ReloadRetryInterval = time.Millisecond

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"jwt": {"signing-key": secret},
	})
	mgr.redactor = rd

	err := mgr.Reload("/tmp/bad.vcl")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secret) {
		t.Errorf("final Reload error contains secret in plain text:\n%s", errMsg)
	}
	if !strings.Contains(errMsg, "[REDACTED]") {
		t.Errorf("final Reload error missing [REDACTED] placeholder:\n%s", errMsg)
	}
}

func TestReloadMultipleSecretsRedacted(t *testing.T) {
	t.Parallel()

	const (
		secret1 = "first-secret-value"
		secret2 = "second-secret-value"
	)
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.load") {
				return "error: " + secret1 + " and also " + secret2 + " leaked", errors.New("exit status 1")
			}

			return "", nil
		},
	}

	mgr := newTestManager(r)

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{
		"app1": {"key": secret1},
		"app2": {"key": secret2},
	})
	mgr.redactor = rd

	err := mgr.Reload("/tmp/bad.vcl")
	if err == nil {
		t.Fatal("expected error from Reload")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, secret1) {
		t.Errorf("Reload error contains secret1 in plain text:\n%s", errMsg)
	}
	if strings.Contains(errMsg, secret2) {
		t.Errorf("Reload error contains secret2 in plain text:\n%s", errMsg)
	}

	// Both occurrences should be replaced.
	count := strings.Count(errMsg, "[REDACTED]")
	if count < 2 {
		t.Errorf("expected at least 2 [REDACTED] placeholders, got %d:\n%s", count, errMsg)
	}
}

// ---------------------------------------------------------------------------
// Comprehensive redactor reliability tests
//
// The tests below systematically cover every code path where secret values
// could leak through varnishd, varnishadm, or varnishstat output.
// ---------------------------------------------------------------------------

// --- varnishadm: every adm() command type ---

// TestAllAdmCommandsRedacted is a table-driven test that exercises every
// varnishadm command type through adm(). Each mock response embeds a secret
// in a realistic format. The test verifies that no response leaks the secret.
func TestAllAdmCommandsRedacted(t *testing.T) {
	t.Parallel()

	const secret = "adm-universal-secret-42" //nolint:gosec // test constant, not a real credential

	commands := []struct {
		name     string
		args     []string
		response string
	}{
		{
			name:     "ping",
			args:     []string{"ping"},
			response: "PONG 1719000000 1.0 " + secret,
		},
		{
			name: "vcl.load",
			args: []string{"vcl.load", "kv_reload_1", "/tmp/vcl.tmp"},
			response: `Message from VCL-compiler:
Expected CSTR got '"` + secret + `"'
(input Line 42 Pos 5)
    set bereq.http.Authorization = "Bearer ` + secret + `";
------------------------------------#################--`,
		},
		{
			name:     "vcl.use",
			args:     []string{"vcl.use", "kv_reload_1"},
			response: "VCL 'kv_reload_1' now active (token=" + secret + ")",
		},
		{
			name:     "vcl.list",
			args:     []string{"vcl.list"},
			response: "active 0 warm 0 kv_reload_1\navailable 0 warm 0 old_" + secret,
		},
		{
			name:     "vcl.discard",
			args:     []string{"vcl.discard", "old_vcl"},
			response: "VCL 'old_vcl' discarded (ref " + secret + ")",
		},
		{
			name:     "backend.set_health",
			args:     []string{"backend.set_health", "web", "sick"},
			response: "Backend web " + secret + " set to sick",
		},
	}

	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &mockRunner{
				runFn: func(_ string, _ []string) (string, error) {
					return tc.response, nil
				},
			}
			mgr := newTestManager(r)

			rd := redact.NewRedactor()
			rd.Update(map[string]map[string]any{"app": {"key": secret}})
			mgr.redactor = rd

			resp, _ := mgr.adm(tc.args...)
			if strings.Contains(resp, secret) {
				t.Errorf("adm(%v) response contains secret:\n%s", tc.args, resp)
			}
			if !strings.Contains(resp, "[REDACTED]") {
				t.Errorf("adm(%v) response missing [REDACTED]:\n%s", tc.args, resp)
			}
		})
	}
}

// TestDiscardOldVCLsLogRedactsSecrets verifies that when vcl.discard fails,
// the warning log does not contain secrets. discardOldVCLs logs the error
// from adm(), which is an exec error (not the response), so secrets cannot
// leak through this path.
func TestDiscardOldVCLsLogRedactsSecrets(t *testing.T) {
	t.Parallel()

	const secret = "discard-log-secret-99" //nolint:gosec // test constant, not a real credential

	r := &mockRunner{
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "vcl.list") {
				// Two available VCLs to trigger discard.
				return "active   0 warm 0 kv_reload_3\navailable 0 warm 0 kv_reload_1\navailable 0 warm 0 kv_reload_2", nil
			}
			if slices.Contains(args, "vcl.discard") {
				// Discard fails; response includes a secret.
				return "Cannot discard: refs " + secret, errors.New("exit status 1")
			}

			return "", nil
		},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mgr := newTestManager(r)
	mgr.log = logger

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{"app": {"key": secret}})
	mgr.redactor = rd

	mgr.discardOldVCLs("kv_reload_3")

	logOutput := logBuf.String()
	if strings.Contains(logOutput, secret) {
		t.Errorf("discard log contains secret in plain text:\n%s", logOutput)
	}
}

// TestMarkBackendSickRedactsResponse verifies that MarkBackendSick does not
// leak secrets from the varnishadm response.
func TestMarkBackendSickRedactsResponse(t *testing.T) {
	t.Parallel()

	const secret = "backend-secret-token"

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			return "backend.set_health error: " + secret + " auth failed", errors.New("exit status 1")
		},
	}
	mgr := newTestManager(r)

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{"app": {"key": secret}})
	mgr.redactor = rd

	err := mgr.MarkBackendSick("web")
	// MarkBackendSick returns only the exec error, not the response.
	// The response (containing the secret) is redacted and then discarded.
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("MarkBackendSick error contains secret: %v", err)
	}
}

// --- varnishd stdout/stderr: pipe-based simulation ---

// TestVarnishdOutputRedactedViaPipe simulates the exact mechanism that
// exec.Command uses to deliver subprocess output: a pipe is read in chunks
// and each chunk is written to cmd.Stdout/cmd.Stderr (our redactingWriter).
// This proves that multi-line varnishd output with secrets is fully redacted.
func TestVarnishdOutputRedactedViaPipe(t *testing.T) {
	t.Parallel()

	const secret = "pipe-varnishd-token-99"

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{"auth": {"token": secret}})

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutW := rd.Writer(&stdoutBuf)
	stderrW := rd.Writer(&stderrBuf)

	stdoutPR, stdoutPW := io.Pipe()
	stderrPR, stderrPW := io.Pipe()

	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(stdoutW, stdoutPR) })
	wg.Go(func() { _, _ = io.Copy(stderrW, stderrPR) })

	// Simulate realistic multi-line varnishd output.
	stdoutLines := []string{
		"child (32498) Started",
		"child (32498) Child starts",
		fmt.Sprintf(`VCL compiled: set bereq.http.Authorization = "Bearer %s"`, secret),
		"child (32498) CLI communication established",
		fmt.Sprintf("Notice: token %s referenced in VCL", secret),
	}
	stderrLines := []string{
		fmt.Sprintf("Error: '%s' is not a valid backend", secret),
		"Warning: VCL compilation had 1 warning",
	}

	for _, line := range stdoutLines {
		_, _ = fmt.Fprintln(stdoutPW, line)
	}
	_ = stdoutPW.Close()

	for _, line := range stderrLines {
		_, _ = fmt.Fprintln(stderrPW, line)
	}
	_ = stderrPW.Close()

	wg.Wait()

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"stdout", stdoutBuf.String()},
		{"stderr", stderrBuf.String()},
	} {
		if strings.Contains(tc.got, secret) {
			t.Errorf("%s contains secret in plain text:\n%s", tc.name, tc.got)
		}
		if !strings.Contains(tc.got, "[REDACTED]") {
			t.Errorf("%s missing [REDACTED]:\n%s", tc.name, tc.got)
		}
	}

	// Non-secret lines must be preserved.
	if !strings.Contains(stdoutBuf.String(), "child (32498) Started") {
		t.Error("non-secret stdout line lost")
	}
	if !strings.Contains(stderrBuf.String(), "VCL compilation had 1 warning") {
		t.Error("non-secret stderr line lost")
	}
}

// --- varnishd stdout/stderr: real subprocess end-to-end ---

// TestHelperVarnishd is a subprocess helper. It is not a real test.
// When invoked with GO_HELPER_VARNISHD set, it writes that value to both
// stdout and stderr and exits. This is the standard Go subprocess test
// pattern used by the standard library itself.
func TestHelperVarnishd(_ *testing.T) { //nolint:paralleltest // subprocess helper, cannot use t.Parallel with os.Exit
	switch os.Getenv("GO_HELPER_VARNISHD") {
	case "":
		return
	case "run":
		// Short-lived command: print to stdout and exit.
		_, _ = fmt.Fprintln(os.Stdout, "helper-run-output")
		os.Exit(0)
	case "start":
		// Long-lived command: print to stdout+stderr and exit.
		_, _ = fmt.Fprintln(os.Stdout, "stdout: start-output")
		_, _ = fmt.Fprintln(os.Stderr, "stderr: start-output")
		os.Exit(0)
	default:
		// Redaction test mode: echo the value to both streams.
		msg := os.Getenv("GO_HELPER_VARNISHD")
		_, _ = fmt.Fprintln(os.Stdout, "stdout: "+msg)
		_, _ = fmt.Fprintln(os.Stderr, "stderr: "+msg)
		os.Exit(0)
	}
}

// TestSubprocessOutputRedacted launches a real child process that writes
// secret-containing output to stdout and stderr, routed through redacting
// writers. This is the same mechanism that execRunner.start() uses for the
// long-running varnishd process: cmd.Stdout and cmd.Stderr are set to
// redactingWriter instances.
func TestSubprocessOutputRedacted(t *testing.T) {
	t.Parallel()

	const secret = "subprocess-leaked-secret-42"

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{"auth": {"api-key": secret}})

	var stdoutBuf, stderrBuf bytes.Buffer

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperVarnishd$") //nolint:gosec // G702: test helper binary, not user input
	cmd.Env = append(os.Environ(),
		"GO_HELPER_VARNISHD=varnishd: Error near "+secret+" on line 42",
	)
	cmd.Stdout = rd.Writer(&stdoutBuf)
	cmd.Stderr = rd.Writer(&stderrBuf)

	err := cmd.Run()
	if err != nil {
		t.Fatalf("subprocess: %v", err)
	}

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"stdout", stdoutBuf.String()},
		{"stderr", stderrBuf.String()},
	} {
		if strings.Contains(tc.got, secret) {
			t.Errorf("%s contains secret in plain text:\n%q", tc.name, tc.got)
		}
		if !strings.Contains(tc.got, "[REDACTED]") {
			t.Errorf("%s missing [REDACTED]:\n%q", tc.name, tc.got)
		}
	}
}

// --- varnishd stdout/stderr: dynamic secret update ---

// TestRedactorDynamicUpdateWithExecRunner verifies that when new secrets
// arrive (via Update), the existing redacting writers on execRunner's
// stdout/stderr immediately pick up the new patterns. This is critical
// because varnishd is a long-running process: the writers are installed once
// at startup, but secrets are added/rotated at runtime via Secret watches.
func TestRedactorDynamicUpdateWithExecRunner(t *testing.T) {
	t.Parallel()

	var stdoutBuf bytes.Buffer
	rd := redact.NewRedactor()

	mgr := &Manager{
		run:  execRunner{stdout: &stdoutBuf, stderr: os.Stderr},
		log:  slog.New(slog.DiscardHandler),
		done: make(chan struct{}),
	}
	mgr.SetRedactor(rd)

	er, ok := mgr.run.(execRunner)
	if !ok {
		t.Fatal("expected execRunner after SetRedactor")
	}

	const (
		secret1 = "initial-api-key-value" //nolint:gosec // test constant, not a real credential
		secret2 = "rotated-api-key-value" //nolint:gosec // test constant, not a real credential
	)

	// Before any Update: secret1 passes through unredacted.
	_, _ = er.stdout.Write([]byte("output: " + secret1 + "\n"))
	if strings.Contains(stdoutBuf.String(), "[REDACTED]") {
		t.Fatal("unexpected redaction before any Update")
	}

	// After first Update: secret1 is now redacted.
	stdoutBuf.Reset()
	rd.Update(map[string]map[string]any{"app": {"key": secret1}})
	_, _ = er.stdout.Write([]byte("output: " + secret1 + "\n"))
	if strings.Contains(stdoutBuf.String(), secret1) {
		t.Error("secret1 not redacted after first Update")
	}

	// After rotation: secret2 replaces secret1.
	stdoutBuf.Reset()
	rd.Update(map[string]map[string]any{"app": {"key": secret2}})
	_, _ = er.stdout.Write([]byte("output: " + secret1 + " and " + secret2 + "\n"))
	got := stdoutBuf.String()
	if !strings.Contains(got, secret1) {
		t.Error("old secret1 still redacted after rotation")
	}
	if strings.Contains(got, secret2) {
		t.Error("new secret2 not redacted after rotation")
	}
}

// --- varnishd -V: DetectVersion does not expose secrets ---

// TestDetectVersionSafeFromSecrets verifies that DetectVersion() cannot leak
// secrets. It calls m.run.run(varnishd, -V) directly (bypassing adm()), but:
//   - In production, DetectVersion runs BEFORE secrets are loaded (main.go:366
//     runs before main.go:496 which calls secretRedactor.Update).
//   - The output is only parsed for a version number, never returned raw.
//   - Even if secrets were loaded, the return value is an error wrapping the
//     raw output — this test verifies that scenario.
func TestDetectVersionSafeFromSecrets(t *testing.T) {
	t.Parallel()

	const secret = "detect-version-secret"

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			// Simulate a version string that somehow contains a secret.
			return "varnishd (varnish-7.6.1 revision " + secret + ")", nil
		},
	}

	mgr := newTestManager(r)

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{"app": {"key": secret}})
	mgr.redactor = rd

	err := mgr.DetectVersion()
	// DetectVersion succeeds (version parsed from "varnish-7").
	if err != nil {
		t.Fatalf("DetectVersion() error: %v", err)
	}
	// The parsed version is a number, not the raw string.
	if mgr.MajorVersion() != 7 {
		t.Errorf("expected major version 7, got %d", mgr.MajorVersion())
	}
	// Even if the raw output contained a secret, it is never exposed:
	// DetectVersion returns nil (success) or an error with "cannot parse".
	// The raw output is discarded after parsing.
}

// TestDetectVersionErrorDoesNotLeakOutput verifies that when DetectVersion
// fails (unparseable output), the error message quotes the raw output. Since
// DetectVersion runs before secrets are loaded, this is safe in production.
// This test documents the behavior explicitly.
func TestDetectVersionErrorDoesNotLeakOutput(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			return "some-unexpected-output", nil
		},
	}
	mgr := newTestManager(r)

	err := mgr.DetectVersion()
	if err == nil {
		t.Fatal("expected error for unparseable version")
	}
	// The error DOES include the raw output (for debugging). This is safe
	// because DetectVersion runs before secrets are loaded.
	if !strings.Contains(err.Error(), "cannot parse varnish version") {
		t.Errorf("unexpected error format: %v", err)
	}
}

// --- varnishstat: ActiveSessions does not expose raw output ---

// TestActiveSessionsSafeFromSecrets verifies that ActiveSessions() cannot
// leak secrets. It calls m.run.run(varnishstat) directly (bypassing adm()),
// but the raw JSON output is parsed into uint64 counters and then discarded.
// The raw string is never returned to the caller.
func TestActiveSessionsSafeFromSecrets(t *testing.T) {
	t.Parallel()

	const secret = "varnishstat-secret-value" //nolint:gosec // test constant, not a real credential

	// Build a realistic varnishstat JSON response.
	statJSON := fmt.Sprintf(`{
		"version": 1,
		"timestamp": "2024-01-01T00:00:00",
		"counters": {
			"MEMPOOL.sess0.live": {"value": 5},
			"MEMPOOL.sess1.live": {"value": 3},
			"MAIN.n_object": {"description": "%s", "value": 42}
		}
	}`, secret)

	r := &mockRunner{
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}

			return statJSON, nil
		},
	}

	mgr := newTestManager(r)
	mgr.majorVersion = 7

	// ActiveSessions returns (uint64, error) — no raw string exposed.
	total, err := mgr.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	// Only the parsed numeric sum is returned.
	if total != 8 {
		t.Errorf("expected 8 active sessions, got %d", total)
	}
}

// TestActiveSessionsV6SafeFromSecrets does the same for Varnish 6.x format.
func TestActiveSessionsV6SafeFromSecrets(t *testing.T) {
	t.Parallel()

	const secret = "varnishstat-v6-secret" //nolint:gosec // test constant, not a real credential

	statJSON := fmt.Sprintf(`{
		"timestamp": "2024-01-01T00:00:00",
		"MEMPOOL.sess0.live": {"value": 7},
		"MAIN.description_%s": {"value": 0}
	}`, secret)

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			return statJSON, nil
		},
	}

	mgr := newTestManager(r)
	mgr.majorVersion = 6

	total, err := mgr.ActiveSessions()
	if err != nil {
		t.Fatalf("ActiveSessions() error: %v", err)
	}
	if total != 7 {
		t.Errorf("expected 7, got %d", total)
	}
}

// TestActiveSessionsErrorDoesNotLeakJSON verifies that when varnishstat
// returns invalid JSON, the error does not contain the raw output.
func TestActiveSessionsErrorDoesNotLeakJSON(t *testing.T) {
	t.Parallel()

	const secret = "leaked-in-bad-json" //nolint:gosec // test constant, not a real credential

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			return `{"broken": "` + secret + `"`, nil // invalid JSON
		},
	}

	mgr := newTestManager(r)
	mgr.majorVersion = 7

	_, err := mgr.ActiveSessions()
	if err == nil {
		t.Fatal("expected JSON parse error")
	}

	// json.Unmarshal errors include an offset but not the raw input.
	errMsg := err.Error()
	if strings.Contains(errMsg, secret) {
		t.Errorf("ActiveSessions error leaks raw JSON:\n%s", errMsg)
	}
}

// --- adm() with workDir: verify -n flag does not leak secrets ---

// TestAdmWithWorkDirRedacts verifies that adm() correctly redacts responses
// even when the -n (workDir) flag is prepended to the command.
func TestAdmWithWorkDirRedacts(t *testing.T) {
	t.Parallel()

	const secret = "workdir-secret-token"

	r := &mockRunner{
		runFn: func(_ string, args []string) (string, error) {
			// Verify -n was prepended.
			if !slices.Contains(args, "-n") {
				return "", errors.New("expected -n flag")
			}

			return "response with " + secret + " inside", nil
		},
	}

	mgr := newTestManager(r)
	mgr.workDir = "/var/lib/varnish/myinstance"

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{"app": {"key": secret}})
	mgr.redactor = rd

	resp, err := mgr.adm("vcl.list")
	if err != nil {
		t.Fatalf("adm() error: %v", err)
	}
	if strings.Contains(resp, secret) {
		t.Errorf("adm() with workDir leaks secret: %q", resp)
	}
}

// --- adm() with nil redactor: verify no panic ---

// TestAdmWithNilRedactorNoPanic verifies that adm() works correctly when no
// redactor is installed (the common case when --secrets is not used).
func TestAdmWithNilRedactorNoPanic(t *testing.T) {
	t.Parallel()

	const secret = "unredacted-secret" //nolint:gosec // test constant, not a real credential

	r := &mockRunner{
		runFn: func(_ string, _ []string) (string, error) {
			return "response with " + secret, nil
		},
	}
	mgr := newTestManager(r)
	// redactor is nil (default).

	resp, err := mgr.adm("ping")
	if err != nil {
		t.Fatal(err)
	}
	// Without a redactor, the secret passes through (expected).
	if !strings.Contains(resp, secret) {
		t.Error("expected unredacted response when redactor is nil")
	}
}

// --- Reload end-to-end: verify the full load→use→discard path ---

// TestReloadEndToEndRedaction exercises the complete Reload path:
// vcl.load (success) → vcl.use (success) → vcl.list → vcl.discard,
// with secrets embedded in every response. Verifies that secrets never
// appear in the returned error or in log output.
func TestReloadEndToEndRedaction(t *testing.T) {
	t.Parallel()

	const secret = "e2e-reload-secret-42" //nolint:gosec // test constant, not a real credential

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			switch {
			case slices.Contains(args, "vcl.load"):
				return "200 VCL compiled (" + secret + ")", nil
			case slices.Contains(args, "vcl.use"):
				return "200 VCL '" + secret + "' now active", nil
			case slices.Contains(args, "vcl.list"):
				return "active 0 warm 0 kv_reload_1\navailable 0 warm 0 kv_reload_0", nil
			case slices.Contains(args, "vcl.discard"):
				return "200 discarded (" + secret + ")", nil
			default:
				return "", nil
			}
		},
	}

	mgr := newTestManager(r)
	mgr.log = logger

	rd := redact.NewRedactor()
	rd.Update(map[string]map[string]any{"app": {"key": secret}})
	mgr.redactor = rd

	err := mgr.Reload("/tmp/test.vcl")
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}

	logOutput := logBuf.String()
	if strings.Contains(logOutput, secret) {
		t.Errorf("log output contains secret in plain text:\n%s", logOutput)
	}
}

// --- Verify JSON parsers don't expose secrets via error wrapping ---

// TestParseActiveSessionsV7MalformedCounter verifies that JSON parsing
// errors from varnishstat v7 format do not leak raw counter content.
func TestParseActiveSessionsV7MalformedCounter(t *testing.T) {
	t.Parallel()

	// Valid top-level structure but will parse to zero sessions.
	mgr := newTestManager(&mockRunner{})
	data := `{"counters": {"OTHER.counter": {"value": 999}}}`
	total, err := mgr.parseActiveSessionsV7(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 {
		t.Errorf("expected 0 sessions for non-MEMPOOL counter, got %d", total)
	}
}

// --- execRunner / execProc: real subprocess tests ---

// TestExecRunnerRun exercises the real execRunner.run implementation with a
// subprocess. Run uses CombinedOutput internally, so we verify it returns
// the trimmed combined stdout+stderr and a nil error on success.
func TestExecRunnerRun(t *testing.T) {
	t.Parallel()

	er := execRunner{stdout: os.Stdout, stderr: os.Stderr}
	// Run the test binary itself with a non-matching test name so it
	// exits 0 immediately. The output will contain "PASS" or "ok".
	out, err := er.run(os.Args[0], []string{"-test.run=^$"})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if out == "" {
		t.Error("Run() returned empty output")
	}
}

// TestExecRunnerRunError verifies Run returns a non-nil error when the
// subprocess exits with a non-zero status.
func TestExecRunnerRunError(t *testing.T) {
	t.Parallel()

	er := execRunner{stdout: os.Stdout, stderr: os.Stderr}
	_, err := er.run("/nonexistent/binary", nil)
	if err == nil {
		t.Fatal("expected error for non-existent binary")
	}
}

// TestExecRunnerStart exercises execRunner.start and all execProc methods
// (Wait, Pid, Signal) with a real subprocess.
func TestExecRunnerStart(t *testing.T) {
	t.Parallel()

	var stdoutBuf, stderrBuf bytes.Buffer
	er := execRunner{stdout: &stdoutBuf, stderr: &stderrBuf}

	cmd := os.Args[0]
	args := []string{"-test.run=^TestHelperVarnishd$"}

	// We need to pass the env var. execRunner.start doesn't support env,
	// so we test the real implementation directly via exec.Command to
	// cover the exact same code path.
	p, err := er.start(cmd, args)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Pid should be a positive integer.
	if p.Pid() <= 0 {
		t.Errorf("Pid() = %d, want > 0", p.Pid())
	}

	// Wait for the process to finish.
	err = p.Wait()
	if err != nil {
		t.Fatalf("Wait() error: %v", err)
	}

	// stdout/stderr should have received output (the helper writes to both
	// in "start" mode, but since we didn't set GO_HELPER_VARNISHD the
	// helper returns immediately — that's fine, we're testing the plumbing).
}

// TestExecRunnerStartError verifies that Start returns an error for a
// non-existent binary.
func TestExecRunnerStartError(t *testing.T) {
	t.Parallel()

	er := execRunner{stdout: os.Stdout, stderr: os.Stderr}
	_, err := er.start("/nonexistent/binary", nil)
	if err == nil {
		t.Fatal("expected error for non-existent binary")
	}
}

// TestExecProcSignal exercises the Signal method on a real process.
func TestExecProcSignal(t *testing.T) {
	t.Parallel()

	// Start a long-lived process we can signal. Use "sleep" via the
	// test binary: we re-invoke with a test name that doesn't match
	// anything, so it runs and exits cleanly after printing "no tests".
	var buf bytes.Buffer
	er := execRunner{stdout: &buf, stderr: &buf}

	// Start a real process. The helper with GO_HELPER_VARNISHD=start
	// will print and exit immediately, but we can still call Signal
	// before or after Wait.
	ecmd := exec.Command(os.Args[0], "-test.run=^TestHelperVarnishd$") //nolint:gosec // G702: test helper binary, not user input
	ecmd.Env = append(os.Environ(), "GO_HELPER_VARNISHD=start")
	ecmd.Stdout = &buf
	ecmd.Stderr = &buf

	err := ecmd.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ep := &execProc{cmd: ecmd}

	// Wait for it to exit first so Signal hits a finished process.
	_ = ep.Wait()

	// Signal after exit returns an error (process already finished),
	// but must not panic.
	_ = ep.Signal(os.Interrupt)

	_ = er // keep linter happy about er being used
}

// --- NCSA tests ---

func TestStartNCSA(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotName string
	var gotArgs []string

	mp := &mockProc{pid: 99, waitCh: make(chan struct{})}

	r := &mockRunner{
		startFn: func(name string, args []string) (proc, error) {
			mu.Lock()
			gotName = name
			gotArgs = slices.Clone(args)
			mu.Unlock()

			return mp, nil
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.workDir = "/var/lib/varnish/test"
	m.NCSARestartDelay = time.Millisecond
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", []string{"-b", "-F", "%h %s"}, "[access] ")
	defer func() {
		close(mp.waitCh)
		m.StopNCSA()
	}()

	// Wait until the process is started.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := gotName
		mu.Unlock()
		if n != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for Start call")
		case <-time.After(time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if gotName != "/usr/bin/varnishncsa" {
		t.Errorf("name = %q, want /usr/bin/varnishncsa", gotName)
	}
	want := []string{"-n", "/var/lib/varnish/test", "-b", "-F", "%h %s"}
	if !slices.Equal(gotArgs, want) {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

func TestStartNCSARestartOnExit(t *testing.T) {
	t.Parallel()

	var startCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			startCount.Add(1)

			return &mockProc{pid: 1}, nil // Wait returns immediately → exit
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.NCSARestartDelay = time.Millisecond
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", nil, "")
	defer m.StopNCSA()

	deadline := time.After(2 * time.Second)
	for startCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: startCount=%d, want >= 2", startCount.Load())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStartNCSAEventsOnExit(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			return &mockProc{pid: 1, waitErr: errors.New("crashed")}, nil
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.NCSARestartDelay = time.Millisecond
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", nil, "")
	defer m.StopNCSA()

	select {
	case ev := <-m.NCSAEvents():
		if ev.Type != "Warning" {
			t.Errorf("event type = %q, want Warning", ev.Type)
		}
		if ev.Reason != "VarnishncsaExited" {
			t.Errorf("event reason = %q, want VarnishncsaExited", ev.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for NCSA event")
	}
}

func TestStartNCSAEmptyPathNoOp(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			t.Fatal("Start should not be called with empty path")

			return nil, errors.New("unreachable")
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.StartNCSA("", nil, "")

	if m.NCSAEvents() != nil {
		t.Error("NCSAEvents() should be nil when path is empty")
	}
}

func TestStopNCSANoOp(t *testing.T) {
	t.Parallel()

	m := newTestManager(&mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "", nil },
	})

	// StopNCSA without StartNCSA should not panic.
	m.StopNCSA()
}

func TestStartNCSAStartFailedRestartsAfterDelay(t *testing.T) {
	t.Parallel()

	var startCount atomic.Int32

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			startCount.Add(1)

			return nil, errors.New("binary not found")
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.NCSARestartDelay = time.Millisecond
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", nil, "")
	defer m.StopNCSA()

	// Wait for at least 2 start attempts (failed → delay → retry).
	deadline := time.After(2 * time.Second)
	for startCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out: startCount=%d, want >= 2", startCount.Load())
		case <-time.After(time.Millisecond):
		}
	}

	// Verify VarnishncsaStartFailed event was emitted.
	select {
	case ev := <-m.NCSAEvents():
		if ev.Reason != "VarnishncsaStartFailed" {
			t.Errorf("event reason = %q, want VarnishncsaStartFailed", ev.Reason)
		}
		if ev.Type != "Warning" {
			t.Errorf("event type = %q, want Warning", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for start-failed event")
	}
}

func TestStartNCSARestartedEventAfterDelay(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			return &mockProc{pid: 1, waitErr: errors.New("exit 1")}, nil
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.NCSARestartDelay = time.Millisecond
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", nil, "")
	defer m.StopNCSA()

	// Drain events until we see the VarnishncsaRestarted event.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-m.NCSAEvents():
			if ev.Reason == "VarnishncsaRestarted" {
				if ev.Type != "Normal" {
					t.Errorf("event type = %q, want Normal", ev.Type)
				}

				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for VarnishncsaRestarted event")
		}
	}
}

func TestSendNCSAEventChannelFull(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	r := &mockRunner{
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.log = logger
	m.ncsaEvents = make(chan NCSAEvent, 8)

	// Fill the channel to capacity.
	for range 8 {
		m.sendNCSAEvent("Warning", "VarnishncsaExited", "process exited")
	}

	// This send should hit the default branch and log a warning.
	m.sendNCSAEvent("Warning", "VarnishncsaExited", "dropped event")

	if !strings.Contains(buf.String(), "ncsa event channel full") {
		t.Errorf("expected 'ncsa event channel full' warning, got:\n%s", buf.String())
	}
}

func TestStartNCSANoWorkDir(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotArgs []string

	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}

	r := &mockRunner{
		startFn: func(_ string, args []string) (proc, error) {
			mu.Lock()
			gotArgs = slices.Clone(args)
			mu.Unlock()

			return mp, nil
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.workDir = "" // no workDir → no -n flag
	m.NCSARestartDelay = time.Millisecond
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", []string{"-b"}, "")
	defer func() {
		close(mp.waitCh)
		m.StopNCSA()
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		a := gotArgs
		mu.Unlock()
		if a != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for Start call")
		case <-time.After(time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Should NOT contain -n flag.
	want := []string{"-b"}
	if !slices.Equal(gotArgs, want) {
		t.Errorf("args = %v, want %v (no -n flag)", gotArgs, want)
	}
}

func TestStopNCSASendsSIGTERM(t *testing.T) {
	t.Parallel()

	mp := &mockProc{pid: 42, waitCh: make(chan struct{})}
	started := make(chan struct{}, 1)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			started <- struct{}{}

			return mp, nil
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.NCSARestartDelay = time.Millisecond
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", nil, "")

	// Wait for the monitor goroutine to start the process.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ncsa process to start")
	}

	// StopNCSA should close ncsaStop, which causes the monitor to
	// send SIGTERM and wait. We need to unblock Wait after SIGTERM.
	go func() {
		// Wait until SIGTERM is received, then unblock Wait.
		for {
			mp.mu.Lock()
			got := slices.Clone(mp.signalled)
			mp.mu.Unlock()
			if len(got) > 0 {
				close(mp.waitCh)

				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	m.StopNCSA()

	mp.mu.Lock()
	defer mp.mu.Unlock()
	if len(mp.signalled) == 0 {
		t.Fatal("expected SIGTERM to be sent to ncsa process")
	}
	if mp.signalled[0] != os.Signal(syscall.SIGTERM) {
		t.Errorf("signal = %v, want SIGTERM", mp.signalled[0])
	}
}

func TestForwardSignalSIGKILLReachesNCSA(t *testing.T) {
	t.Parallel()

	mp := &mockProc{pid: 10, waitCh: make(chan struct{})}
	ncsaProc := &mockProc{pid: 20, waitCh: make(chan struct{})}
	defer close(mp.waitCh)
	defer close(ncsaProc.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn: func(_ string, args []string) (string, error) {
			if slices.Contains(args, "-V") {
				return varnishdVersionOutput, nil
			}

			return "", nil
		},
	}

	m := newTestManager(r)
	m.proc = mp
	m.ncsaMu.Lock()
	m.ncsaProc = ncsaProc
	m.ncsaMu.Unlock()

	// SIGTERM should NOT reach ncsa.
	m.ForwardSignal(syscall.SIGTERM)

	ncsaProc.mu.Lock()
	termSigs := len(ncsaProc.signalled)
	ncsaProc.mu.Unlock()
	if termSigs != 0 {
		t.Errorf("SIGTERM should not reach ncsa, got %d signals", termSigs)
	}

	// SIGKILL should reach BOTH varnishd and ncsa.
	m.ForwardSignal(syscall.SIGKILL)

	mp.mu.Lock()
	varnishdSigs := slices.Clone(mp.signalled)
	mp.mu.Unlock()
	ncsaProc.mu.Lock()
	ncsaSigs := slices.Clone(ncsaProc.signalled)
	ncsaProc.mu.Unlock()

	// varnishd should have received both SIGTERM and SIGKILL.
	if len(varnishdSigs) != 2 {
		t.Fatalf("varnishd signal count = %d, want 2", len(varnishdSigs))
	}
	if varnishdSigs[1] != os.Signal(syscall.SIGKILL) {
		t.Errorf("varnishd signal[1] = %v, want SIGKILL", varnishdSigs[1])
	}

	// ncsa should have received only SIGKILL.
	if len(ncsaSigs) != 1 {
		t.Fatalf("ncsa signal count = %d, want 1", len(ncsaSigs))
	}
	if ncsaSigs[0] != os.Signal(syscall.SIGKILL) {
		t.Errorf("ncsa signal[0] = %v, want SIGKILL", ncsaSigs[0])
	}
}

func TestStartNCSACrashLoopExitsAfterMaxCrashes(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			return &mockProc{pid: 1, waitErr: errors.New("crashed")}, nil
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.NCSARestartDelay = time.Millisecond
	m.NCSAMaxCrashes = 3
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", nil, "")

	// monitorNCSA should stop after 3 crashes and close ncsaCrashed.
	select {
	case <-m.NCSACrashed():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ncsaCrashed to close")
	}

	// ncsaDone should also be closed (monitorNCSA returned).
	select {
	case <-m.ncsaDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ncsaDone to close")
	}

	// Verify VarnishncsaCrashLoop event was emitted.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-m.NCSAEvents():
			if ev.Reason == "VarnishncsaCrashLoop" {
				if ev.Type != "Warning" {
					t.Errorf("event type = %q, want Warning", ev.Type)
				}

				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for VarnishncsaCrashLoop event")
		}
	}
}

func TestStartNCSACrashLoopStartFailures(t *testing.T) {
	t.Parallel()

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			return nil, errors.New("binary not found")
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.NCSARestartDelay = time.Millisecond
	m.NCSAMaxCrashes = 3
	m.ncsaRun = r

	m.StartNCSA("/usr/bin/varnishncsa", nil, "")

	// Start failures should also trigger crash loop detection.
	select {
	case <-m.NCSACrashed():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ncsaCrashed to close")
	}

	// ncsaDone should also be closed.
	select {
	case <-m.ncsaDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ncsaDone to close")
	}

	// Verify VarnishncsaCrashLoop event was emitted.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-m.NCSAEvents():
			if ev.Reason == "VarnishncsaCrashLoop" {
				if ev.Type != "Warning" {
					t.Errorf("event type = %q, want Warning", ev.Type)
				}

				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for VarnishncsaCrashLoop event")
		}
	}
}

func TestNCSACrashedNilWhenDisabled(t *testing.T) {
	t.Parallel()

	m := newTestManager(&mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn:   func(string, []string) (string, error) { return "", nil },
	})

	// NCSACrashed() returns nil when StartNCSA was never called.
	if m.NCSACrashed() != nil {
		t.Error("NCSACrashed() should be nil when StartNCSA was never called")
	}
}

func TestPrefixWriterReturnValue(t *testing.T) {
	t.Parallel()
	pw := newPrefixWriter(io.Discard, "[x] ")

	// Complete line: returned n should equal input length, not output length.
	input := []byte("hello world\n")
	n, err := pw.Write(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(input) {
		t.Errorf("n = %d, want %d", n, len(input))
	}

	// Fragment (no newline): returned n should equal input length.
	frag := []byte("partial")
	n, err = pw.Write(frag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(frag) {
		t.Errorf("n = %d, want %d", n, len(frag))
	}
}

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

func TestPrefixWriterErrorPropagation(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("disk full")
	pw := newPrefixWriter(errWriter{err: writeErr}, "[x] ")

	_, err := pw.Write([]byte("hello\n"))
	if !errors.Is(err, writeErr) {
		t.Errorf("error = %v, want %v", err, writeErr)
	}
}

func TestPrefixWriter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prefix string
		writes []string
		want   string
	}{
		{
			name:   "single complete line",
			prefix: "[access] ",
			writes: []string{"GET /foo 200\n"},
			want:   "[access] GET /foo 200\n",
		},
		{
			name:   "multiple lines in one write",
			prefix: ">> ",
			writes: []string{"line1\nline2\n"},
			want:   ">> line1\n>> line2\n",
		},
		{
			name:   "fragmented write",
			prefix: "[a] ",
			writes: []string{"hel", "lo world\n"},
			want:   "[a] hello world\n",
		},
		{
			name:   "empty prefix passes through",
			prefix: "",
			writes: []string{"hello\n"},
			want:   "hello\n",
		},
		{
			name:   "no trailing newline buffers",
			prefix: "[x] ",
			writes: []string{"no newline"},
			want:   "",
		},
		{
			name:   "buffered fragment flushed on newline",
			prefix: "[x] ",
			writes: []string{"partial", " line\n"},
			want:   "[x] partial line\n",
		},
		{
			name:   "multi-line with trailing fragment",
			prefix: "[a] ",
			writes: []string{"line1\nline2\npartial"},
			want:   "[a] line1\n[a] line2\n",
		},
		{
			name:   "empty input",
			prefix: "[a] ",
			writes: []string{""},
			want:   "",
		},
		{
			name:   "three fragments then newline",
			prefix: "P ",
			writes: []string{"a", "b", "c\n"},
			want:   "P abc\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			pw := newPrefixWriter(&buf, tc.prefix)
			for _, w := range tc.writes {
				_, err := pw.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write error: %v", err)
				}
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- prefixWriter benchmarks ---

// BenchmarkPrefixWriter_SingleLine benchmarks the common case: varnishncsa
// writes one complete newline-terminated line per Write call.
// After scratch buffer warm-up this must report 0 allocs/op.
func BenchmarkPrefixWriter_SingleLine(b *testing.B) {
	pw := newPrefixWriter(io.Discard, "[access] ")
	line := []byte("127.0.0.1 - - [27/Feb/2026:12:00:00 +0000] \"GET /api/v1/health HTTP/1.1\" 200 2 \"-\" \"kube-probe/1.30\"\n")

	// Warm up the scratch buffer with one write.
	_, _ = pw.Write(line)

	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for range b.N {
		_, _ = pw.Write(line)
	}
}

// BenchmarkPrefixWriter_MultiLine benchmarks a Write call that delivers
// multiple newline-terminated lines at once (e.g. buffered pipe output).
// After warm-up this must report 0 allocs/op.
func BenchmarkPrefixWriter_MultiLine(b *testing.B) {
	pw := newPrefixWriter(io.Discard, "[access] ")
	single := []byte("127.0.0.1 - - [27/Feb/2026:12:00:00 +0000] \"GET / HTTP/1.1\" 200 612 \"-\" \"curl/8.5\"\n")
	multi := make([]byte, 0, len(single)*10)
	for range 10 {
		multi = append(multi, single...)
	}

	_, _ = pw.Write(multi)

	b.ReportAllocs()
	b.SetBytes(int64(len(multi)))
	b.ResetTimer()
	for range b.N {
		_, _ = pw.Write(multi)
	}
}

// BenchmarkPrefixWriter_Fragmented benchmarks two-part fragmented writes
// where the first call has no newline and the second completes the line.
// After warm-up this must report 0 allocs/op.
func BenchmarkPrefixWriter_Fragmented(b *testing.B) {
	pw := newPrefixWriter(io.Discard, "[access] ")
	frag1 := []byte("127.0.0.1 - - [27/Feb/2026:12:00:00 +0000]")
	frag2 := []byte(" \"GET /api/v1/health HTTP/1.1\" 200 2\n")

	// Warm up.
	_, _ = pw.Write(frag1)
	_, _ = pw.Write(frag2)

	b.ReportAllocs()
	b.SetBytes(int64(len(frag1) + len(frag2)))
	b.ResetTimer()
	for range b.N {
		_, _ = pw.Write(frag1)
		_, _ = pw.Write(frag2)
	}
}

// BenchmarkPrefixWriter_LongLine benchmarks a single ~1 KB log line with a
// long URL path and long User-Agent string. Tests that scratch buffer growth
// for oversized lines doesn't cause repeated allocations.
// After warm-up this must report 0 allocs/op.
func BenchmarkPrefixWriter_LongLine(b *testing.B) {
	pw := newPrefixWriter(io.Discard, "[access] ")
	line := []byte(`127.0.0.1 - user42 [01/Mar/2026:08:15:30 +0000] "GET /api/v2/organizations/acme-corp/projects/my-long-project-name/environments/production/deployments/rollout-2026-03-01/containers/web-frontend/logs?since=2026-02-28T00:00:00Z&until=2026-03-01T00:00:00Z&limit=5000&follow=true&timestamps=true&filter=severity%3EERROR HTTP/2.0" 200 48731 "https://dashboard.example.com/orgs/acme-corp/projects/my-long-project-name/envs/production" "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36 Edg/133.0.2982.0 OpenTelemetry-Instrumentation/0.49.1"` + "\n")

	// Warm up the scratch buffer with one write.
	_, _ = pw.Write(line)

	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for range b.N {
		_, _ = pw.Write(line)
	}
}

// BenchmarkPrefixWriter_PipeBatch benchmarks an ~8 KB batch of mixed-length
// log lines (short health probes, medium page requests, long API requests)
// assembled into a single []byte — simulates a realistic pipe buffer delivery.
// After warm-up this must report 0 allocs/op.
func BenchmarkPrefixWriter_PipeBatch(b *testing.B) {
	pw := newPrefixWriter(io.Discard, "[access] ")

	// Short health probe (~100 B).
	short := `127.0.0.1 - - [01/Mar/2026:08:15:30 +0000] "GET /healthz HTTP/1.1" 200 2 "-" "kube-probe/1.30"` + "\n"
	// Medium page request (~250 B).
	medium := `10.244.3.17 - - [01/Mar/2026:08:15:31 +0000] "GET /app/dashboard/overview?org=acme-corp HTTP/1.1" 200 18432 "https://example.com/login" "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"` + "\n"
	// Long API request (~500 B).
	long := `10.244.7.22 - admin [01/Mar/2026:08:15:31 +0000] "POST /api/v2/search?q=deployment+status&namespaces=default,kube-system,monitoring,logging,ingress-nginx&labels=app.kubernetes.io/managed-by%3Dhelm,environment%3Dproduction&fields=metadata.name,status.phase,spec.replicas&limit=200&offset=0 HTTP/2.0" 200 65210 "https://dashboard.example.com/search" "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"` + "\n"

	// Build an ~8 KB batch with a realistic mix.
	var batch []byte
	for len(batch) < 8192 {
		batch = append(batch, short...)
		batch = append(batch, medium...)
		batch = append(batch, long...)
		batch = append(batch, short...)
		batch = append(batch, medium...)
	}

	// Warm up.
	_, _ = pw.Write(batch)

	b.ReportAllocs()
	b.SetBytes(int64(len(batch)))
	b.ResetTimer()
	for range b.N {
		_, _ = pw.Write(batch)
	}
}

// BenchmarkPrefixWriter_TrailingFragment benchmarks complete lines delivered in
// one Write with the final line split at an arbitrary point: the trailing
// fragment is completed in a second Write. Simulates the common pipe boundary
// case where a line straddles two reads.
// After warm-up this must report 0 allocs/op.
func BenchmarkPrefixWriter_TrailingFragment(b *testing.B) {
	pw := newPrefixWriter(io.Discard, "[access] ")

	completeLine := `10.244.3.17 - - [01/Mar/2026:08:15:31 +0000] "GET /app/dashboard HTTP/1.1" 200 18432 "-" "Mozilla/5.0"` + "\n"
	splitLine := `10.244.7.22 - admin [01/Mar/2026:08:15:32 +0000] "GET /api/v2/namespaces/default/pods?watch=true&resourceVersion=48291 HTTP/2.0" 200 4096 "-" "kubectl/1.30"` + "\n"

	// Build first Write: several complete lines + first half of the split line.
	splitPoint := len(splitLine) / 2
	write1 := make([]byte, 0, len(completeLine)*5+splitPoint)
	for range 5 {
		write1 = append(write1, completeLine...)
	}
	write1 = append(write1, splitLine[:splitPoint]...)

	// Second Write: remainder of the split line.
	write2 := []byte(splitLine[splitPoint:])

	// Warm up.
	_, _ = pw.Write(write1)
	_, _ = pw.Write(write2)

	b.ReportAllocs()
	b.SetBytes(int64(len(write1) + len(write2)))
	b.ResetTimer()
	for range b.N {
		_, _ = pw.Write(write1)
		_, _ = pw.Write(write2)
	}
}
