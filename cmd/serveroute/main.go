// serveroute — minimal HTTP(S) host router and service manager.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"serveroute/internal/config"
	"serveroute/internal/platform"
	"serveroute/internal/sshtunnels"
)

// serviceState tracks runtime state for a platform-backed service.
type serviceState struct {
	cfg      *config.ServiceConfig
	lastSeen atomic.Int64           // unix timestamp of last proxied request
	proxy    *httputil.ReverseProxy // nil if no forwards_to
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	// Load and compile config.
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	compiled, err := config.Compile(cfg)
	if err != nil {
		log.Fatalf("compile config: %v", err)
	}

	log.Printf("local host: %s", compiled.LocalHost)
	log.Printf("domain regex: %s", compiled.HostRegex)

	// Build service state tracking.
	states := buildStates(compiled)

	// Subsystems.
	platformMgr := platform.NewManager()
	tunnelMgr := sshtunnels.NewManager(cfg.StartPort)

	// Build watcher for SSE state-change streaming (local host only).
	watcher := platform.NewWatcher()
	if localSvcs, ok := compiled.ServiceIndex[compiled.LocalHost]; ok {
		for _, svc := range localSvcs {
			if svc.Unit != "" {
				if err := watcher.Add(svc.Unit, *svc.User); err != nil {
					log.Printf("watcher: add %s: %v", svc.Unit, err)
				}
			}
		}
	}
	watcher.Start()

	// Build the handler.
	handler := &handler{
		compiled:    compiled,
		states:      states,
		platformMgr: platformMgr,
		tunnelMgr:   tunnelMgr,
		watcher:     watcher,
	}

	// Start idle reaper.
	go idleReaper(states, platformMgr)

	// Set up HTTP server.
	httpServer := &http.Server{
		Addr:    cfg.Listen.HTTP,
		Handler: handler,
	}
	log.Printf("listening HTTP on %s", cfg.Listen.HTTP)

	// Start HTTPS if configured.
	var httpsServer *http.Server
	if cfg.Listen.HTTPS != "" {
		httpsServer = &http.Server{
			Addr:    cfg.Listen.HTTPS,
			Handler: handler,
		}
		log.Printf("listening HTTPS on %s", cfg.Listen.HTTPS)
	}

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 2)

	go func() {
		if httpsServer != nil {
			errCh <- httpsServer.ListenAndServeTLS(cfg.SSLCertificate, cfg.SSLCertificateKey)
		}
	}()
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case sig := <-stop:
		log.Printf("received %v, shutting down…", sig)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	httpServer.Shutdown(ctx)
	if httpsServer != nil {
		httpsServer.Shutdown(ctx)
	}

	watcher.Shutdown()
	tunnelMgr.Shutdown()
	platformMgr.Shutdown()

	log.Println("shutdown complete")
}

// buildStates creates serviceState entries for every platform-backed service.
func buildStates(compiled *config.Compiled) map[string]map[string]*serviceState {
	states := make(map[string]map[string]*serviceState)
	for host, svcs := range compiled.ServiceIndex {
		m := make(map[string]*serviceState)
		for sub, svc := range svcs {
			if svc.Unit != "" {
				ss := &serviceState{cfg: svc}
				ss.lastSeen.Store(time.Now().Unix())
				if svc.ForwardsTo != "" {
					target, err := url.Parse(svc.ForwardsTo)
					if err != nil {
						log.Printf("invalid forwards_to for %s/%s: %v", host, sub, err)
					} else {
						ss.proxy = &httputil.ReverseProxy{
							Rewrite: func(pr *httputil.ProxyRequest) {
								// Preserve original path/query so WebSocket
								// upgrades and routed endpoints work.
								pr.Out.URL.Scheme = target.Scheme
								pr.Out.URL.Host = target.Host
								setProxyHeaders(pr.Out, pr.In)
							},
						}
					}
				}
				m[sub] = ss
			}
		}
		if len(m) > 0 {
			states[host] = m
		}
	}
	return states
}

