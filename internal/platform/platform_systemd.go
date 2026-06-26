//go:build systemd

// Package platform is the platform-independent facade for service management.
// When built with -tags systemd, it delegates to the CGo systemd sub-package.
package platform

import "serveroute/internal/platform/systemd"

// Manager wraps systemd.Manager.
type Manager = systemd.Manager

// Watcher wraps systemd.Watcher.
type Watcher = systemd.Watcher

// Event wraps systemd.Event.
type Event = systemd.Event

// NewManager delegates to systemd.NewManager.
var NewManager = systemd.NewManager

// NewWatcher delegates to systemd.NewWatcher.
var NewWatcher = systemd.NewWatcher

// EnsureSuffix delegates to systemd.EnsureSuffix.
var EnsureSuffix = systemd.EnsureSuffix
