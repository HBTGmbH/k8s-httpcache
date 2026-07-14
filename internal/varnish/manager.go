// Package varnish manages the varnishd process lifecycle.
package varnish

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HBTGmbH/k8s-httpcache/internal/redact"
	"github.com/HBTGmbH/k8s-httpcache/internal/telemetry"
)

var (
	errVersionParse = errors.New("cannot parse cache version")
	errAdminTimeout = errors.New("timeout waiting for cache admin")
)

// runner abstracts external command execution for testing.
type runner interface {
	// start starts a long-running process and returns a handle.
	start(name string, args []string) (proc, error)
	// run runs a command to completion and returns its combined output.
	run(name string, args []string) (string, error)
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

// maxBufferedLine caps the bytes prefixWriter holds for a single not-yet-
// terminated line. A stream that never emits a newline (e.g. a misconfigured
// varnishncsa -F format) would otherwise grow buf without bound for the whole
// process lifetime; once a partial line exceeds this, it is flushed (with the
// prefix) and the buffer released. Normal line-oriented output never reaches
// this size, so steady-state behaviour is unchanged.
const maxBufferedLine = 1 << 20 // 1 MiB

// prefixWriter wraps an [io.Writer] and prepends a fixed prefix to every line.
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
		// Bound memory for a pathological newline-free stream: flush the
		// over-long partial line (prefixed) and release the buffer instead of
		// accumulating forever.
		if len(w.buf) >= maxBufferedLine {
			return total, w.flushOverlongLine()
		}

		return total, nil
	}

	// Fast path: single complete line with no buffered fragment.
	// This is the common case for varnishncsa (one line per write).
	// Skips the loop setup, empty-buf append, and reslicing.
	if len(w.buf) == 0 && nl == len(p)-1 {
		w.scratch = append(w.scratch, w.prefix...)
		w.scratch = append(w.scratch, p...)
		_, err := w.out.Write(w.scratch)
		if err != nil {
			return total, err //nolint:wrapcheck // pass through underlying writer error
		}

		return total, nil
	}

	w.scratch = append(w.scratch, w.prefix...)
	if len(w.buf) > 0 {
		w.scratch = append(w.scratch, w.buf...)
		w.buf = w.buf[:0]
	}
	w.scratch = append(w.scratch, p[:nl+1]...)
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

// Flush emits any buffered partial line (prefixed and newline-terminated).
// Called between varnishncsa restarts and at shutdown so a process dying
// mid-line neither loses its final output nor has the next process's first
// line appended to its truncated one - the terminating newline is what
// actually prevents the merge (unlike flushOverlongLine, which flushes
// MID-line and must not fabricate a line boundary in a continuing stream).
func (w *prefixWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	w.buf = append(w.buf, '\n')

	return w.flushOverlongLine()
}

// flushOverlongLine emits the buffered partial line (prefixed) and releases the
// buffer. It bounds memory when the upstream stream never delivers a newline;
// an absurdly long line is split across flushes (each re-prefixed), which is
// acceptable since such input is not genuinely line-oriented.
func (w *prefixWriter) flushOverlongLine() error {
	w.scratch = w.scratch[:0]
	w.scratch = append(w.scratch, w.prefix...)
	w.scratch = append(w.scratch, w.buf...)
	_, err := w.out.Write(w.scratch)
	w.buf = nil // release the backing array so capacity is not pinned high
	if err != nil {
		return err //nolint:wrapcheck // pass through underlying writer error
	}

	return nil
}

// defaultCLITimeout bounds run() subprocess calls (varnishadm, varnishstat).
// varnishadm's own CLI timeout only bounds a responsive-but-slow varnish; a
// wedged connection to a half-open admin socket would otherwise block
// CombinedOutput forever - and every adm() call runs on the event-loop
// goroutine, so one hung call would freeze all event handling including
// signal-driven shutdown. Generous enough for a large VCL compile.
const defaultCLITimeout = 60 * time.Second

