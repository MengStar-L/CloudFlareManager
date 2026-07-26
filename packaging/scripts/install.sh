#!/bin/sh
# CloudFlareManager (cf-r2-manager) Linux installer.
#
# Online (recommended):
#   curl -fsSL https://github.com/MengStar-L/CloudFlareManager/releases/latest/download/install-linux.sh -o /tmp/install-cfm.sh
#   sudo sh /tmp/install-cfm.sh
#
# Options:
#   --install-dir DIR   install root (default /opt/CloudFlareManager)
#   --port N            admin console port (default 14325)
#   --password PASS     admin password for unattended install
#   --binary PATH       offline mode: install this binary instead of downloading
set -eu

REPO="MengStar-L/CloudFlareManager"
INSTALL_DIR="/opt/CloudFlareManager"
ADMIN_PORT="14325"
ADMIN_PASSWORD=""
BINARY=""

while [ "$#" -gt 0 ]; do
    case "$1" in
        --install-dir) INSTALL_DIR="$2"; shift 2 ;;
        --port) ADMIN_PORT="$2"; shift 2 ;;
        --password) ADMIN_PASSWORD="$2"; shift 2 ;;
        --binary) BINARY="$2"; shift 2 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root (use sudo)" >&2
    exit 1
fi
if ! command -v systemctl >/dev/null 2>&1; then
    echo "this installer requires systemd" >&2
    exit 1
fi

case "$(uname -m)" in
    x86_64 | amd64) ARCH="amd64" ;;
    aarch64 | arm64) ARCH="arm64" ;;
    armv7l | armv7) ARCH="arm" ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

echo "==> CloudFlareManager installer"
echo "    install dir : ${INSTALL_DIR}"
echo "    console port: ${ADMIN_PORT}"
echo "    architecture: linux/${ARCH}"

# ---------- 下载并校验二进制 ----------
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

if [ -n "$BINARY" ]; then
    if [ ! -f "$BINARY" ]; then
        echo "binary not found: $BINARY" >&2
        exit 1
    fi
    cp "$BINARY" "$WORK_DIR/cf-r2-manager"
else
    if ! command -v curl >/dev/null 2>&1; then
        echo "curl is required" >&2
        exit 1
    fi
    BASE="https://github.com/${REPO}/releases/latest/download"
    echo "==> downloading cf-r2-manager-linux-${ARCH}"
    curl -fL --proto '=https' --tlsv1.2 -o "$WORK_DIR/cf-r2-manager" "${BASE}/cf-r2-manager-linux-${ARCH}"
    curl -fL --proto '=https' --tlsv1.2 -o "$WORK_DIR/checksums.txt" "${BASE}/checksums.txt"
    echo "==> verifying checksum"
    EXPECTED=$(awk -v name="cf-r2-manager-linux-${ARCH}" '$2 == name || $2 == "./"name || $2 == "*"name { print $1 }' "$WORK_DIR/checksums.txt")
    if [ -z "$EXPECTED" ]; then
        echo "checksums.txt has no entry for cf-r2-manager-linux-${ARCH}" >&2
        exit 1
    fi
    ACTUAL=$(sha256sum "$WORK_DIR/cf-r2-manager" | awk '{ print $1 }')
    if [ "$EXPECTED" != "$ACTUAL" ]; then
        echo "checksum mismatch: expected $EXPECTED got $ACTUAL" >&2
        exit 1
    fi
fi
chmod 0755 "$WORK_DIR/cf-r2-manager"

# ---------- 用户、目录与文件 ----------
if ! getent group cf-r2-manager >/dev/null 2>&1; then
    groupadd --system cf-r2-manager
fi
if ! id cf-r2-manager >/dev/null 2>&1; then
    useradd --system --gid cf-r2-manager --home-dir "$INSTALL_DIR" \
        --shell /usr/sbin/nologin --comment "CloudFlareManager service" cf-r2-manager
fi

install -d -o cf-r2-manager -g cf-r2-manager -m 0750 "$INSTALL_DIR"
install -d -o cf-r2-manager -g cf-r2-manager -m 0750 "$INSTALL_DIR/data"

