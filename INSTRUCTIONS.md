# 2nnel — Coding Agent Prompt

You are building **2nnel** (pronounced "tunnel"), a self-hosted reverse tunnel that exposes local services to the internet through an outbound-only connection from the client. The goal is to replicate the premium features of Cloudflare Tunnel — HTTP tunneling, TCP/SSH forwarding, TLS termination, and multi-service multiplexing — as a free, self-hostable, single-binary solution.

## Project Context

- **Author:** 22or
- **Repo name:** `2nnel`
- **Language:** Go
- **Target users:** Developers and homelabbers who want to expose local services (web apps, SSH, game servers, APIs) without opening inbound firewall ports, paying for ngrok/Cloudflare premium, or trusting a third party with their traffic.
- **Key constraint:** The client must only make *outbound* connections. No port forwarding, no NAT traversal tricks. The relay server accepts public traffic and forwards it through a persistent control connection to the client.

## Architecture

```
[Internet User] --> [2nnel Relay Server (VPS)] <==outbound== [2nnel Client (behind NAT)]
                         |                                          |
                    public-facing ports                      local services
                    (443, 2222, etc.)                     (localhost:3000, sshd, etc.)
```

### Two binaries from one codebase

1. **`2nnel server`** — Runs on a VPS with a public IP. Accepts incoming public traffic (HTTP, TCP, SSH) and routes it to the correct connected client through the control channel.
2. **`2nnel client`** — Runs on the user's local machine or homelab. Connects outbound to the relay server, registers tunnels, and proxies traffic to local services.

### Control channel

- The client opens a single persistent connection to the server over WebSocket (WSS) on port 443.
- This connection is the control channel: it handles authentication, tunnel registration, and heartbeats.
- When the server receives public traffic for a registered tunnel, it signals the client over the control channel. The client opens a new WebSocket connection (a "data channel") to carry that proxied stream.
- All connections are outbound from the client. The server never initiates connections to the client's network.

### Multiplexing

- A single client can register multiple tunnels (e.g., `web.example.com` → `localhost:3000`, `ssh.example.com:2222` → `localhost:22`).
- Multiple clients can connect to one server simultaneously, each with their own tunnels.
- Use `yamux` or a similar stream multiplexer over the WebSocket to avoid opening a new WS connection per proxied stream. This is critical for performance.

## Features to Implement (in priority order)

### Phase 1 — HTTP tunnel (core MVP)

Build the simplest working tunnel: expose a local HTTP service via a public subdomain.

- [ ] **Server CLI:** `2nnel server --domain example.com --port 443`
  - Listens for client control connections on WSS
  - Listens for public HTTP(S) traffic
  - Routes requests by `Host` header to the correct client tunnel
- [ ] **Client CLI:** `2nnel client --server wss://example.com --tunnel web:localhost:3000`
  - Connects to server, registers tunnel named `web`
  - Proxies HTTP traffic bidirectionally
