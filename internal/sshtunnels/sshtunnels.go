// Package sshtunnels manages on-demand SSH tunnels to remote hosts.
package sshtunnels

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"sync"
)

// tunnel represents a single SSH tunnel to a remote host.
type tunnel struct {
	host string
	port int
	cmd  *exec.Cmd
}

// Manager holds all active SSH tunnels.
type Manager struct {
	mu        sync.Mutex
	tunnels   map[string]*tunnel
	startPort int
}

// NewManager creates a tunnel manager.
// startPort is the first port to try for local tunnel endpoints.
// remotePort is the HTTP port the remote serveroute listens on.
func NewManager(startPort int) *Manager {
	return &Manager{
		tunnels:   make(map[string]*tunnel),
		startPort: startPort,
	}
}

// Get returns the local port for the tunnel to host.
// If no tunnel exists, it spawns one lazily.
func (m *Manager) Get(host string) (int, error) {
	m.mu.Lock()
	t, ok := m.tunnels[host]
	if ok {
		m.mu.Unlock()
		return t.port, nil
	}

	// Find a free port.
	port, err := m.findFreePort()
	if err != nil {
		m.mu.Unlock()
		return 0, fmt.Errorf("find free port for ssh to %s: %w", host, err)
	}

	// Build the ssh command: ssh -L 127.0.0.1:<port>:localhost:80 <host>
	// The remote HTTP port is known from shared config; default 80.
	remotePort := 80
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("127.0.0.1:%d:localhost:%d", port, remotePort),
		host,
	}
	cmd := exec.Command("ssh", args...)

	t = &tunnel{
		host: host,
		port: port,
		cmd:  cmd,
	}
	m.tunnels[host] = t
	m.mu.Unlock()

	if err := cmd.Start(); err != nil {
		m.mu.Lock()
		delete(m.tunnels, host)
		m.mu.Unlock()
		return 0, fmt.Errorf("ssh -L %d -> %s: %w", port, host, err)
	}

	// Goroutine waits for ssh to exit; removes entry on unexpected exit.
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		// Only delete if it's still our tunnel (not replaced).
		if m.tunnels[host] == t {
			delete(m.tunnels, host)
		}
		m.mu.Unlock()
		if err != nil {
			fmt.Printf("ssh tunnel to %s (port %d) exited: %v\n", host, port, err)
		}
	}()

	return port, nil
}

// Shutdown kills all active SSH tunnels.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for host, t := range m.tunnels {
		if t.cmd.Process != nil {
			t.cmd.Process.Kill()
		}
		delete(m.tunnels, host)
	}
}

func (m *Manager) findFreePort() (int, error) {
	port := m.startPort
	for i := 0; i < 100; i++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		l, err := net.Listen("tcp", addr)
		if err == nil {
			l.Close()
			return port, nil
		}
		port++
	}
	return 0, fmt.Errorf("no free port found starting at %d", m.startPort)
}
