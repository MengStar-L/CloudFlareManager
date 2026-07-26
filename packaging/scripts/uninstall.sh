#!/bin/sh
set -eu

INSTALL_DIR="/opt/CloudFlareManager"

if [ "$(id -u)" -ne 0 ]; then
    echo "uninstall.sh must run as root" >&2
    exit 1
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now cf-r2-manager.service 2>/dev/null || true
fi

rm -f /etc/systemd/system/cf-r2-manager.service
rm -f /usr/local/bin/cf-r2-manager
rm -f "$INSTALL_DIR/cf-r2-manager" "$INSTALL_DIR/cf-r2-manager.old" "$INSTALL_DIR/cf-r2-manager.new"

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
fi

echo "CloudFlareManager binaries were removed."
echo "Configuration and data were preserved in ${INSTALL_DIR}."
