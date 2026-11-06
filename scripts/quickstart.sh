#!/usr/bin/env sh
# XianCun Cloud (idlemem) one-command local trial for Linux / macOS.
#
# Starts the control plane, one agent, a demo tenant and a demo order, then prints
# ready-to-paste redis-cli commands.
#
# Usage:
#   ./scripts/quickstart.sh                 # download v0.1.1 binaries, run everything
#   ./scripts/quickstart.sh --stop          # stop the previous run
#   ./scripts/quickstart.sh --build-local   # use ./bin (or ../idlemem-core/bin) instead of downloading
#   ./scripts/quickstart.sh --gateway 127.0.0.1:6380 --api 127.0.0.1:8088
#
# NOTICE: this script is not exercised in CI; the .ps1 twin is verified on Windows.
#         The flow is identical, so a failure here is most likely a missing curl or
#         an unreachable github.com download.
set -eu

API="127.0.0.1:8080"
GATEWAY="127.0.0.1:6379"
KV="127.0.0.1:16380"
VERSION="v0.1.1"
WORKDIR="$(cd "$(dirname "$0")/.." && pwd)/quickstart"
BUILD_LOCAL=0
STOP=0
FORCE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --api) API="$2"; shift 2 ;;
    --gateway) GATEWAY="$2"; shift 2 ;;
    --kv) KV="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --workdir) WORKDIR="$2"; shift 2 ;;
    --build-local) BUILD_LOCAL=1; shift ;;
    --stop) STOP=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

BIN="$WORKDIR/bin"
LOG="$WORKDIR/log"
CFG="$WORKDIR/config.json"
SERVER_PID="$WORKDIR/server.pid"
AGENT_PID="$WORKDIR/agent.pid"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

say() { printf '==> %s\n' "$1"; }
info() { printf '    %s\n' "$1"; }
warn() { printf '[warn] %s\n' "$1" >&2; }

stop_pid() {
  pidfile="$1"
  if [ -f "$pidfile" ]; then
    pid="$(cat "$pidfile" 2>/dev/null || true)"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      info "stopped pid $pid"
    fi
    rm -f "$pidfile"
  fi
}

if [ "$STOP" = "1" ]; then
  say "stopping previous run"
  stop_pid "$AGENT_PID"
  stop_pid "$SERVER_PID"
  info "done"
  exit 0
fi

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) A=amd64 ;;
  arm64|aarch64) A=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
case "$OS" in
  Linux) P=linux ;;
  Darwin) P=darwin ;;
  *) echo "unsupported os: $OS (use scripts/quickstart.ps1 on Windows)" >&2; exit 1 ;;
esac

mkdir -p "$BIN" "$LOG"

if [ "$FORCE" = "1" ]; then
  for port in "${GATEWAY##*:}" "${KV##*:}" "${API##*:}"; do
    if command -v lsof >/dev/null 2>&1; then
      pids="$(lsof -ti tcp:"$port" 2>/dev/null || true)"
      for p in $pids; do warn "port $port busy, killing pid $p"; kill -9 "$p" 2>/dev/null || true; done
    fi
  done
fi

say "resolving binaries"
VER_NO="${VERSION#v}"
BASE="https://github.com/renzhiyong1680-code/idlemem-server/releases/download/$VERSION"
if [ "$BUILD_LOCAL" = "1" ]; then
  [ -x "$REPO_ROOT/bin/idlemem-server" ] || { echo "missing $REPO_ROOT/bin/idlemem-server; run: go build -o bin/idlemem-server ./cmd/server" >&2; exit 1; }
  CORE_AGENT="$(dirname "$REPO_ROOT")/idlemem-core/bin/xiancun-agent"
  [ -x "$CORE_AGENT" ] || { echo "missing $CORE_AGENT; run in idlemem-core: go build -o bin/xiancun-agent ./cmd/xiancun-agent" >&2; exit 1; }
  cp "$REPO_ROOT/bin/idlemem-server" "$BIN/idlemem-server"
  cp "$CORE_AGENT" "$BIN/xiancun-agent"
  info "using locally built binaries"
else
  if [ ! -x "$BIN/idlemem-server" ]; then
    info "downloading idlemem-server ..."
    curl -fSL --retry 2 -o "$BIN/idlemem-server.part" "$BASE/server_${VER_NO}_${P}_${A}" && mv "$BIN/idlemem-server.part" "$BIN/idlemem-server"
    chmod +x "$BIN/idlemem-server"
  fi
  if [ ! -x "$BIN/xiancun-agent" ]; then
    info "downloading xiancun-agent ..."
    curl -fSL --retry 2 -o "$BIN/xiancun-agent.part" "$BASE/agent_${VER_NO}_${P}_${A}" && mv "$BIN/xiancun-agent.part" "$BIN/xiancun-agent"
    chmod +x "$BIN/xiancun-agent"
  fi
  info "binaries ready in $BIN"
