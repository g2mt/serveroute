//go:build !systemd

// Package platform is the platform-independent facade for service management.
// When built without -tags systemd, it provides no-op implementations.
package platform

// Manager is a no-op service manager — Start, Stop, and Shutdown do nothing.
// State always returns "active" so the proxy path is never blocked.
type Manager struct{}

// NewManager creates a stub Manager.
func NewManager() *Manager { return &Manager{} }

// Start is a no-op.
func (m *Manager) Start(name string, useUser bool) error { return nil }

// Stop is a no-op.
func (m *Manager) Stop(name string, useUser bool) error { return nil }

// State always returns "active".
func (m *Manager) State(name string, useUser bool) (string, error) { return "active", nil }

// Shutdown is a no-op.
func (m *Manager) Shutdown() {}

// Watcher is a no-op watcher that never emits events.
type Watcher struct {
	done chan struct{}
}

// NewWatcher creates a stub Watcher.
func NewWatcher() *Watcher { return &Watcher{done: make(chan struct{})} }

// Add is a no-op.
func (w *Watcher) Add(unitName string, useUser bool) error { return nil }

// Start is a no-op.
func (w *Watcher) Start() {}

// Subscribe returns a channel that never receives events and an unsubscribe
// function that closes the channel.
func (w *Watcher) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event)
	return ch, func() { close(ch) }
}

// Shutdown is a no-op.
func (w *Watcher) Shutdown() {}

// Event represents a systemd unit state change.
type Event struct {
	Service string `json:"service"`
	Active  bool   `json:"active"`
}

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
