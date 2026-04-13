# 2nnel

Self-hosted reverse tunnel. Exposes local services to the internet through outbound-only connections — no accounts, no cloud, no per-seat pricing. Drop-in replacement for ngrok or Cloudflare Tunnel on infrastructure you control.

```
[Browser] ──► [2nnel-server on VPS] ◄══outbound WebSocket══ [2nnel on laptop]
               myapp.example.com:443                          localhost:3000
```

## Philosophy

- **You own the infra.** One VPS, one binary. No data transits third-party servers.
- **Zero friction.** A tunnel is one command. No account creation, no dashboard login, no agent config file required to get started.
- **Promote when ready.** A tunnel running locally can be promoted to run permanently on the server with one click — the server builds it via Nixpacks and runs it in Docker. No Dockerfile needed on the client.
- **Production is your problem.** 2nnel gets your app online. Whether it's production-ready is up to you.

---

## Features

| | |
|--|--|
| HTTP tunnels | `myapp.tunnel.example.com` → `localhost:3000` |
| TCP tunnels | Raw TCP forwarding (SSH, databases, game servers) |
| WebSocket | Transparent WS support through HTTP tunnels |
| Promote | Elevate a local tunnel to a permanent server-hosted app |
| Live build log | Dashboard streams Nixpacks build output in real time |
| File sharing | `2nnel share file.zip` — instant public download link |
| Dashboard | Live metrics, per-tunnel traffic, dynamic tunnel management |
| Dynamic tunnels | Add/remove tunnels from the dashboard without client restart |
| YAML config | Full config file support alongside CLI flags |
| Service install | One command installs the client as a systemd service |
| Auto-reconnect | Exponential backoff; tunnels restore on reconnect |
| Multi-client | Multiple independent clients per server |
| State persistence | Promoted apps survive server restarts |

---

## How it works

```
Client (2nnel)                      Server (2nnel-server)
──────────────                      ─────────────────────
WebSocket (outbound) ──────────────► /ws
yamux multiplex                      yamux multiplex
  │
  ├─ stream 0 (control)
  │    auth handshake
  │    tunnel registration
  │    heartbeat loop
  │    ← add_tunnel / remove_tunnel (dashboard)
  │    ← promote (dashboard trigger)
  │
  └─ data streams (one per connection)
       read StreamHeader
       dial local service
       pipe bytes ↔
```

**HTTP:** server matches `Host` header → opens yamux stream to client → client dials local service → pipes raw HTTP bytes.

**TCP:** server listens on assigned port → opens yamux stream → client dials local service → pipes bytes.

**WebSocket:** server detects `Upgrade: websocket` → hijacks connection → pipes over yamux stream.

**Promote:** dashboard sends promote trigger → client tarballs the project directory → POSTs to server → server runs `nixpacks build` (streaming logs to dashboard) → `docker run --network host` → server detects listening port → routes `myapp.example.com` to Docker container.

---

## Installation

### Server

**Requirements:** Linux VPS with Docker and Nixpacks if you want the promote feature.

```bash
# Download (replace VERSION and ARCH)
curl -L https://github.com/22or/2nnel/releases/latest/download/2nnel-server-linux-amd64 \
  -o /usr/local/bin/2nnel-server && chmod +x /usr/local/bin/2nnel-server

# Optional: install Docker + Nixpacks for promote support
apt install docker.io
curl -sSL https://nixpacks.com/install.sh | bash

# Verify promote dependencies
2nnel-server check
```

**DNS records:**

```
tunnel.example.com     A   <VPS IP>
*.tunnel.example.com   A   <VPS IP>
```