// execRunner is the real implementation that uses os/exec.
type execRunner struct {
	stdout io.Writer
	stderr io.Writer
	// runTimeout bounds run() calls; the subprocess is killed at the
	// deadline. 0 means no bound. start() is never bounded (long-running).
	runTimeout time.Duration
}

func (r execRunner) start(name string, args []string) (proc, error) {
	cmd := exec.Command(name, args...) //nolint:gosec,noctx // G702: paths from CLI flags, not runtime input; noctx: cancelled via SIGTERM.
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr

	err := cmd.Start()
	if err != nil {
		return nil, err //nolint:wrapcheck // interface impl delegates to exec.Cmd
	}

	return &execProc{cmd: cmd}, nil
}

func (r execRunner) run(name string, args []string) (string, error) {
	ctx := context.Background()
	if r.runTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.runTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G702: paths from CLI flags, not runtime input
	out, err := cmd.CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// execProc wraps an [exec.Cmd] as a proc.
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
	majorVersion        atomic.Int64 // major version of varnishd (e.g. 6, 7, 8); atomic: read by the varnishstat scrape goroutine
	done                chan struct{}
	err                 error
	reloadCounter       atomic.Int64
	metrics             *telemetry.Metrics
	AdminTimeout        time.Duration
	ReloadRetries       int
	ReloadRetryInterval time.Duration
	VCLKept             int

	tlsMu      sync.Mutex        // serialises TLS cert stage→commit→discard
	tlsCertIDs map[string]string // logical cert name → active varnishadm cert id
	tlsCertDir string            // temp dir holding combined PEM files (lazily created)

	// outFlushers holds the redacting writers wrapping varnishd's
	// stdout/stderr. They line-buffer across writes, so the monitor goroutine
	// flushes them once varnishd exited (its final output may lack a
	// newline). Written once by SetRedactor before Start; read only by the
	// monitor goroutine (ordered by the go statement).
	outFlushers []flusher

	// ncsaFlushers holds the buffering writers in the varnishncsa output
	// chain (prefix + redaction). monitorNCSA flushes them after every
	// process exit; written once by StartNCSA before the monitor goroutine
	// starts (ordered by the go statement).
	ncsaFlushers []flusher

	ncsaMu     sync.Mutex // protects ncsaProc
	ncsaProc   proc
	ncsaRun    runner // separate runner with prefix writer for ncsa stdout
	ncsaPath   string
	ncsaArgs   []string
	ncsaStop   chan struct{}  // closed to stop monitorNCSA
	ncsaEvents chan NCSAEvent // buffered, read by event loop
	ncsaDone   chan struct{}  // closed when monitorNCSA exits

	NCSARestartDelay time.Duration // default 5s, exported for testing
	NCSAMaxCrashes   int           // max consecutive crashes; default 3, exported for testing
	NCSAStableUptime time.Duration // run time after which the crash counter resets; default 1m, exported for testing (0 = never reset)
	NCSAStopTimeout  time.Duration // wait after SIGTERM before SIGKILL on stop; default 5s, exported for testing
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
		run:              execRunner{stdout: os.Stdout, stderr: os.Stderr, runTimeout: defaultCLITimeout},
		done:             make(chan struct{}),
		metrics:          metrics,
		tlsCertIDs:       make(map[string]string),
		AdminTimeout:     30 * time.Second,
		NCSARestartDelay: 5 * time.Second,
		NCSAMaxCrashes:   3,
		NCSAStableUptime: time.Minute,
		NCSAStopTimeout:  5 * time.Second,
	}
}

// flusher is implemented by redacting writers that line-buffer output and
// must be flushed once the writing subprocess exited.
type flusher interface{ Flush() error }

