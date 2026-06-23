// watcher.go — systemd unit state-change watcher with epoll-based event loop.
package systemd

/*
#cgo pkg-config: libsystemd
#include <systemd/sd-bus.h>
#include <stdlib.h>
#include <string.h>
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <time.h>
#include <unistd.h>

// strerror_r wrapper — returns a malloc'd string.
static char *errstr(int errnum) {
	char buf[256];
	strerror_r(-errnum, buf, sizeof(buf));
	return strdup(buf);
}

// do_subscribe subscribes to systemd manager signals on a bus.
static int do_subscribe(sd_bus *bus) {
	sd_bus_error error = SD_BUS_ERROR_NULL;
	int r = sd_bus_call_method(bus,
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"Subscribe",
		&error, NULL, NULL);
	if (r < 0) {
		int e = r;
		sd_bus_error_free(&error);
		return e;
	}
	sd_bus_error_free(&error);
	return 0;
}

// do_add_unit_match adds a D-Bus match rule for PropertiesChanged on a unit path.
static int do_add_unit_match(sd_bus *bus, const char *unit_path) {
	char match[1024];
	snprintf(match, sizeof(match),
		"type='signal',"
		"sender='org.freedesktop.systemd1',"
		"path='%s',"
		"interface='org.freedesktop.DBus.Properties',"
		"member='PropertiesChanged'",
		unit_path);
	return sd_bus_add_match(bus, NULL, match, NULL, NULL);
}

// do_encode_unit_path returns a malloc'd D-Bus object path for a unit name.
static char *do_encode_unit_path(const char *name) {
	char *path = NULL;
	int r = sd_bus_path_encode("/org/freedesktop/systemd1/unit", name, &path);
	if (r < 0) return NULL;
	return path;
}

// do_now_usec returns the current CLOCK_MONOTONIC time in microseconds.
static uint64_t do_now_usec(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (uint64_t)ts.tv_sec * 1000000ULL + (uint64_t)ts.tv_nsec / 1000ULL;
}

// do_process_event drains one pending bus message and, if it is a
// PropertiesChanged signal for ActiveState on org.freedesktop.systemd1.Unit,
// extracts the unit path and state string.
// Returns  2 with *path_out and *state_out set (caller must free both),
//          1 when a message was consumed but not relevant (caller should loop),
//          0 when no message is available (caller should retry later),
//         <0 on error.
static int do_process_event(sd_bus *bus, char **path_out, char **state_out) {
	sd_bus_message *m = NULL;
	int r = sd_bus_process(bus, &m);
	if (r <= 0) return r;
	if (m == NULL) return 0;

	if (!sd_bus_message_is_signal(m,
	    "org.freedesktop.DBus.Properties", "PropertiesChanged"))
		goto skip;

	const char *iface = NULL;
	r = sd_bus_message_read(m, "s", &iface);
	if (r < 0 || strcmp(iface, "org.freedesktop.systemd1.Unit") != 0)
		goto skip;

	r = sd_bus_message_enter_container(m, SD_BUS_TYPE_ARRAY, "{sv}");
	if (r < 0) goto skip;

	const char *prop_name;
	while ((r = sd_bus_message_enter_container(m,
	    SD_BUS_TYPE_DICT_ENTRY, "sv")) > 0) {
		r = sd_bus_message_read(m, "s", &prop_name);
		if (r < 0) break;
		if (strcmp(prop_name, "ActiveState") == 0) {
			const char *state = NULL;
			r = sd_bus_message_read(m, "v", "s", &state);
			if (r >= 0 && state != NULL) {
				*path_out = strdup(sd_bus_message_get_path(m));
				*state_out = strdup(state);
				sd_bus_message_exit_container(m);
				sd_bus_message_exit_container(m);
				sd_bus_message_unref(m);
				return 2;
			}
		} else {
			sd_bus_message_skip(m, "v");
		}
		sd_bus_message_exit_container(m);
	}
	sd_bus_message_exit_container(m);

skip:
	sd_bus_message_unref(m);
	return 1;
}
*/
import "C"

import (
	"fmt"
	"log"
	"sync"
	"unsafe"
)

// Event represents a systemd unit state change detected by the Watcher.
type Event struct {
	Service string `json:"service"`
	Active  bool   `json:"active"`
}

// Watcher watches systemd units for ActiveState changes via D-Bus signals
// using an epoll-based event loop for efficient fd monitoring.
type Watcher struct {
	userBus    *C.sd_bus
	systemBus  *C.sd_bus
	shutdownFd C.int // eventfd used to wake epoll_wait on shutdown

	events chan Event
	done   chan struct{}
	once   sync.Once

	mu          sync.Mutex
	pathToState map[string]*pathState // D-Bus path -> unit + last seen active state

	subsMu      sync.RWMutex
	subscribers map[chan Event]struct{}
}

