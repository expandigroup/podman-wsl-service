#!/bin/bash
set -euo pipefail

BIN_DIR="/usr/local/bin"

if [[ "$EUID" -ne 0 ]]; then
  echo "Please run as root"
  exit 1
fi

systemctl disable --now podman-wsl-service.socket podman-wsl-service.service
rm -f "/etc/systemd/system/podman-wsl-service.socket" "/etc/systemd/system/podman-wsl-service.service"
rm -f "$BIN_DIR/podman-wsl-service"
