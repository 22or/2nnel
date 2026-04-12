# 2nnel

Self-hosted reverse tunnel. Exposes local services to the internet through outbound-only connections. Free alternative to ngrok / Cloudflare Tunnel. Single binary, no accounts.

```
[Browser] ──► [2nnel server on VPS] ◄══outbound══ [2nnel client behind NAT]
                   public :443                           localhost:3000
```

## Features

- **HTTP tunnels** — expose local web apps via subdomain (`myapp.tunnel.example.com`)
- **TCP tunnels** — raw TCP forwarding (SSH, databases, game servers)
- **WebSocket support** — WS connections through HTTP tunnels work transparently
- **Multiplexed** — one outbound WebSocket carries all tunnels via yamux
- **Dynamic tunnel management** — add/remove tunnels live from the dashboard, no restart needed
- **Client service install** — one command installs client as a systemd service
- **Auto-reconnect** — exponential backoff, tunnels restore on reconnect
- **Admin dashboard** — live metrics, per-tunnel traffic, disconnect controls
- **Multi-client** — multiple clients, each with independent tunnel sets
- **YAML config** — full config file support alongside CLI flags

## Setup

### Server (VPS)

**With nginx in front (recommended if nginx already serves other sites):**

```bash
# Get wildcard cert
certbot certonly --dns-cloudflare \
  --dns-cloudflare-credentials ~/.cf-creds.ini \
  -d tunnel.example.com -d '*.tunnel.example.com'

# nginx config
server {
    listen 443 ssl;
    server_name tunnel.example.com *.tunnel.example.com;

    ssl_certificate     /etc/letsencrypt/live/tunnel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tunnel.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host       $host;
        proxy_set_header X-Real-IP  $remote_addr;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        proxy_buffering off;
    }
}

server {
    listen 80;
    server_name tunnel.example.com *.tunnel.example.com;
    return 301 https://$host$request_uri;
}

# Run 2nnel in dev mode (nginx handles TLS)
./2nnel server \
    --dev \
    --port 8080 \
    --domain tunnel.example.com \
    --auth-token supersecret \
    --tcp-port-range 2200-2300
```

**Standalone (2nnel owns 443 directly):**

```bash
sudo ./2nnel server \
    --domain example.com \
    --auth-token supersecret \
    --tcp-port-range 2200-2300
```

**DNS records required:**

```
example.com        A   <VPS IP>
*.example.com      A   <VPS IP>
```

**Firewall:**

```bash
sudo ufw allow 443/tcp
sudo ufw allow 2200:2300/tcp   # TCP tunnel range
```

### Client (local machine)

**Run directly:**

```bash
# HTTP tunnel: myapp.tunnel.example.com → localhost:3000
./2nnel client \
    --server wss://tunnel.example.com \
    --auth-token supersecret \
    --tunnel myapp:localhost:3000

# TCP tunnel (port auto-assigned from server's range)
./2nnel client \
    --server wss://tunnel.example.com \
    --auth-token supersecret \
    --tunnel ssh:localhost:22:tcp

# Explicit remote port
./2nnel client \
    --server wss://tunnel.example.com \
    --auth-token supersecret \
    --tunnel ssh:localhost:22:tcp:2222
```

**Install as a service** (persists across reboots, tunnels manageable from dashboard):

```bash
sudo ./2nnel client install \
    --server wss://tunnel.example.com \
    --auth-token supersecret \
    --tunnel myapp:localhost:3000 \
    --tunnel ssh:localhost:22:tcp
```

Writes config to `/etc/2nnel/client.yaml`, installs and starts `2nnel-client` systemd service. Logs: `journalctl -u 2nnel-client -f`

**YAML config:**

```yaml
server: wss://tunnel.example.com
auth_token: supersecret
tunnels:
  - name: myapp
    local: localhost:3000
    type: http
  - name: ssh
    local: localhost:22
    type: tcp
```

```bash
./2nnel client -c config.yaml
```

### SSH access via tunnel

Add once to `~/.ssh/config` on any machine you SSH from:

```
Host *.tunnel.example.com
    Port 2200
```

Then connect normally:

```bash
ssh user@ssh.tunnel.example.com
```

## Dashboard

Visit `https://tunnel.example.com?token=<auth-token>` for the admin dashboard.

- Live metrics: bytes in/out, request counts, active connections
- **Add Tunnel** — click `+ Tunnel` on a connected client to add a tunnel without restarting
- **Remove Tunnel** — remove individual tunnels per client
- **Disconnect** — forcibly disconnect a client

Tunnels added/removed via dashboard are persisted to the client's config file automatically.

## Server flags

| Flag | Default | Description |
|------|---------|-------------|
| `--domain` | | Base domain for HTTP tunnels |
| `--port` | `443` | Public port |
| `--auth-token` | | Shared secret (empty = no auth) |
| `--dev` | false | Plain HTTP, no TLS (use behind nginx) |
| `--tls-cert` | | Custom TLS cert (PEM) |
| `--tls-key` | | Custom TLS key (PEM) |
| `--acme-cache` | `/tmp/2nnel-certs` | Let's Encrypt cert cache dir |
| `--tcp-port-range` | | Port range for TCP tunnels (e.g. `2200-2300`) |
| `--allowed-ports` | (all) | Restrict TCP to specific ports |

## Client flags

| Flag | Description |
|------|-------------|
| `--server` | Server URL (`wss://` or `ws://` for dev) |
| `--auth-token` | Auth token |
| `--tunnel` | Tunnel spec (repeatable) |
| `-c` / `--config` | YAML config file |

**Tunnel spec formats:**
- HTTP: `name:host:port` → `myapp:localhost:3000`
- TCP (auto port): `name:host:port:tcp` → `ssh:localhost:22:tcp`
- TCP (fixed port): `name:host:port:tcp:remote_port` → `ssh:localhost:22:tcp:2222`

## Architecture

```
Client                              Server
──────                              ──────
WebSocket (outbound) ──────────────► /ws handler
yamux.Client                         yamux.Server
  │
  ├─ stream 1 (control)             Accept stream 1
  │    ← auth                        ├─ authenticate
  │    → auth_ack                    ├─ register tunnels
  │    → register_tunnel             └─ heartbeat loop
  │    ← tunnel_registered
  │    ← heartbeat / → heartbeat
  │    ← add_tunnel (dashboard)
  │    → register_tunnel (dynamic)
  │    ← remove_tunnel (dashboard)
  │
  └─ Accept loop                    Open new stream per connection
       read StreamHeader             write StreamHeader
       dial local service            forward public traffic
       pipe bytes ↔
```

**HTTP requests:** server matches `Host` header → opens yamux stream → forwards raw HTTP → client dials local service → pipes bytes.

**TCP connections:** server listens on assigned port → incoming TCP opens yamux stream → client dials local service → pipes bytes.

**WebSocket through HTTP tunnel:** server detects `Upgrade: websocket` → hijacks connection → pipes raw bytes over yamux stream.

## Docker

```bash
docker build -t 2nnel .
docker run -d \
    -p 443:443 -p 80:80 \
    -v /var/lib/2nnel:/certs \
    2nnel server \
        --domain example.com \
        --auth-token supersecret \
        --acme-cache /certs \
        --tcp-port-range 2200-2300
```
