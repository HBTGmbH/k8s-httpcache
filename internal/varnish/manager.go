// Package varnish manages the varnishd process lifecycle.
package varnish

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"k8s-httpcache/internal/redact"
	"k8s-httpcache/internal/telemetry"
)

// runner abstracts external command execution for testing.
type runner interface {
	// Start starts a long-running process and returns a handle.
	Start(name string, args []string) (proc, error)
	// Run runs a command to completion and returns its combined output.
	Run(name string, args []string) (string, error)
}

// proc represents a running process.
type proc interface {
	Wait() error
	Signal(sig os.Signal) error
	Pid() int
}

// NCSAEvent represents a lifecycle event for the varnishncsa subprocess.
type NCSAEvent struct {
	Type    string // "Warning" or "Normal" (maps to v1.EventType*)
	Reason  string // e.g. "VarnishncsaExited"
	Message string
}

// prefixWriter wraps an io.Writer and prepends a fixed prefix to every line.
// It buffers partial lines so the prefix appears exactly once at the start of
// each newline-terminated line, even when Write is called with fragments.
//
// The scratch buffer is reused across calls so that, after a brief warm-up
// period, Write is zero-allocation in steady state.
type prefixWriter struct {
	out     io.Writer
	prefix  []byte
	buf     []byte // buffered partial line (no trailing newline yet)
	scratch []byte // reusable output buffer, retained between calls
}

func newPrefixWriter(out io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{out: out, prefix: []byte(prefix)}
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	total := len(p)
	w.scratch = w.scratch[:0]

	// First line: the IndexByte result doubles as the fragment check
	// (nl < 0 means no newline → buffer and return) and locates the
	// first newline position, avoiding a redundant scan.
	nl := bytes.IndexByte(p, '\n')
	if nl < 0 {
		w.buf = append(w.buf, p...)

		return total, nil
	}
	w.scratch = append(w.scratch, w.prefix...)
	w.scratch = append(w.scratch, w.buf...)
	w.scratch = append(w.scratch, p[:nl+1]...)
	w.buf = w.buf[:0]
	p = p[nl+1:]

	// Remaining lines: no buffered fragment, one Write call for the
	// entire batch instead of one per line.
	for len(p) > 0 {
		nl = bytes.IndexByte(p, '\n')
		if nl < 0 {
			w.buf = append(w.buf, p...)

			break
		}
		w.scratch = append(w.scratch, w.prefix...)
		w.scratch = append(w.scratch, p[:nl+1]...)
		p = p[nl+1:]
	}

	_, err := w.out.Write(w.scratch)
	if err != nil {
		return total, err //nolint:wrapcheck // pass through underlying writer error
	}

	return total, nil
}

// execRunner is the real implementation that uses os/exec.
type execRunner struct {
	stdout io.Writer
	stderr io.Writer
}

func (r execRunner) Start(name string, args []string) (proc, error) {
	cmd := exec.Command(name, args...) //nolint:noctx // varnishd runs for the container's lifetime; cancelled via SIGTERM, not context.
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr

	err := cmd.Start()
	if err != nil {
		return nil, err //nolint:wrapcheck // interface impl delegates to exec.Cmd
	}

	return &execProc{cmd: cmd}, nil
}

func (execRunner) Run(name string, args []string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:noctx // varnishadm commands are short-lived; timeout is handled by the admin socket.
	out, err := cmd.CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// execProc wraps an exec.Cmd as a proc.
type execProc struct{ cmd *exec.Cmd }

func (p *execProc) Wait() error                { return p.cmd.Wait() }              //nolint:wrapcheck // interface impl delegates to exec.Cmd
func (p *execProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) } //nolint:wrapcheck // interface impl delegates to exec.Cmd
func (p *execProc) Pid() int                   { return p.cmd.Process.Pid }

