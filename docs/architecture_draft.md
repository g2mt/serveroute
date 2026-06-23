# serveroute — Architecture Draft

serveroute is a minimal HTTP(S) host router and service manager. It listens on
one set of sockets, matches the `Host` header against a domain template, and
either serves locally (static files / built-in API / a proxied systemd unit) or
forwards to another host over an on-demand SSH tunnel.

Dependencies: **Go standard library only**, plus a thin cgo wrapper around
**libsystemd** (`sd-bus`) for unit start/stop/state. No third-party Go modules.

## The four components

There are exactly four pieces.

```
 +-----------------------+        +------------------+
 |  http.Server          |        |  systemd (cgo)   |
 |  one Handler does:    |        |  Start/Stop/State|
 |   allow-list check    |        |  + idle reaper   |
 |   Host -> route       |        +--------+---------+
 |   static / api / proxy|                 ^
 |   remote -> ssh tunnel|                 |
 +----------+------------+                |
            |                             |
            v                             |
 +------------------+   ssh -L :port:host:http   +-------+
 | sshTunnels       | <------------------------> | remote|
 | map[host]*tunnel |                            +-------+
 +------------------+
```

### 1. `http.Server` + single `Handler`

One `http.ServeMux`-equivalent, one handler function. It does, in order:

1. **allow-list**: compare `r.RemoteAddr`'s IP against `allow` (a `[]netip.Prefix`).
   Reject → `403`. No separate listener wrapper.
2. **parse Host**: strip port, match against `domain_template` to split
   `subdomain` and `host`. One regex, compiled once.
3. **dispatch** (one `switch`):
   - `host` is remote (`!= os.Hostname()`) → call `sshTunnels.Get(host)` and
     `httputil.ReverseProxy` to its local port, preserving `Host`.
     The same `config.yaml` is deployed everywhere, so the remote HTTP port is
     known locally from the shared config.
   - `host` is local → look up `services[subdomain]`:
     - `serve_files` → `http.FileServer`.
     - `api: true` → inline API handlers (a few routes on this same handler).
       These API handlers are also served when the request reached this host
       through an SSH tunnel — the remote host's handler processes it the same way.
     - `service` → ensure systemd unit active (call `systemd.Start` if needed),
       update last-seen, `httputil.ReverseProxy` to
       `127.0.0.1:<start_port + service index>`.

All routing is a function of the parsed `config.yaml` baked into a lookup table
at startup. There is no router *type*, just data + a switch.

### 2. `systemd` (cgo, libsystemd)

Owns the single sd-bus connection (one dedicated goroutine; request/reply via a
channel to keep cgo boundary simple). Exposes:

- `Start(name)`, `Stop(name)`, `State(name)`.
- Selects system bus vs `--user` bus per service's `user` flag.
- **Owns idle reaping**: a `time.Ticker` goroutine inside this component walks
  each systemd service's last-seen timestamp and calls `Stop` when
  `now - last_seen >= stops_after` and `stoppable`. Last-seen is an
  `atomic.Int64` per service; the handler updates it on every proxied request.

No IdleTracker component, no PortAllocator component — last-seen is a field on
the service record; ports are `start_port + index` computed inline.

### 3. `sshTunnels` (a `map[host]*tunnel` + `sync.Mutex`)

The entire remote-host subsystem. `Get(host)` lazily spawns, if not present:

```
ssh -L 127.0.0.1:<port>:localhost:<remote_http_port> <host>
```

`<remote_http_port>` is known because the same `config.yaml` is deployed on
every host; the local instance reads the remote host's HTTP listener port
from its own copy of the config.

`<port>` is the next free port at/after `start_port` (probed with
`net.Listen`). A goroutine waits on the `ssh` process and, on unexpected exit,
removes the entry so the next `Get` re-establishes it. Nothing else.

### 4. `config` (load + validate)

The same `config.yaml` is deployed on every host. Each instance parses it
into structs, computes per-service `start_port + index`, pre-resolves the
local hostname via `os.Hostname()`, compiles the one domain-template regex.
Because the config is uniform, the remote HTTP port for each host is known
locally without out-of-band coordination. Pure data, no behavior.

## Concurrency model

- `net/http` gives one goroutine per connection; the handler is safe because it
  only reads immutable config + calls two guarded subsystems.
- `sshTunnels`: `sync.Mutex` around the map; each tunnel's process handle is
  immutable after creation.
- `systemd`: single goroutine owns the sd-bus connection; callers send a request
  on a channel and block on the reply. Per-service last-seen is `atomic.Int64`.
- No other shared mutable state.

## TLS

TLS certificates are loaded once at startup. Reload is restart-only: there is
no `SIGHUP` handler. To rotate certs, restart the process.

## Shutdown

`SIGINT`/`SIGTERM` → `server.Shutdown(ctx)` → kill any `ssh` child processes →
exit. Systemd units are left in their current state (no forced stop).

## Package layout

```
cmd/serveroute/main.go     // load config, build tables, run http.Server
internal/config            // parse + validate config.yaml
internal/systemd           // cgo libsystemd: start/stop/state + reaper
internal/sshtunnels         // map[host]*tunnel, lazy ssh -L, restart
```

Four files of real logic. The handler lives in `main.go`.