- [ ] **TLS termination on the server** using autocert (Let's Encrypt via `golang.org/x/crypto/acme/autocert`) for `*.example.com`
  - Wildcard cert or per-subdomain cert
  - Client-to-server control channel is always TLS
- [ ] **Reconnect logic:** Client auto-reconnects with exponential backoff if the control channel drops
- [ ] **Health check / heartbeat:** Periodic ping/pong over control channel; server evicts stale clients

### Phase 2 — TCP/SSH forwarding

Expose raw TCP services (especially SSH) on arbitrary server ports.

- [ ] **TCP tunnel type:** `2nnel client --server wss://example.com --tunnel ssh:localhost:22:tcp:2222`
  - Server listens on public port 2222, forwards raw TCP bytes to client's localhost:22
  - No HTTP semantics — pure byte stream proxying
- [ ] **SSH works transparently:** A user can `ssh -p 2222 user@example.com` and reach the client's sshd
- [ ] **Port allocation:** Server validates requested ports, rejects conflicts, configurable allowed port range

### Phase 3 — Config file and multi-tunnel

- [ ] **YAML config file** as alternative to CLI flags:
  ```yaml
  server: wss://relay.example.com
  auth_token: "secret"
  tunnels:
    - name: web
      local: localhost:3000
      type: http
      subdomain: myapp
    - name: ssh
      local: localhost:22
      type: tcp
      remote_port: 2222
    - name: minecraft
      local: localhost:25565
      type: tcp
      remote_port: 25565
  ```
- [ ] **Auth tokens:** Server issues tokens, client authenticates on connect. Simple shared-secret for the prototype; no need for a full auth system yet.
- [ ] **Simultaneous multi-client:** Server handles N clients with different tunnel registrations without conflict

### Phase 4 — Observability and polish

- [ ] **Structured logging** with `slog` — connection events, bytes transferred, errors
- [ ] **Metrics endpoint** on server: active tunnels, connected clients, bytes proxied, uptime
- [ ] **Graceful shutdown:** Drain active connections on SIGTERM
- [ ] **Rate limiting:** Per-tunnel connection rate limit to prevent abuse
- [ ] **README** with architecture diagram, quickstart, and deployment instructions for the relay server (Dockerfile + systemd unit file)

## Technical Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Language | Go | Excellent stdlib for networking, easy cross-compilation, single binary output |
| Control channel | WebSocket (gorilla/websocket or nhooyr.io/websocket) | Works through corporate proxies and firewalls, upgradeable from HTTPS |
| Multiplexing | `hashicorp/yamux` | Battle-tested stream multiplexer, avoids per-stream WS overhead |
| TLS | `autocert` (Let's Encrypt) | Zero-config TLS for the server |
| CLI framework | `cobra` | Standard for Go CLIs |
| Config | `viper` + YAML | Familiar, supports both file and flags |
| Build | `goreleaser` | Cross-platform binary releases |

## Code Organization

```
2nnel/
├── cmd/
│   ├── server.go          # cobra command for `2nnel server`
│   └── client.go          # cobra command for `2nnel client`
├── internal/
│   ├── server/
│   │   ├── server.go      # main server struct, lifecycle
│   │   ├── control.go     # control channel handler (client registration, heartbeat)
│   │   ├── http_proxy.go  # HTTP reverse proxy, Host-header routing
│   │   ├── tcp_proxy.go   # TCP listener + forwarding
│   │   └── tls.go         # autocert setup
│   ├── client/
│   │   ├── client.go      # main client struct, lifecycle
│   │   ├── control.go     # control channel (connect, register, heartbeat)
│   │   └── proxy.go       # local service proxying
│   ├── proto/
│   │   ├── messages.go    # control message types (JSON or msgpack)
│   │   └── mux.go         # yamux session wrapper
│   └── config/
│       └── config.go      # YAML config parsing
├── main.go                # root cobra command
├── go.mod
├── Dockerfile
└── README.md
```

## Control Protocol Messages

Define these as JSON over the control channel:

```go
// Client -> Server
RegisterTunnel { Name, Type("http"|"tcp"), Subdomain, RemotePort, LocalAddr }
Heartbeat { Timestamp }

// Server -> Client
TunnelRegistered { Name, PublicURL }
TunnelError { Name, Error }
OpenStream { StreamID, TunnelName }  // "I have traffic for you, open a data stream"
Heartbeat { Timestamp }
```

When the server sends `OpenStream`, the client opens a new yamux stream on the existing connection and begins proxying bytes between that stream and the local service.

## What "Done" Looks Like for the Prototype

The prototype is done when you can:

1. Run `2nnel server` on a $5 VPS
2. Run `2nnel client` on your laptop behind home WiFi
3. Visit `https://myapp.yourdomain.com` in a browser and see your local dev server
4. Run `ssh -p 2222 user@yourdomain.com` and land in a shell on your laptop
5. Kill the client, restart it, and have tunnels auto-reconnect within seconds
6. Show a friend the README and have them set up their own tunnel in under 5 minutes

## What NOT to Build (Yet)

- No web dashboard or admin UI
- No user accounts or multi-tenancy beyond auth tokens
- No load balancing or failover
- No UDP support
- No browser-based terminal
- No custom domain routing beyond subdomains
- No plugin system

Keep it lean. A working tunnel with good code is the goal, not a product.
