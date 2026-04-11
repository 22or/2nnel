# 2nnel

A self-hosted reverse tunnel that exposes local services to the internet through outbound-only connections. Free alternative to ngrok / Cloudflare Tunnel. Single binary, no agents, no accounts.

```
[Browser] ──► [2nnel server on VPS] ◄══outbound══ [2nnel client behind NAT]
                   public :443                           localhost:3000
```

## Features

- **HTTP tunnels** — expose local web apps via subdomain (`myapp.yourdomain.com`)
- **TCP/SSH tunnels** — expose raw TCP services (SSH, game servers, databases)
- **Multiplexed** — one outbound WebSocket connection carries all tunnels via yamux
- **TLS termination** — automatic Let's Encrypt certs via autocert
- **Auto-reconnect** — client reconnects with exponential backoff
- **Multi-client** — multiple clients, each with independent tunnel sets
- **YAML config** — full config file support alongside CLI flags

## Quickstart

### 1. Server (VPS)

```bash
# Build
go build -o 2nnel .

# Run (autocert — needs port 443 open and a real domain pointing at your VPS)
sudo ./2nnel server --domain example.com --auth-token supersecret

# Dev mode (no TLS, port 8080)
./2nnel server --dev --auth-token supersecret
```

### 2. Client (local machine)

```bash
# Expose localhost:3000 as https://myapp.example.com
./2nnel client \
    --server wss://example.com \
    --auth-token supersecret \
    --tunnel myapp:localhost:3000

# Also expose SSH on port 2222
./2nnel client \
    --server wss://example.com \
    --auth-token supersecret \
    --tunnel myapp:localhost:3000 \
    --tunnel ssh:localhost:22:tcp:2222
```

### 3. YAML config file

```yaml
server: wss://example.com
auth_token: supersecret
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

```bash
./2nnel client -c config.yaml
```

## Architecture

```
Client                              Server
──────                              ──────
WebSocket (outbound) ──────────────► /ws handler
yamux.Client                         yamux.Server
  │
  ├─ stream 1 (control)             Accept stream 1
  │    JSON messages                 ├─ authenticate client
  │    ← auth                        ├─ register tunnels
  │    → auth_ack                    └─ heartbeat loop
  │    → register_tunnel
  │    ← tunnel_registered
  │    ← heartbeat / → heartbeat
  │
  └─ Accept loop                    Open new stream per connection
       read StreamHeader             write StreamHeader
       dial local service            forward bytes from public traffic
       pipe bytes ↔
```

**HTTP requests:**
1. Browser hits `myapp.example.com`
2. Server matches Host header → finds client yamux session
3. Server opens new yamux stream, writes `StreamHeader{tunnel_name, local_addr}`
4. Server writes raw HTTP request on stream, reads response back
5. Client accept loop: reads header, dials `localhost:3000`, pipes bytes

**TCP connections:**
1. Server listens on public port (e.g. `:2222`)
2. Incoming TCP → server opens yamux stream, writes StreamHeader
3. Client dials local sshd, pipes bytes bidirectionally

## Deployment

### Docker

```bash
docker build -t 2nnel .
docker run -d \
    -p 443:443 \
    -v /var/lib/2nnel:/certs \
    2nnel server \
        --domain example.com \
        --auth-token supersecret \
        --acme-cache /certs
```

### systemd

```bash
# Copy binary
sudo cp 2nnel /usr/local/bin/

# Create user
sudo useradd -r -s /bin/false 2nnel
sudo mkdir -p /var/lib/2nnel/certs
sudo chown 2nnel:2nnel /var/lib/2nnel/certs

# Install service (edit domain + token first)
sudo cp 2nnel.service /etc/systemd/system/
sudo systemctl enable --now 2nnel
```

## Server flags

| Flag | Default | Description |
|------|---------|-------------|
| `--domain` | | Base domain (e.g. `example.com`) |
| `--port` | `443` | Public port |
| `--auth-token` | | Shared secret (empty = no auth) |
| `--dev` | false | Plain HTTP on port 8080, no TLS |
| `--tls-cert` | | Custom TLS cert (PEM) |
| `--tls-key` | | Custom TLS key (PEM) |
| `--acme-cache` | `/tmp/2nnel-certs` | Let's Encrypt cert cache dir |
| `--allowed-ports` | (all) | Comma-separated allowed TCP ports |

## Client flags

| Flag | Description |
|------|-------------|
| `--server` | Server URL (`wss://` or `ws://` for dev) |
| `--auth-token` | Auth token |
| `--tunnel` | Tunnel spec (repeatable) |
| `-c` / `--config` | YAML config file |

**Tunnel spec formats:**
- HTTP: `name:host:port` (e.g. `web:localhost:3000`)
- TCP: `name:host:port:tcp:remote_port` (e.g. `ssh:localhost:22:tcp:2222`)

## DNS setup

Point a wildcard record at your VPS:

```
*.example.com   A   1.2.3.4
example.com     A   1.2.3.4
```

Let's Encrypt will issue per-subdomain certs automatically on first access.

## Firewall

On the VPS, open:
- `443/tcp` — HTTPS + WSS control channel
- Any TCP ports you allow for TCP tunnels (e.g. `2222/tcp` for SSH)

The client needs only outbound port 443.