# 二进制归服务用户所有：软件内自升级需要原地替换它。
# 先落到 .new 再原子改名，避免直接覆盖运行中的可执行文件。
install -o cf-r2-manager -g cf-r2-manager -m 0755 "$WORK_DIR/cf-r2-manager" "$INSTALL_DIR/cf-r2-manager.new"
mv -f "$INSTALL_DIR/cf-r2-manager.new" "$INSTALL_DIR/cf-r2-manager"
ln -sf "$INSTALL_DIR/cf-r2-manager" /usr/local/bin/cf-r2-manager

if [ ! -e "$INSTALL_DIR/config.yaml" ]; then
    cat > "$INSTALL_DIR/config.yaml" <<EOF
data_dir: ${INSTALL_DIR}/data
database_path: ${INSTALL_DIR}/data/manager.db
master_key_file: ${INSTALL_DIR}/data/master.key
log_level: info
trusted_proxies:
  - 127.0.0.1/32
  - ::1/128
listeners:
  # 管理控制台（Web 界面）
  admin: 0.0.0.0:${ADMIN_PORT}
  # 协议前端默认仅监听本机，经反向代理暴露；直连请改为 0.0.0.0
  s3: 127.0.0.1:14326
  webdav: 127.0.0.1:14327
  ai: 127.0.0.1:14328
  metrics: 127.0.0.1:14329
r2:
  logical_bucket: storage
  temp_dir: ${INSTALL_DIR}/data/tmp
  # 服务端强制分片块大小（字节，最小 5MiB）：大 PUT 切块转发，磁盘峰值仅一块；小磁盘可调小
  upload_chunk_bytes: 67108864
  storage_soft_limit_bytes: 9000000000
  class_a_soft_limit: 900000
  class_b_soft_limit: 9000000
ai:
  neuron_soft_limit: 9000
  max_retry_accounts: 2
EOF
    chown root:cf-r2-manager "$INSTALL_DIR/config.yaml"
    chmod 0640 "$INSTALL_DIR/config.yaml"
fi

cat > /etc/systemd/system/cf-r2-manager.service <<EOF
[Unit]
Description=CF-R2Manager Cloudflare control plane
Documentation=https://github.com/${REPO}
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=cf-r2-manager
Group=cf-r2-manager
ExecStart=${INSTALL_DIR}/cf-r2-manager server --config ${INSTALL_DIR}/config.yaml
Restart=on-failure
RestartSec=5s
TimeoutStopSec=20s

RuntimeDirectory=cf-r2-manager
RuntimeDirectoryMode=0750
UMask=0027

NoNewPrivileges=yes
PrivateDevices=yes
PrivateTmp=yes
ProtectClock=yes
ProtectControlGroups=yes
ProtectHome=yes
ProtectHostname=yes
ProtectKernelLogs=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectSystem=strict
ReadWritePaths=${INSTALL_DIR}
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload

# ---------- 初始化管理员密码 ----------
if [ ! -e "$INSTALL_DIR/data/manager.db" ]; then
    if [ -z "$ADMIN_PASSWORD" ]; then
        printf "设置管理员登录密码: "
        stty -echo
        read -r ADMIN_PASSWORD
        stty echo
        printf "\n"
    fi
    if [ -z "$ADMIN_PASSWORD" ]; then
        echo "管理员密码不能为空" >&2
        exit 1
    fi
    PASS_FILE="$INSTALL_DIR/data/.admin-password"
    umask 077
    printf '%s' "$ADMIN_PASSWORD" > "$PASS_FILE"
    chown cf-r2-manager:cf-r2-manager "$PASS_FILE"
    su -s /bin/sh cf-r2-manager -c \
        "'$INSTALL_DIR/cf-r2-manager' init --config '$INSTALL_DIR/config.yaml' --admin-password-file '$PASS_FILE'"
    rm -f "$PASS_FILE"
fi

systemctl enable cf-r2-manager.service >/dev/null 2>&1 || true
if systemctl is-active --quiet cf-r2-manager.service; then
    # 升级场景：服务已在运行，必须重启才能加载新二进制
    systemctl restart cf-r2-manager.service
else
    systemctl start cf-r2-manager.service
fi

IP=$(hostname -I 2>/dev/null | awk '{ print $1 }')
[ -n "$IP" ] || IP="<服务器IP>"
echo ""
echo "==> 安装完成"
echo "    控制台地址 : http://${IP}:${ADMIN_PORT}"
echo "    配置文件   : ${INSTALL_DIR}/config.yaml"
echo "    数据目录   : ${INSTALL_DIR}/data"
echo "    服务状态   : systemctl status cf-r2-manager"
