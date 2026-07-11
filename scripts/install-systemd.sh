#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$HOME/.local/bin"
UNIT_DIR="$HOME/.config/systemd/user"

mkdir -p "$BIN_DIR" "$UNIT_DIR" "$HOME/.local/share/ccusage-hub"
go build -trimpath -o "$BIN_DIR/ccusage-hub" "$ROOT/cmd/ccusage-hub"
install -m 0644 "$ROOT/deploy/systemd/ccusage-hub-server.service" "$UNIT_DIR/"
install -m 0644 "$ROOT/deploy/systemd/ccusage-hub-client.service" "$UNIT_DIR/"

systemctl --user daemon-reload
systemctl --user enable --now ccusage-hub-server.service

echo "Server installed. Install ccusage and then enable the local collector with:"
echo "  systemctl --user enable --now ccusage-hub-client.service"