// Manager manages the varnishd process and performs VCL reloads via varnishadm.
type Manager struct {
	varnishdPath        string
	varnishadmPath      string
	varnishstatPath     string
	listenAddrs         []string
	extraArgs           []string
	workDir             string // varnishd -n instance name, forwarded to varnishadm/varnishstat
	log                 *slog.Logger
	run                 runner
	redactor            *redact.Redactor
	proc                proc
	majorVersion        int // major version of varnishd (e.g. 6, 7, 8)
	done                chan struct{}
	err                 error
	reloadCounter       atomic.Int64
	metrics             *telemetry.Metrics
	AdminTimeout        time.Duration
	ReloadRetries       int
	ReloadRetryInterval time.Duration
	VCLKept             int

	ncsaMu     sync.Mutex // protects ncsaProc
	ncsaProc   proc
	ncsaRun    runner // separate runner with prefix writer for ncsa stdout
	ncsaPath   string
	ncsaArgs   []string
	ncsaStop   chan struct{}  // closed to stop monitorNCSA
	ncsaEvents chan NCSAEvent // buffered, read by event loop
	ncsaDone   chan struct{}  // closed when monitorNCSA exits

	NCSARestartDelay time.Duration // default 5s, exported for testing
	NCSAMaxCrashes   int           // default 3, exported for testing
	ncsaCrashed      chan struct{} // closed when crash limit is reached
}

// extractWorkDir scans args for a -n flag and returns its value.
// It handles both "-n value" (separate args) and "-nvalue" (concatenated).
func extractWorkDir(args []string) string {
	for i, a := range args {
		if a == "-n" {
			if i+1 < len(args) {
				return args[i+1]
			}

			return ""
		}
		if strings.HasPrefix(a, "-n") && len(a) > 2 {
			return a[2:]
		}
	}

	return ""
}

// defaultVarnishstatPath is the default binary name for varnishstat.
const defaultVarnishstatPath = "varnishstat"

// New creates a new varnish Manager. listenAddrs are passed as individual -a
// flags to varnishd. extraArgs are appended to the varnishd command line.
// varnishstatPath is the path to the varnishstat binary (defaults to "varnishstat" if empty).
func New(varnishdPath, varnishadmPath string, listenAddrs, extraArgs []string, varnishstatPath string, metrics *telemetry.Metrics) *Manager {
	if metrics == nil {
		panic("varnish: metrics must not be nil")
	}
	if varnishstatPath == "" {
		varnishstatPath = defaultVarnishstatPath
	}

	return &Manager{
		varnishdPath:     varnishdPath,
		varnishadmPath:   varnishadmPath,
		varnishstatPath:  varnishstatPath,
		listenAddrs:      listenAddrs,
		extraArgs:        extraArgs,
		workDir:          extractWorkDir(extraArgs),
		log:              slog.Default(),
		run:              execRunner{stdout: os.Stdout, stderr: os.Stderr},
		done:             make(chan struct{}),
		metrics:          metrics,
		AdminTimeout:     30 * time.Second,
		NCSARestartDelay: 5 * time.Second,
		NCSAMaxCrashes:   3,
	}
}

// SetRedactor installs a Redactor that filters secret values from varnishd
// stdout/stderr and varnishadm command responses.
func (m *Manager) SetRedactor(r *redact.Redactor) {
	m.redactor = r
	if er, ok := m.run.(execRunner); ok {
		er.stdout = r.Writer(er.stdout)
		er.stderr = r.Writer(er.stderr)
		m.run = er
	}
}

// vclReloadPrefix is the naming prefix for VCL objects loaded by k8s-httpcache.
const vclReloadPrefix = "kv_reload_"

var versionRe = regexp.MustCompile(`varnish-(\d+)\.`)

// trunkVersionRe matches "varnish-trunk" (the development branch).
var trunkVersionRe = regexp.MustCompile(`varnish-trunk\b`)

// trunkMajorVersion is the synthetic major version assigned to trunk builds.
// It is set high so that trunk is always treated as the latest version.
const trunkMajorVersion = 99

