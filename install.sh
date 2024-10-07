#!/bin/bash
set -euo pipefail

BIN_DIR="/usr/local/bin"

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
cd "$SCRIPT_DIR"

if [[ "$EUID" -ne 0 ]]; then
  echo "Please run as root"
  exit 1
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# Install node modules to temporary directory
npm ci --prefix "$TMP_DIR" --no-audit --no-fund
npm run --prefix "$TMP_DIR" pkg

sed "s|BIN_DIR|$BIN_DIR|g" < "systemd/podman-wsl-service.service" > "$TMP_DIR/podman-wsl-service"

install -Dm755 "$TMP_DIR/dist/podman-wsl-service" "$BIN_DIR/podman-wsl-service"
install -Dm644 "$TMP_DIR/podman-wsl-service.service" "/etc/systemd/system/podman-wsl-service.service"
install -Dm644 "systemd/podman-wsl-service.socket" "/etc/systemd/system/podman-wsl-service.socket"

systemctl daemon-reload
systemctl enable --now podman-wsl-service.socket