**Run standalone (2nnel-server owns port 443, handles TLS via Let's Encrypt):**

```bash
sudo 2nnel-server \
    --domain tunnel.example.com \
    --auth-token supersecret \
    --tcp-port-range 2200-2300
```

**Run behind nginx (nginx handles TLS):**

```nginx
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
        proxy_read_timeout  600s;   # required for long Nixpacks builds
        proxy_send_timeout  600s;
        client_max_body_size 500m;  # required for promote tarball uploads
        proxy_buffering off;
    }
}
```

```bash
2nnel-server \
    --dev \
    --port 8080 \
    --domain tunnel.example.com \
    --auth-token supersecret \
    --tcp-port-range 2200-2300
```

**Systemd service** (`/etc/systemd/system/2nnel-server.service`):

```ini
[Unit]
Description=2nnel relay server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/2nnel-server \
    --domain tunnel.example.com \
    --port 443 \
    --auth-token supersecret \
    --acme-cache /var/lib/2nnel/certs
Restart=always
RestartSec=5
User=2nnel
Group=2nnel
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

**Firewall:**

```bash
ufw allow 443/tcp
ufw allow 2200:2300/tcp   # TCP tunnel range
```

---

### Client

```bash
# Download
curl -L https://github.com/22or/2nnel/releases/latest/download/2nnel-linux-amd64 \
  -o /usr/local/bin/2nnel && chmod +x /usr/local/bin/2nnel
```

**One-shot tunnel:**

```bash
# HTTP tunnel: myapp.tunnel.example.com → localhost:3000
2nnel --server wss://tunnel.example.com --auth-token supersecret \
      --tunnel myapp:localhost:3000

# TCP (auto port from server range)
2nnel --server wss://tunnel.example.com --auth-token supersecret \
      --tunnel ssh:localhost:22:tcp

# TCP (fixed remote port)
2nnel --server wss://tunnel.example.com --auth-token supersecret \
      --tunnel ssh:localhost:22:tcp:2222
```

**YAML config** (`~/.config/2nnel/config.yaml`):

```yaml
server: wss://tunnel.example.com
auth_token: supersecret
tunnels:
  - name: myapp
    local: localhost:3000
    type: http
  - name: api
    local: localhost:8080
    type: http
  - name: ssh
    local: localhost:22
    type: tcp
```

```bash
2nnel -c ~/.config/2nnel/config.yaml
```

**Install as a systemd service** (persists across reboots, tunnels manageable from dashboard):

```bash
sudo 2nnel install \
    --server wss://tunnel.example.com \
    --auth-token supersecret \
    --tunnel myapp:localhost:3000
```

Writes `/etc/2nnel/client.yaml`, installs and starts `2nnel.service`.
Logs: `journalctl -u 2nnel -f`

---

## Dashboard

Open `https://tunnel.example.com/_2nnel/?token=<auth-token>`

- Live traffic metrics per tunnel (bytes, requests, active connections)
- **Add Tunnel** — add a tunnel to a connected client without restarting it
- **Remove Tunnel** — remove individual tunnels
- **Disconnect** — forcibly close a client session
- **Promote** — deploy a local tunnel as a permanent server-hosted app (see below)

---

## Promote

Promote turns a locally-running HTTP tunnel into a permanently deployed app on the server. The client sends the project directory; the server builds and runs it.

**Requirements (server only):** Docker, Nixpacks.

**Setup:**

1. Set the project directory for a tunnel — either via CLI flag or the dashboard "Set Dir" button:

```bash
2nnel --server wss://tunnel.example.com --auth-token supersecret \
      --tunnel myapp:localhost:3000 --dir ~/projects/myapp
```

Or click **Set Dir** on the tunnel row in the dashboard.

2. Click **Promote** in the dashboard. Confirm.

3. The dashboard shows a live build log as Nixpacks builds the Docker image. When done, the app appears as a deployed app card with its public URL.

**What happens under the hood:**

- Client walks the project directory, tarballs it (respects `.gitignore` for directories; always includes `.env` and other source files needed for the build)
- Server extracts the tarball, runs `nixpacks build` (streaming output to the dashboard)
- Server runs the container with `docker run --network host --restart=unless-stopped`
- Server detects which port the app bound to and starts routing traffic there
- Client tunnel is evicted — the subdomain now points to Docker

**Nixpacks detects** Node.js, Python, Go, Ruby, Rust, Java, PHP, .NET, and more from standard project files (`package.json`, `requirements.txt`, `go.mod`, etc.).

**Make your app production-ready before promoting.** 2nnel doesn't add a production server for you — if your `npm run start` launches `ng serve`, that's what runs.

**Stop a deployed app** from the dashboard → container is stopped, removed, and the image is deleted.

---

## File sharing

Instantly share any local file via a temporary public URL:

```bash
2nnel share ./report.pdf \
    --server wss://tunnel.example.com \
    --auth-token supersecret

# Sharing "report.pdf" at: https://share-a3f1.tunnel.example.com/report.pdf
# Press Ctrl+C to stop.
```

---

## SSH via TCP tunnel

Add to `~/.ssh/config` on the machines you SSH from:

```
Host *.tunnel.example.com
    Port 2200
```

Then:

```bash
ssh user@myhost.tunnel.example.com
```

---

## Server flags

| Flag | Default | Description |
|------|---------|-------------|
| `--domain` | | Base domain for HTTP tunnels |
| `--port` | `443` | Public port |
| `--auth-token` | | Shared secret (empty = no auth) |
| `--dev` | `false` | Plain HTTP, no TLS (use behind nginx/caddy) |
| `--tls-cert` | | Custom TLS cert path (PEM) |
| `--tls-key` | | Custom TLS key path (PEM) |
| `--acme-cache` | `/tmp/2nnel-certs` | Let's Encrypt cert cache dir |
| `--tcp-port-range` | | Port range for TCP tunnels (e.g. `2200-2300`) |
| `--allowed-ports` | (all) | Restrict TCP to specific ports |
| `--deploy-dir` | (system temp) | Directory for promote build files and state |

## Client flags

| Flag | Description |
|------|-------------|
| `--server` | Server WebSocket URL (`wss://` or `ws://` for dev) |
| `--auth-token` | Auth token |
| `--tunnel` | Tunnel spec (repeatable) |
| `--dir` | Project directory for promote (single-tunnel mode only) |
| `-c` / `--config` | YAML config file |

**Tunnel spec:**

```
HTTP:            name:host:port           myapp:localhost:3000
TCP (auto port): name:host:port:tcp       ssh:localhost:22:tcp
TCP (fixed):     name:host:port:tcp:port  ssh:localhost:22:tcp:2222
```

---

## Build from source

```bash
git clone https://github.com/22or/2nnel
cd 2nnel
make all           # builds ./2nnel-server and ./2nnel
make install       # installs both to /usr/local/bin
```

Requires Go 1.22+.