// DetectVersion runs varnishd -V and stores the major version number.
// It is safe to call multiple times; Start calls it automatically if
// the version has not been detected yet. Trunk builds ("varnish-trunk")
// are treated as the latest version.
func (m *Manager) DetectVersion() error {
	out, err := m.run.Run(m.varnishdPath, []string{"-V"})
	if err != nil {
		return fmt.Errorf("running varnishd -V: %w", err)
	}
	if trunkVersionRe.MatchString(out) {
		m.majorVersion = trunkMajorVersion
		m.log.Info("detected varnish version", "major", "trunk")

		return nil
	}
	matches := versionRe.FindStringSubmatch(out)
	if len(matches) < 2 {
		return fmt.Errorf("cannot parse varnish version from: %q", out)
	}
	v, err := strconv.Atoi(matches[1])
	if err != nil {
		return fmt.Errorf("parsing major version %q: %w", matches[1], err)
	}
	m.majorVersion = v
	m.log.Info("detected varnish version", "major", m.majorVersion)

	return nil
}

// MajorVersion returns the detected varnishd major version (e.g. 6, 7, 8).
// Returns 0 if DetectVersion has not been called yet.
func (m *Manager) MajorVersion() int {
	return m.majorVersion
}

// Start launches varnishd in foreground mode with the given initial VCL file.
// It blocks until the admin port is ready or a timeout is reached.
func (m *Manager) Start(initialVCL string) error {
	if m.majorVersion == 0 {
		err := m.DetectVersion()
		if err != nil {
			return fmt.Errorf("detecting varnish version: %w", err)
		}
	}

	args := []string{
		"-F",
	}
	for _, la := range m.listenAddrs {
		args = append(args, "-a", la)
	}
	args = append(args,
		"-f", initialVCL,
	)
	args = append(args, m.extraArgs...)

	m.log.Debug("exec", "cmd", m.varnishdPath, "args", args)

	p, err := m.run.Start(m.varnishdPath, args)
	if err != nil {
		return fmt.Errorf("starting varnishd: %w", err)
	}
	m.proc = p

	m.log.Info("started varnishd", "pid", m.proc.Pid())

	// Monitor the process in the background.
	go func() {
		m.err = m.proc.Wait()
		close(m.done)
	}()

	// Wait for admin port to become ready.
	err = m.waitForAdmin(m.AdminTimeout)
	if err != nil {
		return fmt.Errorf("waiting for varnish admin: %w", err)
	}

	m.log.Info("varnish admin ready")

	return nil
}

// Reload loads a new VCL file and activates it, recording the elapsed time
// in the VCLReloadDurationSeconds histogram.
// It retries vcl.load failures up to ReloadRetries times (vcl.use failures
// are not retried). Each attempt uses a fresh VCL name.
func (m *Manager) Reload(vclPath string) error {
	start := time.Now()
	err := m.reload(vclPath)
	m.metrics.VCLReloadDurationSeconds.Observe(time.Since(start).Seconds())

	return err
}

// ForwardSignal sends a signal to the varnishd process.
// SIGKILL is also forwarded to varnishncsa if running.
func (m *Manager) ForwardSignal(sig os.Signal) {
	if m.proc != nil {
		_ = m.proc.Signal(sig)
	}
	if sig == syscall.SIGKILL {
		m.ncsaMu.Lock()
		p := m.ncsaProc
		m.ncsaMu.Unlock()
		if p != nil {
			_ = p.Signal(sig)
		}
	}
}