type pathState struct {
	unit   string
	active bool
}

// NewWatcher creates a new Watcher. Call Add to register units, then Start.
func NewWatcher() *Watcher {
	fd := C.eventfd(0, C.EFD_CLOEXEC|C.EFD_NONBLOCK)
	return &Watcher{
		shutdownFd:  fd,
		events:      make(chan Event, 64),
		done:        make(chan struct{}),
		pathToState:  make(map[string]*pathState),
		subscribers: make(map[chan Event]struct{}),
	}
}

// Add registers a unit to watch. Must be called before Start.
func (w *Watcher) Add(unitName string, useUser bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cName := C.CString(unitName)
	defer C.free(unsafe.Pointer(cName))

	cPath := C.do_encode_unit_path(cName)
	if cPath == nil {
		return fmt.Errorf("encode path for %s failed", unitName)
	}
	path := C.GoString(cPath)
	w.pathToState[path] = &pathState{unit: unitName}

	// Open the appropriate bus lazily and add the match.
	if useUser {
		if w.userBus == nil {
			if err := w.initBus(true); err != nil {
				C.free(unsafe.Pointer(cPath))
				return err
			}
		}
		r := C.do_add_unit_match(w.userBus, cPath)
		C.free(unsafe.Pointer(cPath))
		if r < 0 {
			errStr := C.errstr(r)
			defer C.free(unsafe.Pointer(errStr))
			return fmt.Errorf("add match on user bus for %s: %s", unitName, C.GoString(errStr))
		}
	} else {
		if w.systemBus == nil {
			if err := w.initBus(false); err != nil {
				C.free(unsafe.Pointer(cPath))
				return err
			}
		}
		r := C.do_add_unit_match(w.systemBus, cPath)
		C.free(unsafe.Pointer(cPath))
		if r < 0 {
			errStr := C.errstr(r)
			defer C.free(unsafe.Pointer(errStr))
			return fmt.Errorf("add match on system bus for %s: %s", unitName, C.GoString(errStr))
		}
	}
	return nil
}

func (w *Watcher) initBus(useUser bool) error {
	var bus *C.sd_bus
	var r C.int
	if useUser {
		r = C.sd_bus_default_user(&bus)
	} else {
		r = C.sd_bus_open_system(&bus)
	}
	if r < 0 {
		errStr := C.errstr(r)
		defer C.free(unsafe.Pointer(errStr))
		return fmt.Errorf("open bus: %s", C.GoString(errStr))
	}

	r = C.do_subscribe(bus)
	if r < 0 {
		errStr := C.errstr(r)
		defer C.free(unsafe.Pointer(errStr))
		C.sd_bus_unref(bus)
		return fmt.Errorf("subscribe: %s", C.GoString(errStr))
	}

	if useUser {
		w.userBus = bus
	} else {
		w.systemBus = bus
	}
	return nil
}

// Start begins watching for events. Must be called after all units are added.
func (w *Watcher) Start() {
	go w.broadcast()
	if w.userBus != nil {
		go func() {
			if err := w.watchBus(w.userBus); err != nil {
				log.Printf("systemd watcher: user bus error: %v", err)
			}
		}()
	}
	if w.systemBus != nil {
		go func() {
			if err := w.watchBus(w.systemBus); err != nil {
				log.Printf("systemd watcher: system bus error: %v", err)
			}
		}()
	}
}

// Subscribe returns a channel that receives state change events and an
// unsubscribe function. The returned channel is closed on unsubscribe or
// watcher shutdown.
func (w *Watcher) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	w.subsMu.Lock()
	w.subscribers[ch] = struct{}{}
	w.subsMu.Unlock()
	return ch, func() {
		w.subsMu.Lock()
		delete(w.subscribers, ch)
		w.subsMu.Unlock()
		close(ch)
	}
}

// Shutdown stops the watcher and closes all subscriber channels.
func (w *Watcher) Shutdown() {
	w.once.Do(func() {
		// Wake epoll_wait via eventfd so watchBus goroutines exit promptly.
		var val C.uint64_t = 1
		C.write(w.shutdownFd, unsafe.Pointer(&val), C.sizeof_uint64_t)

		close(w.done)
		w.subsMu.Lock()
		for ch := range w.subscribers {
			close(ch)
		}
		w.subscribers = nil
		w.subsMu.Unlock()
	})
}