// handler is the single http.Handler for all requests.
type handler struct {
	compiled    *config.Compiled
	states      map[string]map[string]*serviceState
	tunnelMgr   *sshtunnels.Manager
	platformMgr *platform.Manager
	watcher     *platform.Watcher
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Allow-list check.
	if !h.checkAllow(r) {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// 2. Parse Host header.
	host, subdomain, ok := h.matchHost(r.Host)
	if !ok {
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}

	// 3. Dispatch.
	if host != h.compiled.LocalHost {
		// Remote host — tunnel + proxy.
		h.handleRemote(w, r, host, subdomain)
		return
	}

	// Local host.
	h.handleLocal(w, r, host, subdomain)
}

func (h *handler) checkAllow(r *http.Request) bool {
	if len(h.compiled.AllowNets) == 0 {
		return true // no allow list means allow all
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, prefix := range h.compiled.AllowNets {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *handler) matchHost(hostHeader string) (host string, subdomain string, ok bool) {
	// Strip port.
	hostOnly, _, err := net.SplitHostPort(hostHeader)
	if err != nil {
		hostOnly = hostHeader
	}
	hostOnly = strings.ToLower(hostOnly)

	matches := h.compiled.HostRegex.FindStringSubmatch(hostOnly)
	if matches == nil {
		return "", "", false
	}

	subIdx := h.compiled.HostRegex.SubexpIndex("subdomain")
	hostIdx := h.compiled.HostRegex.SubexpIndex("host")

	if subIdx >= 0 {
		subdomain = matches[subIdx]
	}
	if hostIdx >= 0 {
		host = matches[hostIdx]
	}
	return host, subdomain, true
}

func (h *handler) handleRemote(w http.ResponseWriter, r *http.Request, remoteHost, subdomain string) {
	tunnelHost := remoteHost
	if hostCfg, ok := h.compiled.Hosts[remoteHost]; ok && hostCfg.SSHHost != "" {
		tunnelHost = hostCfg.SSHHost
	}

	port, err := h.tunnelMgr.Get(tunnelHost)
	if err != nil {
		log.Printf("ssh tunnel to %s: %v", tunnelHost, err)
		http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
		return
	}

	// Build a reverse proxy to the local tunnel endpoint.
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Preserve original path/query for WebSocket upgrades.
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			setProxyHeaders(pr.Out, pr.In)
		},
	}
	rp.ServeHTTP(w, r)
}

func (h *handler) handleLocal(w http.ResponseWriter, r *http.Request, host, subdomain string) {
	svcs, ok := h.compiled.ServiceIndex[host]
	if !ok {
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}

	svc, ok := svcs[subdomain]
	if !ok {
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}

	if svc.AllowOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", svc.AllowOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}

	if r.Method == http.MethodOptions && svc.AllowOrigin != "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch {
	case svc.ServeFiles != "":
		fileServer := http.FileServer(http.Dir(svc.ServeFiles))
		fileServer.ServeHTTP(w, r)

	case svc.API:
		h.serveAPI(w, r, host)

	case svc.Unit != "":
		// Ensure platform unit is active.
		state, err := h.platformMgr.State(svc.Unit, *svc.User)
		if err != nil {
			log.Printf("state %s: %v", svc.Unit, err)
			http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
			return
		}
		if state != "active" && state != "activating" {
			if err := h.platformMgr.Start(svc.Unit, *svc.User); err != nil {
				log.Printf("start %s: %v", svc.Unit, err)
				http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
				return
			}
		}

		// Update last-seen timestamp and reverse proxy.
		ss := h.lookupState(host, subdomain)
		if ss == nil || ss.proxy == nil {
			http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
			return
		}
		ss.lastSeen.Store(time.Now().Unix())
		ss.proxy.ServeHTTP(w, r)

	default:
		http.Error(w, "404 Not Found", http.StatusNotFound)
	}
}

func (h *handler) lookupState(host, subdomain string) *serviceState {
	if hm, ok := h.states[host]; ok {
		return hm[subdomain]
	}
	return nil
}

// idleReaper periodically checks and stops idle platform services.
func idleReaper(states map[string]map[string]*serviceState, mgr *platform.Manager) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now().Unix()
		for _, svcMap := range states {
			for _, ss := range svcMap {
				if !*ss.cfg.Stoppable {
					continue
				}
				stopsAfter := ss.cfg.StopsAfterDuration()
				if stopsAfter <= 0 {
					continue
				}
				lastSeen := ss.lastSeen.Load()
				if now-lastSeen >= int64(stopsAfter.Seconds()) {
					state, err := mgr.State(ss.cfg.Unit, *ss.cfg.User)
					if err != nil {
						continue
					}
					if state == "active" || state == "activating" {
						log.Printf("idle reaper: stopping %s (last seen %ds ago)", ss.cfg.Unit, now-lastSeen)
						mgr.Stop(ss.cfg.Unit, *ss.cfg.User)
					}
				}
			}
		}
	}
}

// setProxyHeaders adds standard nginx-style proxy headers to the outgoing
// request.  inReq is the original client request (used for real client IP
// and TLS detection); outReq is the request being sent to the backend.
func setProxyHeaders(outReq, inReq *http.Request) {
	// Host → localhost (mimics proxy_set_header Host localhost).
	outReq.Host = "localhost"

	// X-Real-IP → client address.
	clientIP, _, err := net.SplitHostPort(inReq.RemoteAddr)
	if err != nil {
		clientIP = inReq.RemoteAddr
	}
	outReq.Header.Set("X-Real-IP", clientIP)

	// X-Forwarded-Proto → original scheme.
	if inReq.TLS != nil {
		outReq.Header.Set("X-Forwarded-Proto", "https")
	} else {
		outReq.Header.Set("X-Forwarded-Proto", "http")
	}
	// X-Forwarded-For is already handled by httputil.ReverseProxy.
}
