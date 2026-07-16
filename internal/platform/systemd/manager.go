// Package systemd wraps systemctl for unit lifecycle operations.
package systemd

import (
	"fmt"
	"os/exec"
	"strings"
)

// knownSuffixes lists all valid systemd unit file suffixes.
var knownSuffixes = []string{
	"service", "timer", "socket", "mount", "automount",
	"path", "target", "device", "scope", "slice",
	"swap", "network", "link", "netdev",
}

// EnsureSuffix appends ".service" to name if it does not already carry a known
// systemd unit suffix (e.g. ".timer", ".socket", ".service", etc.).
func EnsureSuffix(name string) string {
	for _, s := range knownSuffixes {
		dotSuffix := "." + s
		if len(name) > len(dotSuffix) && name[len(name)-len(dotSuffix):] == dotSuffix {
			return name
		}
	}
	return name + ".service"
}

// Manager manages systemd units via the systemctl CLI.
type Manager struct{}

// NewManager creates a new Manager.
func NewManager() *Manager {
	return &Manager{}
}

// Start starts a systemd unit and waits for the start job to finish.
func (m *Manager) Start(name string, useUser bool) error {
	args := m.buildArgs(useUser, "start", name)
	_, err := systemctl(args...)
	return err
}

// Stop stops a systemd unit.
func (m *Manager) Stop(name string, useUser bool) error {
	args := m.buildArgs(useUser, "stop", name)
	_, err := systemctl(args...)
	return err
}

// State returns the ActiveState of a systemd unit.
func (m *Manager) State(name string, useUser bool) (string, error) {
	args := m.buildArgs(useUser, "show", "-p", "ActiveState", "--value", name)
	out, err := systemctl(args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// Shutdown is a no-op for the systemctl-based manager.
func (m *Manager) Shutdown() {}

func (m *Manager) buildArgs(useUser bool, args ...string) []string {
	if useUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

func systemctl(args ...string) (string, error) {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("systemctl %s: %s", strings.Join(args, " "),
				strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