fi

say "generating tokens"
TOKENS="$("$BIN/idlemem-server" -gen-token)"
ADMIN="$(printf '%s' "$TOKENS" | sed -n 's/^admin_token: *\(.*\)$/\1/p')"
NODE="$(printf '%s' "$TOKENS" | sed -n 's/^node_token: *\(.*\)$/\1/p')"
[ -n "$ADMIN" ] && [ -n "$NODE" ] || { echo "cannot parse -gen-token output: $TOKENS" >&2; exit 1; }
info "admin_token ${ADMIN%"${ADMIN#????????}"}... generated"

say "writing config"
cat > "$CFG" <<EOF
{
  "api_addr": "$API",
  "gateway_addr": "$GATEWAY",
  "db_path": "$WORKDIR/idlemem-server.db",
  "default_replicas": 1,
  "customer_price_per_gb_hour": 0.0004,
  "supplier_price_per_gb_hour": 0.00032,
  "admin_token": "$ADMIN",
  "node_token": "$NODE"
}
EOF
info "$CFG"

while :; do
  if [ ! -x "$BIN/idlemem-server" ]; then
    echo "server binary missing: $BIN/idlemem-server" >&2; exit 1
  fi
  break
done

say "starting control plane"
( cd "$WORKDIR" && nohup "$BIN/idlemem-server" -config "$CFG" > "$LOG/server.log" 2>&1 & echo $! > "$SERVER_PID" )
info "pid $(cat "$SERVER_PID")"

READY=0
i=0
while [ "$i" -lt 50 ]; do
  if curl -fsS "http://${API}/healthz" >/dev/null 2>&1; then READY=1; break; fi
  i=$((i+1)); sleep 0.5
done
[ "$READY" = "1" ] || { echo "control plane did not become healthy; see $LOG/server.log" >&2; exit 1; }
info "api http://$API ready"

say "starting agent"
( cd "$WORKDIR" && nohup "$BIN/xiancun-agent" -server "http://${API}" -kv "$KV" -node-token "$NODE" > "$LOG/agent.log" 2>&1 & echo $! > "$AGENT_PID" )
info "pid $(cat "$AGENT_PID")"

ONLINE=0
i=0
while [ "$i" -lt 40 ]; do
  body="$(curl -fsS -H "Authorization: Bearer $ADMIN" "http://${API}/api/v1/nodes" 2>/dev/null || true)"
  case "$body" in
    *'"status":"online"'*) ONLINE=1; break ;;
  esac
  i=$((i+1)); sleep 0.7
done
if [ "$ONLINE" = "1" ]; then info "agent online"; else warn "agent started but no online node reported yet; see $LOG/agent.log"; fi

say "creating demo tenant and order"
TENANT="$(curl -fsS -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $ADMIN" \
  -d '{"name":"demo-tenant","quota_bytes":10737418240}' "http://${API}/api/v1/tenants")"
TID="$(printf '%s' "$TENANT" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
TTK="$(printf '%s' "$TENANT" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
info "tenant $TID"

ORDER="$(curl -fsS -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $ADMIN" \
  -d "{\"tenant_id\":\"$TID\",\"capacity_bytes\":2147483648,\"replicas\":1,\"expire_hours\":720}" "http://${API}/api/v1/orders" 2>/dev/null || true)"
case "$ORDER" in
  *'"error"'*|"") warn "order creation failed (usually means the agent reported less than 2 GB free); response: $ORDER" ;;
  *) OID="$(printf '%s' "$ORDER" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"; info "order $OID capacity 2 GB" ;;
esac

printf '\n%s\n' "XianCun Cloud local trial is up"
printf '  REST API   %s\n' "http://$API"
printf '  gateway    %s   (Redis protocol)\n' "$GATEWAY"
printf '  agent KV   %s\n' "$KV"
printf '  workdir    %s\n' "$WORKDIR"
printf '  admin      %s\n' "$ADMIN"
printf '  tenant     demo-tenant  token %s\n' "$TTK"
printf '\n%s\n' "Try it with any Redis client:"
printf '  redis-cli -h %s -p %s\n' "${API%%:*}" "${GATEWAY##*:}"
printf '  AUTH %s\n' "$TTK"
printf '  SET hello world\n'
printf '  GET hello\n'
printf '\n%s\n' "Stop everything:  ./scripts/quickstart.sh --stop --workdir $WORKDIR"