// SetRedactor installs a Redactor that filters secret values from varnishd
// stdout/stderr and varnishadm command responses.
func (m *Manager) SetRedactor(r *redact.Redactor) {
	m.redactor = r
	if er, ok := m.run.(execRunner); ok {
		er.stdout = r.Writer(er.stdout)
		er.stderr = r.Writer(er.stderr)
		for _, w := range []io.Writer{er.stdout, er.stderr} {
			if f, ok := w.(flusher); ok {
				m.outFlushers = append(m.outFlushers, f)
			}
		}
		m.run = er
	}
}

// vclReloadPrefix is the naming prefix for VCL objects loaded by k8s-httpcache.
const vclReloadPrefix = "kv_reload_"

// versionRe matches "varnish-<major>." or "vinyl-<major>." version strings.
var versionRe = regexp.MustCompile(`(?:varnish|vinyl)-(\d+)\.`)

// trunkVersionRe matches "varnish-trunk" or "vinyl-trunk" (development branches).
var trunkVersionRe = regexp.MustCompile(`(?:varnish|vinyl)-trunk\b`)

// trunkMajorVersion is the synthetic major version assigned to trunk builds.
// It is set high so that trunk is always treated as the latest version.
const trunkMajorVersion = 99

// DetectVersion runs the cache daemon with -V and stores the major version number.
// It recognises both Varnish Cache ("varnish-7.6.1") and Vinyl Cache ("vinyl-9.0.0")
// version strings. Trunk builds are treated as the latest version.
// It is safe to call multiple times; Start calls it automatically if
// the version has not been detected yet.
func (m *Manager) DetectVersion() error {
	out, err := m.run.run(m.varnishdPath, []string{"-V"})
	if err != nil {
		return fmt.Errorf("running %s -V: %w", m.varnishdPath, err)
	}
	if trunkVersionRe.MatchString(out) {
		m.majorVersion.Store(trunkMajorVersion)
		m.log.Info("detected cache version", "major", "trunk")

		return nil
	}
	matches := versionRe.FindStringSubmatch(out)
	if len(matches) < 2 {
		return fmt.Errorf("%w from: %q", errVersionParse, out)
	}
	v, err := strconv.Atoi(matches[1])
	if err != nil {
		return fmt.Errorf("parsing major version %q: %w", matches[1], err)
	}
	m.majorVersion.Store(int64(v))
	m.log.Info("detected cache version", "major", v)

	return nil
}

// MajorVersion returns the detected varnishd major version (e.g. 6, 7, 8).
// Returns 0 if DetectVersion has not been called yet.
func (m *Manager) MajorVersion() int {
	return int(m.majorVersion.Load())
}

