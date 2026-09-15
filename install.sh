#!/bin/bash
# Installs braid as a background service that starts at login.
#
# After this there is no app to open: the engine is simply always running, and
# anything on this machine or this network can use it.
set -euo pipefail

BIN_DIR="$HOME/.local/bin"
CACHE_DIR="$HOME/.braid/cache"
CONF_DIR="$HOME/.braid"
PLIST="$HOME/Library/LaunchAgents/com.shaazkhan.braid.plist"
PORT="${BRAID_PORT:-8422}"

command -v go >/dev/null 2>&1 || export PATH="$HOME/.local/go/bin:$PATH"

echo "Building…"
mkdir -p "$BIN_DIR" "$CACHE_DIR" "$CONF_DIR" "$HOME/Library/LaunchAgents"
go build -ldflags="-s -w" -o "$BIN_DIR/braid" ./cmd/braid

# A stable token, so bookmarks and player links keep working across restarts.
if [ ! -f "$CONF_DIR/token" ]; then
  openssl rand -hex 16 > "$CONF_DIR/token"
  chmod 600 "$CONF_DIR/token"
fi
TOKEN="$(cat "$CONF_DIR/token")"

cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.shaazkhan.braid</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN_DIR/braid</string>
    <string>serve</string>
    <string>-port</string><string>$PORT</string>
    <string>-token</string><string>$TOKEN</string>
    <string>-cache</string><string>$CACHE_DIR</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$CONF_DIR/braid.log</string>
  <key>StandardErrorPath</key><string>$CONF_DIR/braid.log</string>
</dict>
</plist>
PLIST_EOF

launchctl unload "$PLIST" 2>/dev/null || true
launchctl load "$PLIST"

echo "Waiting for it to come up…"
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/api/links?t=$TOKEN" 2>/dev/null; then
    echo
    echo "braid is running, and will start itself at login."
    echo
    echo "  Open this, on this Mac or any device on your network:"
    for ip in $(ipconfig getifaddr en0 2>/dev/null) 127.0.0.1; do
      echo "    http://$ip:$PORT/?t=$TOKEN"
    done
    echo
    echo "  Uplinks it can use right now:"
    curl -fsS "http://127.0.0.1:$PORT/api/links?t=$TOKEN" |
      sed 's/},{/}\n{/g' | sed -E 's/.*"label":"([^"]*)".*"metered":(true|false).*/    \1 (metered: \2)/'
    echo
    echo "  To stop it:    launchctl unload $PLIST"
    exit 0
  fi
  sleep 0.5
done

echo "braid did not come up. Check $CONF_DIR/braid.log" >&2
exit 1
