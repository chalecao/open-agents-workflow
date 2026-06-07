#!/usr/bin/env bash
# Initialize .env from .env.example and bind NEXT_PUBLIC_API_URL +
# CORS_ALLOWED_ORIGINS to the local machine's LAN IP, so that a phone or
# another device on the same network can reach the dev server.
#
# Usage:
#   bash scripts/init-env.sh                # uses .env
#   bash scripts/init-env.sh .env.lan       # uses a custom env file
#   PORT=8080 FRONTEND_PORT=3000 bash scripts/init-env.sh
#
# Re-running is safe: it only updates the two keys it owns, leaving every
# other line in .env (DB creds, JWT, OAuth, S3, ...) untouched.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

ENV_FILE="${1:-.env}"
PORT="${PORT:-8080}"
FRONTEND_PORT="${FRONTEND_PORT:-3000}"

# ---------- Step 1: ensure .env exists ----------
if [ ! -f "$ENV_FILE" ]; then
  if [ ! -f .env.example ]; then
    echo "✗ .env.example not found at $REPO_ROOT/.env.example"
    exit 1
  fi
  echo "==> Creating $ENV_FILE from .env.example..."
  cp .env.example "$ENV_FILE"
fi

# ---------- Step 2: detect a non-loopback IPv4 address ----------
detect_local_ip() {
  local ip=""

  if [ "$(uname)" = "Darwin" ]; then
    # macOS: en0 = Wi-Fi, en1 = Thunderbolt Ethernet / secondary.
    for iface in en0 en1 en2; do
      ip="$(ipconfig getifaddr "$iface" 2>/dev/null || true)"
      if [ -n "$ip" ] && [ "$ip" != "127.0.0.1" ]; then
        printf '%s\n' "$ip"
        return 0
      fi
    done
  else
    # Linux: try the routing-table shortcut, then fall back to hostname.
    ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1; i<=NF; i++) if ($i == "src") {print $(i+1); exit}}')"
    if [ -z "$ip" ]; then
      ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    fi
  fi

  # Final fallback: parse `ifconfig` / `ip addr` for the first non-loopback v4.
  if [ -z "$ip" ] || [ "$ip" = "127.0.0.1" ]; then
    ip="$(ifconfig 2>/dev/null | awk '/inet / && $2 != "127.0.0.1" {print $2; exit}')"
  fi
  if [ -z "$ip" ] || [ "$ip" = "127.0.0.1" ]; then
    ip="$(ip -4 addr show 2>/dev/null | awk '/inet / && $2 !~ /^127\./ {sub(/\/.*/, "", $2); print $2; exit}')"
  fi

  if [ -n "$ip" ] && [ "$ip" != "127.0.0.1" ]; then
    printf '%s\n' "$ip"
    return 0
  fi
  return 1
}

if ! LOCAL_IP="$(detect_local_ip)"; then
  echo "✗ Could not detect a non-loopback IPv4 address on this host."
  echo "  Connect to Wi-Fi/Ethernet, or set NEXT_PUBLIC_API_URL / CORS_ALLOWED_ORIGINS manually in $ENV_FILE."
  exit 1
fi

API_URL="http://${LOCAL_IP}:${PORT}"

# CORS_ALLOWED_ORIGINS is a comma-separated list. The LAN IP entry is
# intentionally written without a port (the example in the env file uses
# bare hostnames). Loopback dev origins keep their ports so curl /
# browser dev tools from this same machine still pass CORS.
CORS_VALUE="http://${LOCAL_IP},http://localhost:${FRONTEND_PORT},http://localhost:${PORT},http://127.0.0.1:${FRONTEND_PORT},http://127.0.0.1:${PORT}"

echo "==> Detected local IP: $LOCAL_IP"
echo "==> Setting NEXT_PUBLIC_API_URL=${API_URL}"
echo "==> Setting CORS_ALLOWED_ORIGINS=${CORS_VALUE}"

# ---------- Step 3: rewrite the two keys in-place (idempotent) ----------
# Use a temp file so we don't depend on BSD vs GNU `sed -i` flags.
TMP_FILE="$(mktemp "${ENV_FILE}.XXXXXX")"
trap 'rm -f "$TMP_FILE"' EXIT

awk -v api_url="$API_URL" -v cors_value="$CORS_VALUE" '
  BEGIN { updated_api = 0; updated_cors = 0 }
  /^NEXT_PUBLIC_API_URL=/        { print "NEXT_PUBLIC_API_URL=" api_url;        updated_api   = 1; next }
  /^CORS_ALLOWED_ORIGINS=/       { print "CORS_ALLOWED_ORIGINS=" cors_value;    updated_cors  = 1; next }
  { print }
  END {
    if (!updated_api)  print "NEXT_PUBLIC_API_URL=" api_url
    if (!updated_cors) print "CORS_ALLOWED_ORIGINS=" cors_value
  }
' "$ENV_FILE" > "$TMP_FILE"

mv "$TMP_FILE" "$ENV_FILE"
trap - EXIT

echo "✓ $ENV_FILE is ready. Frontend devices on the same LAN can now reach the API at ${API_URL}."
