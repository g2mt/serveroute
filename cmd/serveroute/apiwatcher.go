// watcher.go — SSE endpoint for systemd unit state-change events.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// apiWatchSSE serves a Server-Sent Events stream of service state changes.
//
//	GET /:watch
//
// Each event is a JSON line of the form:
//
//	data: {"service":"mpd","active":true}
func (h *handler) apiWatchSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.watcher == nil {
		http.Error(w, "watcher not available", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := h.watcher.Subscribe()
	defer cancel()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
