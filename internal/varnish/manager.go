// Package varnish manages the varnishd process lifecycle.
package varnish

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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
	Signal(os.Signal) error
	Pid() int
}

// execRunner is the real implementation that uses os/exec.
type execRunner struct{}

func (execRunner) Start(name string, args []string) (proc, error) {
	cmd := exec.Command(name, args...) //nolint:noctx // varnishd runs for the container's lifetime; cancelled via SIGTERM, not context.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
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

func (p *execProc) Wait() error                { return p.cmd.Wait() }
func (p *execProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p *execProc) Pid() int                   { return p.cmd.Process.Pid }

// Manager manages the varnishd process and performs VCL reloads via varnishadm.
type Manager struct {
	varnishdPath    string
	varnishadmPath  string
	varnishstatPath string
	listenAddrs     []string
	extraArgs       []string
	workDir         string // varnishd -n instance name, forwarded to varnishadm/varnishstat
	run             runner
	proc            proc
	majorVersion    int // major version of varnishd (e.g. 6, 7, 8)
	done            chan struct{}
	err             error
	reloadCounter   atomic.Int64
	AdminTimeout    time.Duration
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

// New creates a new varnish Manager. listenAddrs are passed as individual -a
// flags to varnishd. extraArgs are appended to the varnishd command line.
// varnishstatPath is the path to the varnishstat binary (defaults to "varnishstat" if empty).
func New(varnishdPath, varnishadmPath string, listenAddrs []string, extraArgs []string, varnishstatPath string) *Manager {
	if varnishstatPath == "" {
		varnishstatPath = "varnishstat"
	}
	return &Manager{
		varnishdPath:    varnishdPath,
		varnishadmPath:  varnishadmPath,
		varnishstatPath: varnishstatPath,
		listenAddrs:     listenAddrs,
		extraArgs:       extraArgs,
		workDir:         extractWorkDir(extraArgs),
		run:             execRunner{},
		done:            make(chan struct{}),
		AdminTimeout:    30 * time.Second,
	}
}

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
		slog.Info("detected varnish version", "major", "trunk")
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
	slog.Info("detected varnish version", "major", m.majorVersion)
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
		if err := m.DetectVersion(); err != nil {
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

	slog.Debug("exec", "cmd", m.varnishdPath, "args", args)

	p, err := m.run.Start(m.varnishdPath, args)
	if err != nil {
		return fmt.Errorf("starting varnishd: %w", err)
	}
	m.proc = p

	slog.Info("started varnishd", "pid", m.proc.Pid())

	// Monitor the process in the background.
	go func() {
		m.err = m.proc.Wait()
		close(m.done)
	}()

	// Wait for admin port to become ready.
	if err := m.waitForAdmin(m.AdminTimeout); err != nil {
		return fmt.Errorf("waiting for varnish admin: %w", err)
	}

	slog.Info("varnish admin ready")
	return nil
}

// Reload loads a new VCL file and activates it.
func (m *Manager) Reload(vclPath string) error {
	n := m.reloadCounter.Add(1)
	name := fmt.Sprintf("kv_reload_%d", n)

	// Load the new VCL.
	resp, err := m.adm("vcl.load", name, vclPath)
	if err != nil {
		return fmt.Errorf("vcl.load: %w: %s", err, resp)
	}

	// Activate it.
	resp, err = m.adm("vcl.use", name)
	if err != nil {
		return fmt.Errorf("vcl.use: %w: %s", err, resp)
	}

	slog.Info("activated VCL", "name", name)

	// Discard old available VCLs in the background (best-effort).
	m.discardOldVCLs(name)

	return nil
}

// ForwardSignal sends a signal to the varnishd process.
func (m *Manager) ForwardSignal(sig os.Signal) {
	if m.proc != nil {
		_ = m.proc.Signal(sig)
	}
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
func (m *Manager) ActiveSessions() (int64, error) {
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

func (m *Manager) parseActiveSessionsV7(out string) (int64, error) {
	var data struct {
		Counters map[string]struct {
			Value uint64 `json:"value"`
		} `json:"counters"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return 0, fmt.Errorf("parsing varnishstat JSON: %w", err)
	}

	var total int64
	for key, counter := range data.Counters {
		if strings.HasPrefix(key, "MEMPOOL.sess") && strings.HasSuffix(key, ".live") {
			total += int64(counter.Value)
		}
	}
	return total, nil
}

func (m *Manager) parseActiveSessionsV6(out string) (int64, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return 0, fmt.Errorf("parsing varnishstat JSON: %w", err)
	}

	var total int64
	for key, raw := range data {
		if !strings.HasPrefix(key, "MEMPOOL.sess") || !strings.HasSuffix(key, ".live") {
			continue
		}
		var counter struct {
			Value uint64 `json:"value"`
		}
		if err := json.Unmarshal(raw, &counter); err != nil {
			return 0, fmt.Errorf("parsing varnishstat counter %q: %w", key, err)
		}
		total += int64(counter.Value)
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

		if _, err := m.adm("ping"); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("timeout waiting for varnish admin")
}

func (m *Manager) adm(args ...string) (string, error) {
	var cmdArgs []string
	if m.workDir != "" {
		cmdArgs = []string{"-n", m.workDir}
	}
	cmdArgs = append(cmdArgs, args...)

	slog.Debug("exec", "cmd", m.varnishadmPath, "args", cmdArgs)

	return m.run.Run(m.varnishadmPath, cmdArgs)
}

func (m *Manager) discardOldVCLs(currentName string) {
	resp, err := m.adm("vcl.list")
	if err != nil {
		return
	}

	scanner := bufio.NewScanner(strings.NewReader(resp))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		state := fields[0]
		name := fields[len(fields)-1]

		// Only discard "available" VCLs that are not the current one.
		if state == "available" && name != currentName {
			if _, err := m.adm("vcl.discard", name); err != nil {
				slog.Warn("failed to discard VCL", "name", name, "error", err)
			}
		}
	}
}
