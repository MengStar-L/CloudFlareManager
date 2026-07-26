#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root" >&2
    exit 1
fi

if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
    echo "usage: $0 /path/to/cf-r2-manager" >&2
    exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary=$1

if ! getent group cf-r2-manager >/dev/null 2>&1; then
    groupadd --system cf-r2-manager
fi
if ! id cf-r2-manager >/dev/null 2>&1; then
    useradd --system --gid cf-r2-manager --home-dir /var/lib/cf-r2-manager \
        --shell /usr/sbin/nologin --comment "CF-R2Manager service" cf-r2-manager
fi

install -d -o root -g cf-r2-manager -m 0750 /etc/cf-r2-manager
install -d -o cf-r2-manager -g cf-r2-manager -m 0750 /var/lib/cf-r2-manager
install -m 0755 "$binary" /usr/local/bin/cf-r2-manager
if [ ! -e /etc/cf-r2-manager/config.yaml ]; then
    install -o root -g cf-r2-manager -m 0640 "$script_dir/../config.yaml" /etc/cf-r2-manager/config.yaml
fi
install -o root -g root -m 0644 "$script_dir/../systemd/cf-r2-manager.service" \
    /etc/systemd/system/cf-r2-manager.service

systemctl daemon-reload

echo "CF-R2Manager installed but not started."
echo "Initialize it as the service user, then run:"
echo "  systemctl enable --now cf-r2-manager"
echo "  systemctl status cf-r2-manager"