// StartNCSA launches the varnishncsa subprocess with auto-restart.
// If ncsaPath is empty, it is a no-op. When prefix is non-empty, every
// line written to stdout by varnishncsa is prefixed with the given string.
func (m *Manager) StartNCSA(ncsaPath string, args []string, prefix string) {
	if ncsaPath == "" {
		return
	}
	m.ncsaPath = ncsaPath
	m.ncsaEvents = make(chan NCSAEvent, 8)
	m.ncsaStop = make(chan struct{})
	m.ncsaDone = make(chan struct{})
	m.ncsaCrashed = make(chan struct{})

	// Build a runner whose stdout is wrapped with the prefix writer.
	// When ncsaRun is already set (e.g. by tests), keep it as-is.
	if m.ncsaRun == nil {
		ncsaStdout := io.Writer(os.Stdout)
		if prefix != "" {
			ncsaStdout = newPrefixWriter(os.Stdout, prefix)
		}
		m.ncsaRun = execRunner{stdout: ncsaStdout, stderr: os.Stderr}
	}

	m.ncsaArgs = make([]string, 0, len(args)+2)
	if m.workDir != "" {
		m.ncsaArgs = append(m.ncsaArgs, "-n", m.workDir)
	}
	m.ncsaArgs = append(m.ncsaArgs, args...)

	go m.monitorNCSA()
}

// NCSAEvents returns the channel for varnishncsa lifecycle events.
// Returns nil when varnishncsa is not enabled.
func (m *Manager) NCSAEvents() <-chan NCSAEvent { return m.ncsaEvents }

// NCSACrashed returns a channel that is closed when the varnishncsa crash
// limit is reached. Returns nil when StartNCSA was never called, which
// makes the corresponding select case a no-op.
func (m *Manager) NCSACrashed() <-chan struct{} { return m.ncsaCrashed }

// StopNCSA gracefully stops the varnishncsa subprocess.
// It is a no-op if StartNCSA was never called.
func (m *Manager) StopNCSA() {
	if m.ncsaStop == nil {
		return
	}
	close(m.ncsaStop)
	<-m.ncsaDone
}

// Done returns a channel that is closed when varnishd exits.
func (m *Manager) Done() <-chan struct{} {
	return m.done
}

// Err returns the error from varnishd exiting, if any.
func (m *Manager) Err() error {
	return m.err
}

// MarkBackendSick sets the named backend to "sick" via varnishadm.
func (m *Manager) MarkBackendSick(name string) error {
	_, err := m.adm("backend.set_health", name, "sick")

	return err
}

// ActiveSessions returns the total number of live client sessions by summing
// MEMPOOL.sess<N>.live counters from varnishstat -1 -j output.
// It handles both the Varnish 7+ nested format and the Varnish 6.x flat format.
func (m *Manager) ActiveSessions() (uint64, error) {
	args := []string{"-1", "-j"}
	if m.workDir != "" {
		args = []string{"-n", m.workDir, "-1", "-j"}
	}
	out, err := m.run.Run(m.varnishstatPath, args)
	if err != nil {
		return 0, fmt.Errorf("varnishstat: %w", err)
	}
	if m.majorVersion < 7 {
		return m.parseActiveSessionsV6(out)
	}

	return m.parseActiveSessionsV7(out)
}

// VarnishstatFunc returns a closure that runs varnishstat -1 -j and returns
// the raw JSON output along with the detected major version. This is used by
// the Prometheus varnishstat collector without creating an import cycle.
func (m *Manager) VarnishstatFunc() func() (string, int, error) {
	return func() (string, int, error) {
		args := []string{"-1", "-j"}
		if m.workDir != "" {
			args = []string{"-n", m.workDir, "-1", "-j"}
		}
		out, err := m.run.Run(m.varnishstatPath, args)
		if err != nil {
			return "", 0, fmt.Errorf("varnishstat: %w", err)
		}

		return out, m.majorVersion, nil
	}
}