// Start launches the cache daemon in foreground mode with the given initial VCL file.
// It blocks until the admin port is ready or a timeout is reached.
func (m *Manager) Start(initialVCL string) error {
	if m.majorVersion.Load() == 0 {
		err := m.DetectVersion()
		if err != nil {
			return fmt.Errorf("detecting cache version: %w", err)
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

	p, err := m.run.start(m.varnishdPath, args)
	if err != nil {
		return fmt.Errorf("starting %s: %w", m.varnishdPath, err)
	}
	m.proc = p

	m.log.Info("started cache daemon", "cmd", m.varnishdPath, "pid", m.proc.Pid())

	// Monitor the process in the background.
	go func() {
		m.err = m.proc.Wait()
		// Wait has reaped the output-copy goroutines, so no Write can race
		// this flush of a final unterminated output line.
		for _, f := range m.outFlushers {
			_ = f.Flush()
		}
		close(m.done)
	}()

	// Wait for admin port to become ready.
	err = m.waitForAdmin(m.AdminTimeout)
	if err != nil {
		// Don't leave the freshly spawned varnishd behind: the caller exits
		// on this error, and an unkilled varnishd would survive it whenever
		// this process is not the container's PID 1 (shared PID namespace,
		// shell/tini wrapper), keeping the listen ports bound and turning a
		// transient slow start into a persistent crash loop.
		_ = m.proc.Signal(syscall.SIGKILL)
		<-m.done

		return fmt.Errorf("waiting for cache admin: %w", err)
	}

	m.log.Info("cache admin ready")

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
		ncsaStdout, ncsaStderr := m.buildNCSAWriters(os.Stdout, os.Stderr, prefix)
		m.ncsaRun = execRunner{stdout: ncsaStdout, stderr: ncsaStderr}
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

// StopNCSA gracefully stops the varnishncsa subprocess: SIGTERM first,
// escalating to SIGKILL after NCSAStopTimeout so a wedged varnishncsa
// (e.g. blocked on a full-disk log file) cannot stall shutdown indefinitely.
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
//
// Callers must first observe Done() being closed: m.err is written by the
// monitor goroutine immediately before it closes done, and that close is the
// only happens-before edge ordering the write against this read. Calling Err
// without having received from Done() is a data race.
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
// It handles both the nested format (Varnish 6.5+) and the flat 6.0-6.4 format.
func (m *Manager) ActiveSessions() (uint64, error) {
	args := []string{"-1", "-j"}
	if m.workDir != "" {
		args = []string{"-n", m.workDir, "-1", "-j"}
	}
	out, err := m.run.run(m.varnishstatPath, args)
	if err != nil {
		return 0, fmt.Errorf("varnishstat: %w", err)
	}

	return m.parseActiveSessions(out)
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
		out, err := m.run.run(m.varnishstatPath, args)
		if err != nil {
			return "", 0, fmt.Errorf("varnishstat: %w", err)
		}

		return out, int(m.majorVersion.Load()), nil
	}
}

// buildNCSAWriters wraps the base stdout/stderr for the varnishncsa
// subprocess: redaction first (innermost, so the prefix writer hands it
// complete lines), then the optional line prefix. Without the redaction wrap
// varnishncsa output would bypass the redactor entirely - an access-log
// format that echoes a header carrying a secret-derived value (e.g. an API
// key doubling as a Kubernetes Secret) would log it in cleartext.
func (m *Manager) buildNCSAWriters(stdout, stderr io.Writer, prefix string) (io.Writer, io.Writer) {
	// Collect every buffering layer separately and flush outermost-first:
	// the prefix writer's flush WRITES INTO the redacting writer, so the
	// redacting writers must be flushed after it (registering only the final
	// wrapped writer would strand the prefix flush in the redactor's
	// partial-line buffer).
	var redactFlushers []flusher
	if m.redactor != nil {
		stdout = m.redactor.Writer(stdout)
		stderr = m.redactor.Writer(stderr)
		for _, w := range []io.Writer{stdout, stderr} {
			if f, ok := w.(flusher); ok {
				redactFlushers = append(redactFlushers, f)
			}
		}
	}
	if prefix != "" {
		pw := newPrefixWriter(stdout, prefix)
		stdout = pw
		m.ncsaFlushers = append(m.ncsaFlushers, pw)
	}
	m.ncsaFlushers = append(m.ncsaFlushers, redactFlushers...)

	return stdout, stderr
}

// flushNCSAWriters drains partial-line buffers in the varnishncsa output
// chain. Called by monitorNCSA after each process exit: p.Wait() has reaped
// the output-copy goroutines by then, so no Write can race the flush.
func (m *Manager) flushNCSAWriters() {
	for _, f := range m.ncsaFlushers {
		_ = f.Flush()
	}
}

func (m *Manager) monitorNCSA() {
	defer close(m.ncsaDone)

	crashes := 0

	for {
		m.log.Debug("exec", "cmd", m.ncsaPath, "args", m.ncsaArgs)
		p, err := m.ncsaRun.start(m.ncsaPath, m.ncsaArgs)
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
		started := time.Now()

		waitDone := make(chan error, 1)
		go func() { waitDone <- p.Wait() }()
		select {
		case <-m.ncsaStop:
			_ = p.Signal(syscall.SIGTERM)
			select {
			case <-waitDone:
			case <-time.After(m.NCSAStopTimeout):
				m.log.Warn("varnishncsa did not exit after SIGTERM, forcing", "timeout", m.NCSAStopTimeout)
				_ = p.Signal(syscall.SIGKILL)
				// SIGKILL cannot be ignored; wait for the exit so the child
				// is reaped before StopNCSA returns.
				<-waitDone
			}
			m.flushNCSAWriters()
			m.ncsaMu.Lock()
			m.ncsaProc = nil
			m.ncsaMu.Unlock()

			return
		case waitErr := <-waitDone:
			// Emit any partial final line before a restarted varnishncsa's
			// first output would be appended to it.
			m.flushNCSAWriters()
			m.ncsaMu.Lock()
			m.ncsaProc = nil
			m.ncsaMu.Unlock()
			m.log.Warn("varnishncsa exited unexpectedly", "error", waitErr)
			m.sendNCSAEvent("Warning", "VarnishncsaExited",
				fmt.Sprintf("varnishncsa exited unexpectedly: %v", waitErr))
			// A run that stayed up long enough is not part of a crash
			// loop - reset the counter and do not count this exit, so only
			// consecutive rapid failures trip the breaker, not unrelated
			// exits spread over the process lifetime. Counting the stable
			// exit itself (crashes = 0 then crashes++) would leave crashes
			// at 1 and trip the breaker one rapid crash too early.
			if m.NCSAStableUptime > 0 && time.Since(started) >= m.NCSAStableUptime {
				crashes = 0
			} else {
				crashes++
			}
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
				// Reload runs on the event loop goroutine, so this wait
				// blocks all event handling (signals, varnishd exit, ncsa
				// events). Abort the retries when varnishd exits: further
				// attempts cannot succeed, and waiting would only delay the
				// loop's handling of the exit.
				select {
				case <-time.After(m.ReloadRetryInterval):
				case <-m.done:
					return lastErr
				}

				continue
			}

			return lastErr
		}

		// Activate it.
		resp, err = m.adm("vcl.use", name)
		if err != nil {
			// vcl.load succeeded but activation failed, leaving this VCL
			// loaded but unused. Discard it best-effort so a failed
			// activation doesn't leak a compiled VCL (which would otherwise
			// linger until a later reload's vcl.use happens to succeed).
			_, discardErr := m.adm("vcl.discard", name)
			if discardErr != nil {
				m.log.Warn("failed to discard VCL after vcl.use error", "name", name, "error", discardErr)
			}

			return fmt.Errorf("vcl.use: %w: %s", err, resp)
		}

		if attempt > 0 {
			m.log.Info("activated VCL after retry", "name", name, "attempts", attempt+1)
		} else {
			m.log.Info("activated VCL", "name", name)
		}

		// Discard old available VCLs (best-effort). Runs synchronously on the
		// reload path; the cleanup is a few fast varnishadm calls, and any
		// failure is logged and ignored rather than failing the reload.
		m.discardOldVCLs(name)

		return nil
	}

	return lastErr
}

// parseActiveSessions sums MEMPOOL.sess<N>.live counters, auto-detecting the
// varnishstat JSON schema from the content: the nested "counters" object
// (Varnish 6.5+) first, falling back to the flat top-level format (6.0-6.4).
// The schema must NOT be selected by major version: 6.5/6.6 already emit the
// nested format, and the flat parser would find no MEMPOOL keys at the top
// level - reporting 0 live sessions with no error, which would make graceful
// drain exit while client sessions are still active.
func (m *Manager) parseActiveSessions(out string) (uint64, error) {
	total, nested, err := m.parseActiveSessionsV7(out)
	if err != nil {
		return 0, err
	}
	if nested {
		return total, nil
	}

	return m.parseActiveSessionsV6(out)
}

// parseActiveSessionsV7 parses the nested schema. The second return value
// reports whether the input actually carried a top-level "counters" object;
// false means the flat 6.0-6.4 schema (flat counter keys always contain dots,
// so a bare "counters" key cannot occur there) and the caller must fall back
// to parseActiveSessionsV6.
func (*Manager) parseActiveSessionsV7(out string) (uint64, bool, error) {
	var data struct {
		Counters map[string]struct {
			Value uint64 `json:"value"`
		} `json:"counters"`
	}

	err := json.Unmarshal([]byte(out), &data)
	if err != nil {
		return 0, false, fmt.Errorf("parsing varnishstat JSON: %w", err)
	}
	if data.Counters == nil {
		return 0, false, nil
	}

	var total uint64
	for key, counter := range data.Counters {
		if strings.HasPrefix(key, "MEMPOOL.sess") && strings.HasSuffix(key, ".live") {
			total += counter.Value
		}
	}

	return total, true, nil
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

		// Run the ping asynchronously so a wedged varnishadm (hung admin
		// socket) cannot overshoot the deadline by up to defaultCLITimeout:
		// the deadline cuts the wait; the abandoned subprocess is bounded by
		// its own CLI timeout and exits on its own.
		pingDone := make(chan error, 1)
		go func() {
			_, err := m.adm("ping")
			pingDone <- err
		}()
		select {
		case err := <-pingDone:
			if err == nil {
				return nil
			}
			time.Sleep(250 * time.Millisecond)
		case <-time.After(time.Until(deadline)):
			return errAdminTimeout
		case <-m.done:
			return fmt.Errorf("varnishd exited before admin was ready: %w", m.err)
		}
	}

	return errAdminTimeout
}

