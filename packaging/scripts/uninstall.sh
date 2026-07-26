#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "uninstall.sh must run as root" >&2
    exit 1
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now cf-r2-manager.service 2>/dev/null || true
fi

rm -f /etc/systemd/system/cf-r2-manager.service
rm -f /usr/local/bin/cf-r2-manager

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
fi

echo "CF-R2Manager binaries were removed."
echo "Configuration and data were preserved in /etc/cf-r2-manager and /var/lib/cf-r2-manager."
