#!/usr/bin/env bash
# install.sh — set up 2nnel relay server on this machine.
# Run directly on the VPS:
#
#   sudo bash install.sh --domain example.com --token mysecret [--cf-token TOKEN]
#
# Expects the 2nnel binary next to this script, or pass --binary ./path/to/2nnel
set -euo pipefail

# ── Parse args ────────────────────────────────────────────────────────────────
DOMAIN=""
TOKEN=""
PORT="443"
BINARY=""
CF_TOKEN=""

while [[ $# -gt 0 ]]; do
  case $1 in
    --domain)   DOMAIN="$2";   shift 2 ;;
    --token)    TOKEN="$2";    shift 2 ;;
    --port)     PORT="$2";     shift 2 ;;
    --binary)   BINARY="$2";   shift 2 ;;
    --cf-token) CF_TOKEN="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 --domain example.com --token secret [--port 443] [--binary ./2nnel] [--cf-token CF_TOKEN]"
      exit 0 ;;
    *) echo "Unknown arg: $1"; exit 1 ;;
  esac
done

if [[ -z "$DOMAIN" || -z "$TOKEN" ]]; then
  echo "Usage: $0 --domain example.com --token secret [--cf-token TOKEN]"
  exit 1
fi

[[ $EUID -eq 0 ]] || { echo "Run as root: sudo bash $0 $*"; exit 1; }

# ── Find binary ───────────────────────────────────────────────────────────────
if [[ -z "$BINARY" ]]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  for candidate in "$SCRIPT_DIR/2nnel" "$SCRIPT_DIR/2nnel-linux-amd64" "$SCRIPT_DIR/2nnel-linux-arm64"; do
    if [[ -x "$candidate" ]]; then
      BINARY="$candidate"
      break
    fi
  done
fi

if [[ -z "$BINARY" || ! -f "$BINARY" ]]; then
  if command -v go &>/dev/null; then
    echo "→ No binary found — building from source…"
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    cd "$SCRIPT_DIR"
    go build -ldflags="-s -w" -o /tmp/2nnel-built .
    BINARY="/tmp/2nnel-built"
  else
    echo "ERROR: No 2nnel binary found and Go is not installed."
    echo "  Copy the binary here first:"
    echo "    scp 2nnel-linux-amd64 root@thisVPS:~/"
    echo "  Then re-run this script."
    exit 1
  fi
fi

echo "→ Binary: $BINARY"

# ── Detect public IP ──────────────────────────────────────────────────────────
echo "→ Detecting public IP…"
VPS_IP=$(curl -sf --max-time 5 https://ifconfig.me \
      || curl -sf --max-time 5 https://api.ipify.org \
      || curl -sf --max-time 5 https://icanhazip.com \
      || true)
if [[ -z "$VPS_IP" ]]; then
  echo "  WARNING: could not detect public IP — DNS step will be skipped"
fi
[[ -n "$VPS_IP" ]] && echo "  Public IP: $VPS_IP"