func (m *Manager) adm(args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+4)
	if m.workDir != "" {
		cmdArgs = append(cmdArgs, "-n", m.workDir)
	}
	// Forward the CLI budget to varnishadm itself: without -t, varnishadm
	// applies its own default response timeout (5s on Varnish 7.0+), which
	// would fail any vcl.load whose VCC+cc compile takes longer - on every
	// reload AND every retry - defeating the generous defaultCLITimeout
	// chosen precisely for large compiles.
	cmdArgs = append(cmdArgs, "-t", strconv.Itoa(int(defaultCLITimeout/time.Second)))
	cmdArgs = append(cmdArgs, args...)

	m.log.Debug("exec", "cmd", m.varnishadmPath, "args", cmdArgs)

	resp, err := m.run.run(m.varnishadmPath, cmdArgs)
	if m.redactor != nil {
		resp = m.redactor.Redact(resp)
	}

	return resp, err
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

	// First pass: split label rows ("... <label> -> <target>") from VCL rows.
	// A label row is not a discardable VCL itself, and its target must be
	// protected: varnishd refuses to discard a labeled VCL, and the "name is
	// the last field" rule below would otherwise misread the label's target
	// as the row's VCL name and queue the still-referenced target for discard.
	labelTargets := make(map[string]struct{})
	var rows [][]string
	scanner := bufio.NewScanner(strings.NewReader(resp))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		if i := slices.Index(fields, "->"); i >= 0 {
			if i+1 < len(fields) {
				labelTargets[fields[i+1]] = struct{}{}
			}

			continue
		}
		rows = append(rows, fields)
	}
	err = scanner.Err()
	if err != nil {
		m.log.Warn("failed to parse vcl.list output, skipping VCL cleanup", "error", err)

		return
	}

	var available []string
	for _, fields := range rows {
		state := fields[0]

		// The VCL name is the last field, except when varnishd appends a
		// "(N label[s])" suffix to a labeled VCL's row - then it is the field
		// just before the "(" token.
		nameIdx := len(fields) - 1
		for i, f := range fields {
			if strings.HasPrefix(f, "(") {
				nameIdx = i - 1

				break
			}
		}
		if nameIdx < 1 {
			continue
		}
		name := fields[nameIdx]

		if state != "available" || name == currentName || !strings.HasPrefix(name, vclReloadPrefix) {
			continue
		}
		if _, labeled := labelTargets[name]; labeled {
			continue
		}
		available = append(available, name)
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
		slices.SortFunc(available, func(a, b string) int {
			return cmp.Compare(vclSuffix(b), vclSuffix(a)) // descending
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
