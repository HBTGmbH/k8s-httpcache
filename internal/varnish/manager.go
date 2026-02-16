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

// Manager manages the varnishd process and performs VCL reloads via varnishadm.
type Manager struct {
	varnishdPath   string
	varnishadmPath string
	adminAddr      string
	listenAddr     string
	extraArgs      []string
	secretPath     string
	secretFile     string
	cmd            *exec.Cmd
	done           chan struct{}
	err            error
	reloadCounter  atomic.Int64
}

// New creates a new varnish Manager. extraArgs are appended to the varnishd command line.
// If secretPath is non-empty, the secret is written to that fixed path; otherwise a temp file is used.
func New(varnishdPath, varnishadmPath, adminAddr, listenAddr, secretPath string, extraArgs []string) *Manager {
	return &Manager{
		varnishdPath:   varnishdPath,
		varnishadmPath: varnishadmPath,
		adminAddr:      adminAddr,
		listenAddr:     listenAddr,
		secretPath:     secretPath,
		extraArgs:      extraArgs,
		done:           make(chan struct{}),
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
		"-a", m.listenAddr,
		"-T", m.adminAddr,
		"-f", initialVCL,
		"-S", m.secretFile,
	}
	args = append(args, m.extraArgs...)
	m.cmd = exec.Command(m.varnishdPath, args...)
	m.cmd.Stdout = os.Stdout
	m.cmd.Stderr = os.Stderr

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("starting varnishd: %w", err)
	}

	log.Printf("varnish: started varnishd pid=%d", m.cmd.Process.Pid)

	// Monitor the process in the background.
	go func() {
		m.err = m.cmd.Wait()
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
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Signal(sig)
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
		os.Remove(m.secretFile)
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
		f.Close()
		os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name())
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

		conn, err := net.DialTimeout("tcp", m.adminAddr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for admin port %s", m.adminAddr)
}

func (m *Manager) adm(args ...string) (string, error) {
	cmdArgs := []string{"-T", m.adminAddr, "-S", m.secretFile}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.Command(m.varnishadmPath, cmdArgs...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
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