# ── Cloudflare DNS ────────────────────────────────────────────────────────────
if [[ -n "$CF_TOKEN" && -n "$VPS_IP" ]]; then
  echo "→ Configuring Cloudflare DNS…"

  CF_API="https://api.cloudflare.com/client/v4"

  # Extract root domain (last two labels)
  ROOT_DOMAIN=$(echo "$DOMAIN" | rev | cut -d. -f1-2 | rev)

  # Get zone ID
  ZONE_RESP=$(curl -sf "$CF_API/zones?name=$ROOT_DOMAIN" \
    -H "Authorization: Bearer $CF_TOKEN" \
    -H "Content-Type: application/json")

  # Validate + extract zone ID
  if ! echo "$ZONE_RESP" | grep -q '"success":true'; then
    echo "  ERROR: Cloudflare API call failed. Check your --cf-token."
    echo "  Response: $ZONE_RESP"
    exit 1
  fi

  ZONE_ID=$(echo "$ZONE_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
if not d['result']:
    print('', end='')
else:
    print(d['result'][0]['id'], end='')
")

  if [[ -z "$ZONE_ID" ]]; then
    echo "  ERROR: zone '$ROOT_DOMAIN' not found in this Cloudflare account."
    exit 1
  fi
  echo "  Zone: $ROOT_DOMAIN ($ZONE_ID)"

  # Upsert an A record — create or update
  cf_upsert() {
    local NAME="$1"
    local EXISTING
    EXISTING=$(curl -sf "$CF_API/zones/$ZONE_ID/dns_records?type=A&name=$(python3 -c "import urllib.parse; print(urllib.parse.quote('$NAME'))")" \
      -H "Authorization: Bearer $CF_TOKEN" | \
      python3 -c "
import sys,json
d=json.load(sys.stdin)
print(d['result'][0]['id'] if d.get('result') else '', end='')
" 2>/dev/null || true)

    local PAYLOAD="{\"type\":\"A\",\"name\":\"$NAME\",\"content\":\"$VPS_IP\",\"ttl\":60,\"proxied\":false}"

    if [[ -n "$EXISTING" ]]; then
      RESULT=$(curl -sf -X PUT "$CF_API/zones/$ZONE_ID/dns_records/$EXISTING" \
        -H "Authorization: Bearer $CF_TOKEN" \
        -H "Content-Type: application/json" \
        -d "$PAYLOAD")
      echo "$RESULT" | python3 -c "
import sys,json; d=json.load(sys.stdin)
print('  updated: $NAME →  $VPS_IP') if d.get('success') else print('  WARN:', d.get('errors'))
"
    else
      RESULT=$(curl -sf -X POST "$CF_API/zones/$ZONE_ID/dns_records" \
        -H "Authorization: Bearer $CF_TOKEN" \
        -H "Content-Type: application/json" \
        -d "$PAYLOAD")
      echo "$RESULT" | python3 -c "
import sys,json; d=json.load(sys.stdin)
print('  created: $NAME → $VPS_IP') if d.get('success') else print('  WARN:', d.get('errors'))
"
    fi
  }

  cf_upsert "$DOMAIN"
  cf_upsert "*.$DOMAIN"
  echo "  DNS configured. Propagation: 1–5 min."

elif [[ -n "$CF_TOKEN" && -z "$VPS_IP" ]]; then
  echo "  WARN: --cf-token provided but could not detect public IP — skipping DNS"
fi

# ── Install binary ────────────────────────────────────────────────────────────
echo "→ Installing binary to /usr/local/bin/2nnel…"
cp "$BINARY" /usr/local/bin/2nnel
chmod +x /usr/local/bin/2nnel

# ── User + dirs ───────────────────────────────────────────────────────────────
echo "→ Creating user and directories…"
id -u 2nnel &>/dev/null || useradd -r -s /bin/false 2nnel
mkdir -p /var/lib/2nnel/certs
chown -R 2nnel:2nnel /var/lib/2nnel

# ── systemd ───────────────────────────────────────────────────────────────────
echo "→ Installing systemd service…"
cat > /etc/systemd/system/2nnel.service << UNIT
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

systemctl daemon-reload
systemctl enable --now 2nnel || true

# ── Firewall ──────────────────────────────────────────────────────────────────
echo "→ Opening firewall ports…"
if command -v ufw &>/dev/null; then
  ufw allow 80/tcp          > /dev/null 2>&1 || true
  ufw allow "${PORT}/tcp"   > /dev/null 2>&1 || true
  echo "  ufw: opened 80, $PORT"
elif command -v firewall-cmd &>/dev/null; then
  firewall-cmd --permanent --add-port=80/tcp          > /dev/null 2>&1 || true
  firewall-cmd --permanent --add-port="${PORT}/tcp"   > /dev/null 2>&1 || true
  firewall-cmd --reload                               > /dev/null 2>&1 || true
  echo "  firewalld: opened 80, $PORT"
else
  echo "  (no firewall tool found — open ports 80 and $PORT manually)"
fi

# ── Status ────────────────────────────────────────────────────────────────────
echo ""
if systemctl is-active --quiet 2nnel; then
  echo "✓ 2nnel is running"
else
  echo "✗ 2nnel failed to start — check logs:"
  echo "    journalctl -u 2nnel -n 30 --no-pager"
  journalctl -u 2nnel -n 20 --no-pager || true
  exit 1
fi

echo ""
echo "Connect from your machine:"
echo "  ./2nnel client \\"
echo "    --server wss://${DOMAIN} \\"
echo "    --auth-token ${TOKEN} \\"
echo "    --tunnel myapp:localhost:3000"
echo ""
echo "Dashboard: https://${DOMAIN}/_2nnel/?token=${TOKEN}"
echo "Logs:      journalctl -u 2nnel -f"
