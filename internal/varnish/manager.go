package varnish

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd}, nil
}

func (execRunner) Run(name string, args []string) (string, error) {
	cmd := exec.Command(name, args...)
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
	varnishdPath   string
	varnishadmPath string
	adminAddr      string
	listenAddrs    []string
	extraArgs      []string
	secretPath     string
	secretFile     string
	run            runner
	proc           proc
	dialFn         func(addr string, timeout time.Duration) (net.Conn, error)
	done           chan struct{}
	err            error
	reloadCounter  atomic.Int64
}

// New creates a new varnish Manager. listenAddrs are passed as individual -a
// flags to varnishd. extraArgs are appended to the varnishd command line.
// If secretPath is non-empty, the secret is written to that fixed path; otherwise a temp file is used.
func New(varnishdPath, varnishadmPath, adminAddr string, listenAddrs []string, secretPath string, extraArgs []string) *Manager {
	return &Manager{
		varnishdPath:   varnishdPath,
		varnishadmPath: varnishadmPath,
		adminAddr:      adminAddr,
		listenAddrs:    listenAddrs,
		secretPath:     secretPath,
		extraArgs:      extraArgs,
		run:            execRunner{},
		dialFn: func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, timeout)
		},
		done: make(chan struct{}),
	}
}

// Start launches varnishd in foreground mode with the given initial VCL file.
// It blocks until the admin port is ready or a timeout is reached.
func (m *Manager) Start(initialVCL string) error {
	secret, err := m.generateSecret()
	if err != nil {
		return fmt.Errorf("generating secret: %w", err)
	}
	m.secretFile = secret

	args := []string{
		"-F",
	}
	for _, la := range m.listenAddrs {
		args = append(args, "-a", la)
	}
	args = append(args,
		"-T", m.adminAddr,
		"-f", initialVCL,
		"-S", m.secretFile,
	)
	args = append(args, m.extraArgs...)

	p, err := m.run.Start(m.varnishdPath, args)
	if err != nil {
		return fmt.Errorf("starting varnishd: %w", err)
	}
	m.proc = p

	log.Printf("varnish: started varnishd pid=%d", m.proc.Pid())

	// Monitor the process in the background.
	go func() {
		m.err = m.proc.Wait()
		close(m.done)
	}()

	// Wait for admin port to become ready.
	if err := m.waitForAdmin(30 * time.Second); err != nil {
		return fmt.Errorf("waiting for varnish admin: %w", err)
	}

	log.Printf("varnish: admin port %s is ready", m.adminAddr)
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

	log.Printf("varnish: activated VCL %s", name)

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

// Cleanup removes temporary files.
func (m *Manager) Cleanup() {
	if m.secretFile != "" {
		_ = os.Remove(m.secretFile)
	}
}

func (m *Manager) generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	var (
		f   *os.File
		err error
	)
	if m.secretPath != "" {
		if err := os.MkdirAll(filepath.Dir(m.secretPath), 0o700); err != nil {
			return "", err
		}
		f, err = os.OpenFile(m.secretPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	} else {
		f, err = os.CreateTemp("", "k8s-httpcache-secret-*")
	}
	if err != nil {
		return "", err
	}

	if _, err := fmt.Fprintf(f, "%x\n", b); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
}

func (m *Manager) waitForAdmin(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Check if varnishd already exited.
		select {
		case <-m.done:
			return fmt.Errorf("varnishd exited before admin port was ready: %v", m.err)
		default:
		}

		conn, err := m.dialFn(m.adminAddr, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for admin port %s", m.adminAddr)
}

func (m *Manager) adm(args ...string) (string, error) {
	cmdArgs := []string{"-T", m.adminAddr, "-S", m.secretFile}
	cmdArgs = append(cmdArgs, args...)

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
				log.Printf("varnish: failed to discard VCL %s: %v", name, err)
			}
		}
	}
}

// ParseAdminPort extracts the port number from the admin address.
func ParseAdminPort(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(portStr)
}
