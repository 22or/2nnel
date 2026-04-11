#!/usr/bin/env bash
# deploy.sh — build 2nnel and install it on a remote VPS in one step.
#
# Usage:
#   ./deploy.sh user@host --domain example.com --token mysecret [options]
#
# Options:
#   --domain           Base domain (required)
#   --token            Auth token for clients (required)
#   --port             Public port (default: 443)
#   --cf-token         Cloudflare API token — auto-creates DNS records if provided
#
# Requirements: ssh access to the VPS, Go installed locally.
set -euo pipefail

# ── Parse args ────────────────────────────────────────────────────────────────
TARGET=""
DOMAIN=""
TOKEN=""
PORT="443"
CF_TOKEN=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --domain)   DOMAIN="$2";   shift 2 ;;
    --token)    TOKEN="$2";    shift 2 ;;
    --port)     PORT="$2";     shift 2 ;;
    --cf-token) CF_TOKEN="$2"; shift 2 ;;
    *)          TARGET="$1";   shift   ;;
  esac
done

if [[ -z "$TARGET" || -z "$DOMAIN" || -z "$TOKEN" ]]; then
  echo "Usage: $0 user@host --domain example.com --token secret [--port 443] [--cf-token TOKEN]"
  exit 1
fi

BINARY="2nnel-linux-amd64"

# ── Build ─────────────────────────────────────────────────────────────────────
echo "→ Building Linux binary…"
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$BINARY" .
echo "  $(du -sh "$BINARY" | cut -f1)B"

# ── Get VPS public IP (needed for DNS) ────────────────────────────────────────
echo "→ Detecting VPS public IP…"
VPS_IP=$(ssh -o BatchMode=yes "$TARGET" 'curl -sf https://ifconfig.me || curl -sf https://api.ipify.org')
if [[ -z "$VPS_IP" ]]; then
  echo "  ERROR: could not detect VPS IP. Check internet access on VPS."
  exit 1
fi
echo "  VPS IP: $VPS_IP"

# ── Cloudflare DNS ────────────────────────────────────────────────────────────
if [[ -n "$CF_TOKEN" ]]; then
  echo "→ Configuring Cloudflare DNS…"

  CF_API="https://api.cloudflare.com/client/v4"

  # Get zone ID for the root domain (strip any subdomain)
  ROOT_DOMAIN=$(echo "$DOMAIN" | awk -F. '{print $(NF-1)"."$NF}')
  ZONE_RESP=$(curl -sf "$CF_API/zones?name=$ROOT_DOMAIN" \
    -H "Authorization: Bearer $CF_TOKEN" \
    -H "Content-Type: application/json")

  ZONE_ID=$(echo "$ZONE_RESP" | python3 -c "
import sys, json
d = json.load(sys.stdin)
if not d.get('success') or not d['result']:
    print('ERROR: zone not found for $ROOT_DOMAIN', file=sys.stderr)
    sys.exit(1)
print(d['result'][0]['id'])
")
  echo "  zone: $ROOT_DOMAIN ($ZONE_ID)"

  # Upsert A record for NAME pointing to VPS_IP.
  # Creates if missing, updates if exists.
  cf_upsert() {
    local NAME="$1"
    # Check existing
    EXISTING=$(curl -sf "$CF_API/zones/$ZONE_ID/dns_records?type=A&name=$NAME" \
      -H "Authorization: Bearer $CF_TOKEN" | \
      python3 -c "import sys,json; d=json.load(sys.stdin); print(d['result'][0]['id'] if d['result'] else '')" 2>/dev/null || true)

    PAYLOAD=$(python3 -c "import json; print(json.dumps({'type':'A','name':'$NAME','content':'$VPS_IP','ttl':60,'proxied':False}))")

    if [[ -n "$EXISTING" ]]; then
      curl -sf -X PUT "$CF_API/zones/$ZONE_ID/dns_records/$EXISTING" \
        -H "Authorization: Bearer $CF_TOKEN" \
        -H "Content-Type: application/json" \
        -d "$PAYLOAD" | python3 -c "import sys,json; d=json.load(sys.stdin); print('  updated:', '$NAME', '→', '$VPS_IP') if d['success'] else print('  ERROR:', d)"
    else
      curl -sf -X POST "$CF_API/zones/$ZONE_ID/dns_records" \
        -H "Authorization: Bearer $CF_TOKEN" \
        -H "Content-Type: application/json" \
        -d "$PAYLOAD" | python3 -c "import sys,json; d=json.load(sys.stdin); print('  created:', '$NAME', '→', '$VPS_IP') if d['success'] else print('  ERROR:', d)"
    fi
  }

  cf_upsert "$DOMAIN"
  cf_upsert "*.$DOMAIN"
  echo "  DNS updated. Propagation takes 1–5 minutes."
else
  echo "  (skipping DNS — no --cf-token provided)"
fi

# ── Deploy to VPS ─────────────────────────────────────────────────────────────
echo "→ Copying binary to $TARGET…"
scp -q "$BINARY" "$TARGET:/tmp/2nnel"

echo "→ Installing on VPS…"
ssh -t "$TARGET" bash -s -- "$DOMAIN" "$TOKEN" "$PORT" << 'REMOTE'
set -euo pipefail
DOMAIN="$1"
TOKEN="$2"
PORT="$3"

sudo mv /tmp/2nnel /usr/local/bin/2nnel
sudo chmod +x /usr/local/bin/2nnel

id -u 2nnel &>/dev/null || sudo useradd -r -s /bin/false 2nnel
sudo mkdir -p /var/lib/2nnel/certs
sudo chown -R 2nnel:2nnel /var/lib/2nnel

sudo tee /etc/systemd/system/2nnel.service > /dev/null << UNIT
[Unit]
Description=2nnel reverse tunnel relay
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/2nnel server \
    --domain ${DOMAIN} \
    --port ${PORT} \
    --auth-token ${TOKEN} \
    --acme-cache /var/lib/2nnel/certs
Restart=always
RestartSec=5
User=2nnel
Group=2nnel
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable --now 2nnel

if command -v ufw &>/dev/null; then
  sudo ufw allow 80/tcp   > /dev/null 2>&1 || true
  sudo ufw allow "${PORT}/tcp" > /dev/null 2>&1 || true
fi

sudo systemctl status 2nnel --no-pager -l | head -8
REMOTE

rm -f "$BINARY"

# ── Done ─────────────────────────────────────────────────────────────────────
echo ""
echo "✓ Done. 2nnel is running on $TARGET"
echo ""
echo "Connect from your machine:"
echo "  ./2nnel client \\"
echo "    --server wss://${DOMAIN} \\"
echo "    --auth-token ${TOKEN} \\"
echo "    --tunnel myapp:localhost:3000"
echo ""
echo "Dashboard: https://${DOMAIN}/_2nnel/?token=${TOKEN}"
