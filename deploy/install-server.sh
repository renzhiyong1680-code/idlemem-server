#!/usr/bin/env bash
# 闲存云调度控制面（idlemem-server）一键部署脚本（Linux systemd）
#
# 功能：
#   1. 安装二进制（本地路径或 URL，缺省使用当前目录/已构建产物）
#   2. 写入生产配置模板（如未提供）
#   3. 创建专用运行用户与目录
#   4. 安装并启用 systemd 服务，开机自启
#   5. 健康检查 + 状态确认
#
# 用法：
#   sudo ./install-server.sh --binary ./idlemem-server
#   sudo ./install-server.sh --binary https://example.com/idlemem-server-linux-amd64
#   sudo ./install-server.sh --config /path/to/server-config.json
#
# 环境变量（可选）：
#   XIANCUN_USER   运行用户（默认 xiancun）
#   XIANCUN_DIR    安装根目录（默认 /opt/xiancun）
#   XIANCUN_DATA   数据目录（默认 /var/lib/xiancun）
set -euo pipefail

# ---------- 默认值 ----------
BINARY_SRC="${1:-}"
CONFIG_SRC=""
INSTALL_USER="${XIANCUN_USER:-xiancun}"
INSTALL_DIR="${XIANCUN_DIR:-/opt/xiancun}"
DATA_DIR="${XIANCUN_DATA:-/var/lib/xiancun}"
BIN_PATH="/usr/local/bin/xiancun-server"
CONFIG_PATH="/etc/xiancun/server-config.json"
UNIT_NAME="xiancun-server.service"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"

log()  { printf '[install] %s\n' "$*"; }
die()  { printf '[install] ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

# ---------- 参数解析 ----------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) BINARY_SRC="${2:-}"; shift 2 ;;
    --config) CONFIG_SRC="${2:-}"; shift 2 ;;
    -h|--help) usage ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

# ---------- 前置检查 ----------
[[ $EUID -eq 0 ]] || die "please run as root (sudo)"
command -v systemctl >/dev/null 2>&1 || die "systemctl not found (not a systemd system?)"

# ---------- 1. 安装二进制 ----------
if [[ -n "$BINARY_SRC" ]]; then
  TMP_BIN="$(mktemp)"
  trap 'rm -f "$TMP_BIN"' EXIT
  if [[ "$BINARY_SRC" =~ ^https?:// ]]; then
    log "downloading binary from $BINARY_SRC"
    curl -fsSL "$BINARY_SRC" -o "$TMP_BIN" || die "download failed"
  else
    log "using local binary $BINARY_SRC"
    cp "$BINARY_SRC" "$TMP_BIN"
  fi
  chmod +x "$TMP_BIN"
  install -m 0755 "$TMP_BIN" "$BIN_PATH"
  log "installed binary -> $BIN_PATH"
else
  [[ -x "$BIN_PATH" ]] || die "no binary at $BIN_PATH; pass --binary <path|url>"
  log "using existing binary $BIN_PATH"
fi

# ---------- 2. 运行用户与目录 ----------
id "$INSTALL_USER" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$INSTALL_USER"
mkdir -p "$INSTALL_DIR" "$DATA_DIR" /etc/xiancun
chown -R "$INSTALL_USER:$INSTALL_USER" "$INSTALL_DIR" "$DATA_DIR"

# ---------- 3. 配置文件 ----------
if [[ -n "$CONFIG_SRC" ]]; then
  install -m 0640 -o "$INSTALL_USER" -g "$INSTALL_USER" "$CONFIG_SRC" "$CONFIG_PATH"
  log "installed config -> $CONFIG_PATH"
elif [[ ! -f "$CONFIG_PATH" ]]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [[ -f "$SCRIPT_DIR/config.json.example" ]]; then
    sed "s|/var/lib/xiancun/idlemem-server.db|${DATA_DIR}/idlemem-server.db|" \
      "$SCRIPT_DIR/config.json.example" > "$CONFIG_PATH"
    chown "$INSTALL_USER:$INSTALL_USER" "$CONFIG_PATH"
    chmod 0640 "$CONFIG_PATH"
    log "wrote config template -> $CONFIG_PATH (edit it before first use if needed)"
  else
    die "no config provided and no template found"
  fi
else
  log "using existing config $CONFIG_PATH"
fi

# ---------- 4. systemd 单元 ----------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! -f "$SCRIPT_DIR/xiancun-server.service" ]]; then
  die "xiancun-server.service not found next to this script"
fi
sed -e "s|^User=.*|User=${INSTALL_USER}|" \
    -e "s|^Group=.*|Group=${INSTALL_USER}|" \
    -e "s|^WorkingDirectory=.*|WorkingDirectory=${DATA_DIR}|" \
    -e "s|^ExecStart=.*|ExecStart=${BIN_PATH} -config ${CONFIG_PATH}|" \
    "$SCRIPT_DIR/xiancun-server.service" > "$UNIT_PATH"
log "installed unit -> $UNIT_PATH"

# ---------- 5. 启动并设置开机自启 ----------
systemctl daemon-reload
systemctl enable --now "$UNIT_NAME"
log "service enabled and started"

# ---------- 6. 状态与健康检查 ----------
sleep 2
systemctl --no-pager status "$UNIT_NAME" --lines=5 || true
API_PORT="$(grep -o '"api_addr"[[:space:]]*:[[:space:]]*"[^"]*"' "$CONFIG_PATH" | head -1 | sed 's/.*"api_addr"[[:space:]]*:[[:space:]]*"//; s/"//' )"
API_ADDR="${API_PORT:-:8080}"
HOST="${API_ADDR%:*}"; HOST="${HOST:-127.0.0.1}"
PORT="${API_ADDR##*:}"
if curl -fsS "http://${HOST}:${PORT}/healthz" >/dev/null 2>&1; then
  log "health check OK: http://${HOST}:${PORT}/healthz"
else
  log "health check failed: http://${HOST}:${PORT}/healthz (check: journalctl -u ${UNIT_NAME} -f)"
fi

log "done. useful commands:"
log "  systemctl status ${UNIT_NAME}    # 服务状态"
log "  journalctl -u ${UNIT_NAME} -f    # 实时日志"
log "  systemctl stop ${UNIT_NAME}      # 停止"
log "  systemctl disable --now ${UNIT_NAME}  # 卸载开机自启"
