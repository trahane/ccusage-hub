#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$HOME/.local/bin"
AGENT_DIR="$HOME/Library/LaunchAgents"
PLIST="$AGENT_DIR/com.trahane.ccusage-hub.client.plist"

mkdir -p "$BIN_DIR" "$AGENT_DIR" "$HOME/Library/Logs"
go build -trimpath -o "$BIN_DIR/ccusage-hub" "$ROOT/cmd/ccusage-hub"
sed "s|__HOME__|$HOME|g" "$ROOT/deploy/launchd/com.trahane.ccusage-hub.client.plist" > "$PLIST"

launchctl bootout "gui/$(id -u)/com.trahane.ccusage-hub.client" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
echo "Client installed. Edit $PLIST if the Pi is not reachable as http://pibot:7432."