func (m *Manager) monitorNCSA() {
	defer close(m.ncsaDone)

	crashes := 0

	for {
		m.log.Debug("exec", "cmd", m.ncsaPath, "args", m.ncsaArgs)
		p, err := m.ncsaRun.Start(m.ncsaPath, m.ncsaArgs)
		if err != nil {
			m.log.Error("failed to start varnishncsa", "error", err)
			m.sendNCSAEvent("Warning", "VarnishncsaStartFailed",
				fmt.Sprintf("Failed to start varnishncsa: %v", err))
			crashes++
			if m.NCSAMaxCrashes > 0 && crashes >= m.NCSAMaxCrashes {
				m.log.Error("varnishncsa crash loop detected, giving up", "crashes", crashes)
				m.sendNCSAEvent("Warning", "VarnishncsaCrashLoop",
					fmt.Sprintf("varnishncsa crashed %d times consecutively, giving up", crashes))
				close(m.ncsaCrashed)

				return
			}
			select {
			case <-m.ncsaStop:
				return
			case <-time.After(m.NCSARestartDelay):
				continue
			}
		}
		m.ncsaMu.Lock()
		m.ncsaProc = p
		m.ncsaMu.Unlock()
		m.log.Info("started varnishncsa", "pid", p.Pid())

		waitDone := make(chan error, 1)
		go func() { waitDone <- p.Wait() }()
		select {
		case <-m.ncsaStop:
			_ = p.Signal(syscall.SIGTERM)
			<-waitDone
			m.ncsaMu.Lock()
			m.ncsaProc = nil
			m.ncsaMu.Unlock()

			return
		case waitErr := <-waitDone:
			m.ncsaMu.Lock()
			m.ncsaProc = nil
			m.ncsaMu.Unlock()
			m.log.Warn("varnishncsa exited unexpectedly", "error", waitErr)
			m.sendNCSAEvent("Warning", "VarnishncsaExited",
				fmt.Sprintf("varnishncsa exited unexpectedly: %v", waitErr))
			crashes++
			if m.NCSAMaxCrashes > 0 && crashes >= m.NCSAMaxCrashes {
				m.log.Error("varnishncsa crash loop detected, giving up", "crashes", crashes)
				m.sendNCSAEvent("Warning", "VarnishncsaCrashLoop",
					fmt.Sprintf("varnishncsa crashed %d times consecutively, giving up", crashes))
				close(m.ncsaCrashed)

				return
			}
			select {
			case <-m.ncsaStop:
				return
			case <-time.After(m.NCSARestartDelay):
				m.sendNCSAEvent("Normal", "VarnishncsaRestarted",
					"Restarting varnishncsa after unexpected exit")
			}
		}
	}
}

func (m *Manager) sendNCSAEvent(eventType, reason, message string) {
	if m.ncsaEvents == nil {
		return
	}
	select {
	case m.ncsaEvents <- NCSAEvent{Type: eventType, Reason: reason, Message: message}:
	default:
		m.log.Warn("ncsa event channel full, dropping event", "reason", reason)
	}
}

func (m *Manager) reload(vclPath string) error {
	maxAttempts := 1 + m.ReloadRetries

	var lastErr error
	for attempt := range maxAttempts {
		n := m.reloadCounter.Add(1)
		name := fmt.Sprintf("%s%d", vclReloadPrefix, n)

		// Load the new VCL.
		resp, err := m.adm("vcl.load", name, vclPath)
		if err != nil {
			lastErr = fmt.Errorf("vcl.load: %w: %s", err, resp)

			if attempt < maxAttempts-1 {
				m.metrics.VCLReloadRetriesTotal.Inc()
				m.log.Warn("vcl.load failed, retrying",
					"attempt", attempt+1,
					"max_attempts", maxAttempts,
					"error", lastErr,
				)
				time.Sleep(m.ReloadRetryInterval)

				continue
			}

			return lastErr
		}

		// Activate it.
		resp, err = m.adm("vcl.use", name)
		if err != nil {
			return fmt.Errorf("vcl.use: %w: %s", err, resp)
		}

		if attempt > 0 {
			m.log.Info("activated VCL after retry", "name", name, "attempts", attempt+1)
		} else {
			m.log.Info("activated VCL", "name", name)
		}

		// Discard old available VCLs in the background (best-effort).
		m.discardOldVCLs(name)

		return nil
	}

	return lastErr
}

