package varnish

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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

// newTestManager creates a Manager wired with mock runner/dial for testing.
// The dialFn defaults to immediate success; override m.dialFn after calling this.
func newTestManager(r *mockRunner) *Manager {
	return &Manager{
		varnishdPath:   "/usr/sbin/varnishd",
		varnishadmPath: "/usr/bin/varnishadm",
		adminAddr:      "127.0.0.1:6082",
		listenAddrs:    []string{":8080"},
		run:            r,
		dialFn: func(string, time.Duration) (net.Conn, error) {
			// default: immediate success via an in-process pipe
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
		done: make(chan struct{}),
	}
}

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
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.listenAddrs = []string{":8080", ":8443"}
	m.extraArgs = []string{"-p", "default_ttl=3600"}
	m.secretFile = "/tmp/test-secret" // pre-set to skip generateSecret

	// Bypass generateSecret by calling the inner logic directly.
	// We need to set secretFile before Start, so we call the method parts manually.
	// Actually, Start calls generateSecret which writes a real file. Instead,
	// let's just call Start and verify args after it sets secretFile.

	// To avoid generateSecret side effects, pre-set secretFile and override Start's
	// first step. We'll test Start end-to-end by letting it create a real temp secret.
	m.secretFile = "" // reset so generateSecret runs
	err := m.Start("/etc/varnish/default.vcl")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Clean up the generated secret file.
	if m.secretFile != "" {
		defer func() { _ = os.Remove(m.secretFile) }()
	}

	if gotName != "/usr/sbin/varnishd" {
		t.Errorf("command name = %q, want /usr/sbin/varnishd", gotName)
	}

	// Expected: -F -a :8080 -a :8443 -T 127.0.0.1:6082 -f /etc/varnish/default.vcl -S <secret> -p default_ttl=3600
	want := []string{
		"-F",
		"-a", ":8080",
		"-a", ":8443",
		"-T", "127.0.0.1:6082",
		"-f", "/etc/varnish/default.vcl",
		"-S", m.secretFile,
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
	dialCount := 0
	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn:   func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	// Dial fails twice, then succeeds.
	m.dialFn = func(string, time.Duration) (net.Conn, error) {
		dialCount++
		if dialCount < 3 {
			return nil, fmt.Errorf("connection refused")
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}

	err := m.Start("/tmp/test.vcl")
	if m.secretFile != "" {
		defer func() { _ = os.Remove(m.secretFile) }()
	}
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if dialCount < 3 {
		t.Errorf("dialCount = %d, want >= 3", dialCount)
	}
}

func TestStartAdminTimeout(t *testing.T) {
	mp := &mockProc{pid: 1, waitCh: make(chan struct{})}
	defer close(mp.waitCh)

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn:   func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.dialFn = func(string, time.Duration) (net.Conn, error) {
		return nil, fmt.Errorf("connection refused")
	}

	// Use a very short timeout by calling waitForAdmin directly.
	m.secretFile = "/dev/null" // skip generateSecret
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
	m.secretFile = "/tmp/secret"

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
		// calls are [varnishadm, -T, addr, -S, secret, subcmd, ...]
		if len(c) >= 6 {
			sub := c[5]
			if sub == "vcl.load" || sub == "vcl.use" {
				loadUseCalls = append(loadUseCalls, c[5:])
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
			for _, a := range args {
				if a == "vcl.load" {
					return "VCL compilation failed", fmt.Errorf("exit status 1")
				}
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.secretFile = "/tmp/secret"

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
			for _, a := range args {
				if a == "vcl.use" {
					return "VCL in use", fmt.Errorf("exit status 1")
				}
			}
			return "200", nil
		},
	}

	m := newTestManager(r)
	m.secretFile = "/tmp/secret"

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
	m.secretFile = "/tmp/secret"

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

func TestForwardSignalNilProc(t *testing.T) {
	m := &Manager{}
	// Should not panic.
	m.ForwardSignal(os.Interrupt)
}

func TestCleanup(t *testing.T) {
	f, err := os.CreateTemp("", "varnish-test-secret-*")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	_ = f.Close()

	m := &Manager{secretFile: name}
	m.Cleanup()

	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("secret file %s still exists after Cleanup", name)
	}
}

func TestParseAdminPort(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{
			name:  "host and port",
			input: "127.0.0.1:6082",
			want:  6082,
		},
		{
			name:  "empty host",
			input: ":6082",
			want:  6082,
		},
		{
			name:  "all interfaces",
			input: "0.0.0.0:6082",
			want:  6082,
		},
		{
			name:  "IPv6 loopback",
			input: "[::1]:6082",
			want:  6082,
		},
		{
			name:  "high port",
			input: "127.0.0.1:65535",
			want:  65535,
		},
		{
			name:    "missing port",
			input:   "127.0.0.1",
			wantErr: true,
		},
		{
			name:    "non-numeric port",
			input:   "127.0.0.1:abc",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAdminPort(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseAdminPort(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
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
		runFn:   func(string, []string) (string, error) { return "200", nil },
	}

	m := newTestManager(r)
	m.secretFile = "/tmp/secret"

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
		runFn:   func(string, []string) (string, error) { return "200", nil },
	}

	m := newTestManager(r)
	m.secretFile = "/tmp/secret"

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
	m := New("/usr/sbin/varnishd", "/usr/bin/varnishadm", "127.0.0.1:6082",
		[]string{":8080", ":8443"}, "/tmp/secret", []string{"-p", "default_ttl=3600"})

	if m.varnishdPath != "/usr/sbin/varnishd" {
		t.Errorf("varnishdPath = %q, want /usr/sbin/varnishd", m.varnishdPath)
	}
	if m.varnishadmPath != "/usr/bin/varnishadm" {
		t.Errorf("varnishadmPath = %q, want /usr/bin/varnishadm", m.varnishadmPath)
	}
	if m.adminAddr != "127.0.0.1:6082" {
		t.Errorf("adminAddr = %q, want 127.0.0.1:6082", m.adminAddr)
	}
	if len(m.listenAddrs) != 2 {
		t.Errorf("listenAddrs length = %d, want 2", len(m.listenAddrs))
	}
	if m.secretPath != "/tmp/secret" {
		t.Errorf("secretPath = %q, want /tmp/secret", m.secretPath)
	}
	if len(m.extraArgs) != 2 {
		t.Errorf("extraArgs length = %d, want 2", len(m.extraArgs))
	}
	if m.done == nil {
		t.Error("done channel should not be nil")
	}
}

func TestDoneAndErr(t *testing.T) {
	m := &Manager{done: make(chan struct{}), err: fmt.Errorf("test error")}

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

func TestGenerateSecretWithPath(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "subdir", "secret")

	m := &Manager{secretPath: secretPath}
	path, err := m.generateSecret()
	if err != nil {
		t.Fatalf("generateSecret() error: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	if path != secretPath {
		t.Errorf("path = %q, want %q", path, secretPath)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading secret: %v", err)
	}
	if len(data) == 0 {
		t.Error("secret file is empty")
	}
}

func TestStartRunnerError(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) {
			return nil, fmt.Errorf("exec failed")
		},
		runFn: func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)

	err := m.Start("/tmp/test.vcl")
	if m.secretFile != "" {
		defer func() { _ = os.Remove(m.secretFile) }()
	}
	if err == nil {
		t.Fatal("expected error from Start, got nil")
	}
	if !strings.Contains(err.Error(), "starting varnishd") {
		t.Errorf("error = %q, want substring 'starting varnishd'", err.Error())
	}
}

func TestWaitForAdminProcessExited(t *testing.T) {
	mp := &mockProc{pid: 1, waitErr: fmt.Errorf("exit status 1")}

	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return mp, nil },
		runFn:   func(string, []string) (string, error) { return "", nil },
	}

	m := newTestManager(r)
	m.dialFn = func(string, time.Duration) (net.Conn, error) {
		return nil, fmt.Errorf("connection refused")
	}

	m.secretFile = "/dev/null"
	m.proc = mp
	go func() {
		m.err = mp.Wait()
		close(m.done)
	}()

	err := m.waitForAdmin(5 * time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exited before admin port") {
		t.Errorf("error = %q, want substring 'exited before admin port'", err.Error())
	}
}

func TestDiscardOldVCLsListError(t *testing.T) {
	r := &mockRunner{
		startFn: func(string, []string) (proc, error) { return &mockProc{pid: 1}, nil },
		runFn: func(_ string, args []string) (string, error) {
			for _, a := range args {
				if a == "vcl.list" {
					return "", fmt.Errorf("admin error")
				}
			}
			return "", nil
		},
	}

	m := newTestManager(r)
	m.secretFile = "/tmp/secret"

	// Should not panic — just return silently on error.
	m.discardOldVCLs("kv_reload_1")
}