// watchBus runs an epoll-based event loop for a single sd-bus connection.
// Returns an error if the loop exits due to a failure; returns nil on clean shutdown.
func (w *Watcher) watchBus(bus *C.sd_bus) error {
	epollFd := C.epoll_create1(C.EPOLL_CLOEXEC)
	if epollFd < 0 {
		return fmt.Errorf("epoll_create1 failed")
	}
	defer C.close(epollFd)

	busFd := C.sd_bus_get_fd(bus)
	if busFd < 0 {
		errStr := C.errstr(busFd)
		defer C.free(unsafe.Pointer(errStr))
		return fmt.Errorf("sd_bus_get_fd: %s", C.GoString(errStr))
	}

	// Register the bus fd with its current event mask.
	var ev C.struct_epoll_event
	ev.events = C.uint32_t(C.sd_bus_get_events(bus))
	// epoll_data_t is a union; set the fd member for wake-up identification.
	*(*C.int)(unsafe.Pointer(&ev.data)) = busFd
	if C.epoll_ctl(epollFd, C.EPOLL_CTL_ADD, busFd, &ev) < 0 {
		return fmt.Errorf("epoll_ctl ADD bus fd failed")
	}

	// Register the shutdown eventfd so Shutdown can interrupt epoll_wait.
	var sdEv C.struct_epoll_event
	sdEv.events = C.EPOLLIN
	*(*C.int)(unsafe.Pointer(&sdEv.data)) = w.shutdownFd
	C.epoll_ctl(epollFd, C.EPOLL_CTL_ADD, w.shutdownFd, &sdEv)

	var events [2]C.struct_epoll_event

	for {
		// Determine epoll_wait timeout from systemd's internal timer.
		timeoutMs := C.int(-1)
		var timeoutUsec C.uint64_t
		if C.sd_bus_get_timeout(bus, &timeoutUsec) > 0 {
			now := C.do_now_usec()
			if uint64(timeoutUsec) > uint64(now) {
				timeoutMs = C.int((uint64(timeoutUsec) - uint64(now)) / 1000)
			} else {
				timeoutMs = 0
			}
		}

		nfds := C.epoll_wait(epollFd, &events[0], 2, timeoutMs)
		if nfds < 0 {
			// EINTR is harmless; loop again.
			continue
		}

		for i := C.int(0); i < nfds; i++ {
			fd := *(*C.int)(unsafe.Pointer(&events[i].data))
			if fd == w.shutdownFd {
				return nil
			}
		}

		// Drain all pending bus messages.
		for {
			var cPath, cState *C.char
			r := C.do_process_event(bus, &cPath, &cState)
			if r < 0 {
				errStr := C.errstr(r)
				err := fmt.Errorf("do_process_event: %s", C.GoString(errStr))
				C.free(unsafe.Pointer(errStr))
				return err
			}
			if r == 0 {
				break // no messages available
			}
			if r == 1 {
				continue // irrelevant message consumed, keep draining
			}

			path := C.GoString(cPath)
			state := C.GoString(cState)
			C.free(unsafe.Pointer(cPath))
			C.free(unsafe.Pointer(cState))

			w.mu.Lock()
			ps := w.pathToState[path]
			if ps == nil {
				w.mu.Unlock()
				continue
			}
			active := state == "active"
			if active == ps.active {
				w.mu.Unlock()
				continue // no state change, skip
			}
			ps.active = active
			unit := ps.unit
			w.mu.Unlock()

			select {
			case w.events <- Event{Service: unit, Active: active}:
			default:
			}
		}

		// Update epoll registration — systemd may have changed its event mask.
		ev.events = C.uint32_t(C.sd_bus_get_events(bus))
		C.epoll_ctl(epollFd, C.EPOLL_CTL_MOD, busFd, &ev)
	}
}

func (w *Watcher) broadcast() {
	for {
		select {
		case <-w.done:
			return
		case evt := <-w.events:
			w.subsMu.RLock()
			for ch := range w.subscribers {
				select {
				case ch <- evt:
				default:
				}
			}
			w.subsMu.RUnlock()
		}
	}
}

func (w *Watcher) cleanup() {
	if w.shutdownFd >= 0 {
		C.close(w.shutdownFd)
		w.shutdownFd = -1
	}
	if w.userBus != nil {
		C.sd_bus_unref(w.userBus)
		w.userBus = nil
	}
	if w.systemBus != nil {
		C.sd_bus_unref(w.systemBus)
		w.systemBus = nil
	}
}