func (*Manager) parseActiveSessionsV7(out string) (uint64, error) {
	var data struct {
		Counters map[string]struct {
			Value uint64 `json:"value"`
		} `json:"counters"`
	}

	err := json.Unmarshal([]byte(out), &data)
	if err != nil {
		return 0, fmt.Errorf("parsing varnishstat JSON: %w", err)
	}

	var total uint64
	for key, counter := range data.Counters {
		if strings.HasPrefix(key, "MEMPOOL.sess") && strings.HasSuffix(key, ".live") {
			total += counter.Value
		}
	}

	return total, nil
}

func (*Manager) parseActiveSessionsV6(out string) (uint64, error) {
	var data map[string]json.RawMessage

	err := json.Unmarshal([]byte(out), &data)
	if err != nil {
		return 0, fmt.Errorf("parsing varnishstat JSON: %w", err)
	}

	var total uint64
	for key, raw := range data {
		if !strings.HasPrefix(key, "MEMPOOL.sess") || !strings.HasSuffix(key, ".live") {
			continue
		}
		var counter struct {
			Value uint64 `json:"value"`
		}

		err := json.Unmarshal(raw, &counter)
		if err != nil {
			return 0, fmt.Errorf("parsing varnishstat counter %q: %w", key, err)
		}
		total += counter.Value
	}

	return total, nil
}

func (m *Manager) waitForAdmin(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Check if varnishd already exited.
		select {
		case <-m.done:
			return fmt.Errorf("varnishd exited before admin was ready: %w", m.err)
		default:
		}

		_, err := m.adm("ping")
		if err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	return errors.New("timeout waiting for varnish admin")
}

func (m *Manager) adm(args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if m.workDir != "" {
		cmdArgs = append(cmdArgs, "-n", m.workDir)
	}
	cmdArgs = append(cmdArgs, args...)

	m.log.Debug("exec", "cmd", m.varnishadmPath, "args", cmdArgs)

	resp, err := m.run.Run(m.varnishadmPath, cmdArgs)
	if m.redactor != nil {
		resp = m.redactor.Redact(resp)
	}

	return resp, err //nolint:wrapcheck // internal helper; callers add context
}

// vclSuffix parses the numeric suffix from a VCL name matching the
// vclReloadPrefix pattern (e.g. "kv_reload_42" → 42).
// Returns -1 for names that don't match.
func vclSuffix(name string) int64 {
	if !strings.HasPrefix(name, vclReloadPrefix) {
		return -1
	}
	n, err := strconv.ParseInt(name[len(vclReloadPrefix):], 10, 64)
	if err != nil {
		return -1
	}

	return n
}

func (m *Manager) discardOldVCLs(currentName string) {
	resp, err := m.adm("vcl.list")
	if err != nil {
		return
	}

	var available []string
	scanner := bufio.NewScanner(strings.NewReader(resp))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		state := fields[0]
		name := fields[len(fields)-1]

		if state == "available" && name != currentName && strings.HasPrefix(name, vclReloadPrefix) {
			available = append(available, name)
		}
	}

	if len(available) == 0 {
		return
	}

	// When VCLKept > 0 and we have fewer (or equal) available VCLs than
	// the retention limit, keep everything.
	if m.VCLKept > 0 && len(available) <= m.VCLKept {
		return
	}

	// When VCLKept > 0, sort descending by suffix (higher = newer) and
	// only discard the tail beyond the retention limit.
	if m.VCLKept > 0 {
		sort.Slice(available, func(i, j int) bool {
			return vclSuffix(available[i]) > vclSuffix(available[j])
		})
		available = available[m.VCLKept:]
	}

	for _, name := range available {
		_, err := m.adm("vcl.discard", name)
		if err != nil {
			m.log.Warn("failed to discard VCL", "name", name, "error", err)
		}
	}
}
