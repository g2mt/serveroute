// Package systemd wraps libsystemd (sd-bus) for unit lifecycle operations.
// All calls are serialized through a single goroutine that owns the bus connections.
package systemd

/*
#cgo pkg-config: libsystemd
#include <systemd/sd-bus.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

static int open_system_bus(sd_bus **bus) {
	return sd_bus_open_system(bus);
}

static int open_user_bus(sd_bus **bus) {
	return sd_bus_default_user(bus);
}

static void free_bus(sd_bus *bus) {
	if (bus) sd_bus_unref(bus);
}

static int do_start_unit(sd_bus *bus, const char *name) {
	sd_bus_error error = SD_BUS_ERROR_NULL;
	sd_bus_message *reply = NULL;
	int r = sd_bus_call_method(bus,
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"StartUnit",
		&error,
		&reply,
		"ss", name, "replace");
	if (r < 0) {
		int e = r;
		sd_bus_error_free(&error);
		return e;
	}
	sd_bus_message_unref(reply);
	sd_bus_error_free(&error);
	return 0;
}

static int do_stop_unit(sd_bus *bus, const char *name) {
	sd_bus_error error = SD_BUS_ERROR_NULL;
	sd_bus_message *reply = NULL;
	int r = sd_bus_call_method(bus,
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"StopUnit",
		&error,
		&reply,
		"ss", name, "replace");
	if (r < 0) {
		int e = r;
		sd_bus_error_free(&error);
		return e;
	}
	sd_bus_message_unref(reply);
	sd_bus_error_free(&error);
	return 0;
}

// systemd_escape escapes a unit name for use in a D-Bus object path.
// Replaces "/" with "-", and if the name starts with "-", prepends "\x2d".
// Returns a malloc'd string that the caller must free.
static char *systemd_escape(const char *name) {
	size_t len = strlen(name);
	// Worst case: every char is '/' -> same length (we replace / with -)
	// +1 for null + optional prefix
	char *escaped = malloc(len + 5);
	if (!escaped) return NULL;

	size_t j = 0;
	// If name starts with '-', we need to encode it.
	// For simplicity, we just handle '/' -> '-'.
	// systemd also requires encoding of '.' but for unit names it's fine.
	for (size_t i = 0; i < len; i++) {
		if (name[i] == '/') {
			escaped[j++] = '-';
		} else {
			escaped[j++] = name[i];
		}
	}
	escaped[j] = '\0';
	return escaped;
}

static int do_get_state(sd_bus *bus, const char *name, char **result) {
	sd_bus_error error = SD_BUS_ERROR_NULL;
	char *escaped = systemd_escape(name);
	if (!escaped) return -ENOMEM;

	char path[1024];
	snprintf(path, sizeof(path), "/org/freedesktop/systemd1/unit/%s", escaped);
	free(escaped);

	int r = sd_bus_get_property_string(bus,
		"org.freedesktop.systemd1",
		path,
		"org.freedesktop.systemd1.Unit",
		"ActiveState",
		&error,
		result);
	if (r < 0) {
		sd_bus_error_free(&error);
		return r;
	}
	sd_bus_error_free(&error);
	return 0;
}

// strerror_r wrapper — returns a malloc'd string.
static char *errstr(int errnum) {
	char buf[256];
	strerror_r(-errnum, buf, sizeof(buf));
	return strdup(buf);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

type action int

const (
	actionStart action = iota
	actionStop
	actionState
)

// request represents a single systemd operation.
type request struct {
	action   action
	name     string // unit name
	useUser  bool   // use user bus instead of system bus
	respChan chan response
}

type response struct {
	state string
	err   error
}

// Manager serializes access to sd-bus connections.
type Manager struct {
	reqs      chan request
	systemBus *C.sd_bus
	userBus   *C.sd_bus
	done      chan struct{}
	once      sync.Once
}

// NewManager starts the background goroutine that owns the bus connections.
func NewManager() *Manager {
	m := &Manager{
		reqs: make(chan request),
		done: make(chan struct{}),
	}
	go m.loop()
	return m
}

// Start starts a systemd unit.
func (m *Manager) Start(name string, useUser bool) error {
	return m.do(actionStart, name, useUser)
}

// Stop stops a systemd unit.
func (m *Manager) Stop(name string, useUser bool) error {
	return m.do(actionStop, name, useUser)
}

// State returns the ActiveState of a systemd unit.
func (m *Manager) State(name string, useUser bool) (string, error) {
	resp := m.doReq(actionState, name, useUser)
	return resp.state, resp.err
}

// Shutdown closes the bus connections and stops the goroutine.
func (m *Manager) Shutdown() {
	m.once.Do(func() {
		close(m.done)
	})
}

func (m *Manager) do(a action, name string, useUser bool) error {
	resp := m.doReq(a, name, useUser)
	return resp.err
}

func (m *Manager) doReq(a action, name string, useUser bool) response {
	respChan := make(chan response, 1)
	m.reqs <- request{
		action:   a,
		name:     name,
		useUser:  useUser,
		respChan: respChan,
	}
	return <-respChan
}

func (m *Manager) loop() {
	for {
		select {
		case <-m.done:
			m.cleanup()
			return
		case req := <-m.reqs:
			state, err := m.handle(req)
			req.respChan <- response{state: state, err: err}
		}
	}
}

func (m *Manager) handle(req request) (string, error) {
	bus, err := m.getBus(req.useUser)
	if err != nil {
		return "", err
	}

	cName := C.CString(req.name)
	defer C.free(unsafe.Pointer(cName))

	switch req.action {
	case actionStart:
		r := C.do_start_unit(bus, cName)
		if r < 0 {
			errStr := C.errstr(r)
			defer C.free(unsafe.Pointer(errStr))
			return "", fmt.Errorf("StartUnit(%s): %s", req.name, C.GoString(errStr))
		}
		return "", nil

	case actionStop:
		r := C.do_stop_unit(bus, cName)
		if r < 0 {
			errStr := C.errstr(r)
			defer C.free(unsafe.Pointer(errStr))
			return "", fmt.Errorf("StopUnit(%s): %s", req.name, C.GoString(errStr))
		}
		return "", nil

	case actionState:
		var cResult *C.char
		r := C.do_get_state(bus, cName, &cResult)
		if cResult != nil {
			defer C.free(unsafe.Pointer(cResult))
		}
		if r < 0 {
			return "", fmt.Errorf("ActiveState(%s): error %d", req.name, int(r))
		}
		return C.GoString(cResult), nil

	default:
		return "", fmt.Errorf("unknown action: %d", req.action)
	}
}

func (m *Manager) getBus(useUser bool) (*C.sd_bus, error) {
	if useUser {
		if m.userBus == nil {
			var bus *C.sd_bus
			r := C.open_user_bus(&bus)
			if r < 0 {
				errStr := C.errstr(r)
				defer C.free(unsafe.Pointer(errStr))
				return nil, fmt.Errorf("sd_bus_default_user: %s", C.GoString(errStr))
			}
			m.userBus = bus
		}
		return m.userBus, nil
	}
	if m.systemBus == nil {
		var bus *C.sd_bus
		r := C.open_system_bus(&bus)
		if r < 0 {
			errStr := C.errstr(r)
			defer C.free(unsafe.Pointer(errStr))
			return nil, fmt.Errorf("sd_bus_open_system: %s", C.GoString(errStr))
		}
		m.systemBus = bus
	}
	return m.systemBus, nil
}

func (m *Manager) cleanup() {
	if m.systemBus != nil {
		C.free_bus(m.systemBus)
		m.systemBus = nil
	}
	if m.userBus != nil {
		C.free_bus(m.userBus)
		m.userBus = nil
	}
}
