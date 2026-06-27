// api.go — REST API endpoints for service status and control.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"serveroute/internal/config"
)

// serveAPI handles API requests for a host.
// Path routing:
//
//	GET  /              → list all services
//	GET  /:watch        → SSE stream of state changes
//	GET  /{svc}         → get service info
//	GET  /{svc}/active  → get activation state (boolean)
//	POST /{svc}/active  → set activation state (start/stop)
func (h *handler) serveAPI(w http.ResponseWriter, r *http.Request, host string) {
	parts := strings.SplitN(strings.Trim(r.URL.Path, "/"), "/", 3)
	if len(parts) == 1 && parts[0] == "" {
		parts = nil // root path → nil slice
	}

	svcs := h.compiled.ServiceIndex[host]

	switch {
	case len(parts) == 0:
		// GET /
		h.apiListAll(w, r, svcs)
	case len(parts) == 1 && parts[0] == ":watch":
		// GET /:watch → SSE stream
		h.apiWatchSSE(w, r)
	case len(parts) == 1:
		// GET /{svc}
		svc, ok := svcs[parts[0]]
		if !ok || svc.Hidden {
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
		h.apiGetOne(w, r, svc)
	case len(parts) >= 2 && parts[1] == "active":
		// GET/POST /{svc}/active
		svc, ok := svcs[parts[0]]
		if !ok || svc.Hidden {
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
		h.apiActive(w, r, svc)
	default:
		http.Error(w, "404 Not Found", http.StatusNotFound)
	}
}

type svcInfo struct {
	Active    bool `json:"active"`
	Stoppable bool `json:"stoppable"`
}

// svcInfo builds an svcInfo for a single service config.
func (h *handler) svcInfo(svc *config.ServiceConfig) svcInfo {
	info := svcInfo{Stoppable: *svc.Stoppable}
	if svc.Unit != "" {
		if state, err := h.platformMgr.State(svc.Unit, *svc.User); err == nil {
			info.Active = state == "active"
		}
	}
	return info
}

// apiListAll handles GET / — lists every non-hidden service on the host.
func (h *handler) apiListAll(w http.ResponseWriter, r *http.Request, svcs map[string]*config.ServiceConfig) {
	if r.Method != http.MethodGet {
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	result := make(map[string]svcInfo)
	for name, svc := range svcs {
		if svc.Hidden {
			continue
		}
		result[name] = h.svcInfo(svc)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}

// apiGetOne handles GET /{svc} — returns info for a single service.
func (h *handler) apiGetOne(w http.ResponseWriter, r *http.Request, svc *config.ServiceConfig) {
	if r.Method != http.MethodGet {
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	info := h.svcInfo(svc)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(info)
}

// apiActive handles GET/POST /{svc}/active.
func (h *handler) apiActive(w http.ResponseWriter, r *http.Request, svc *config.ServiceConfig) {
	if svc.Unit == "" {
		http.Error(w, "service has no systemd unit", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		state, err := h.platformMgr.State(svc.Unit, *svc.User)
		if err != nil {
			log.Printf("state %s: %v", svc.Unit, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"active": state == "active"})

	case http.MethodPost:
		var body struct {
			Active bool `json:"active"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		if body.Active {
			if err := h.platformMgr.Start(svc.Unit, *svc.User); err != nil {
				log.Printf("start %s: %v", svc.Unit, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			if !*svc.Stoppable {
				http.Error(w, "service is not stoppable", http.StatusBadRequest)
				return
			}
			if err := h.platformMgr.Stop(svc.Unit, *svc.User); err != nil {
				log.Printf("stop %s: %v", svc.Unit, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// Return the resulting state.
		state, err := h.platformMgr.State(svc.Unit, *svc.User)
		active := body.Active
		if err == nil {
			active = state == "active"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"active": active})

	default:
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}
