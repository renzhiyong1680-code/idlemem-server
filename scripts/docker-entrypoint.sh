#!/bin/sh
# Container entrypoint for the XianCun Cloud control plane.
#
# On first start it writes /data/config.json using the tokens from the environment,
# then execs the server so signals reach it directly. Re-running the container keeps
# the existing config and database, so the trial is resumable.
set -eu

DATA_DIR="${DATA_DIR:-/data}"
CFG="$DATA_DIR/config.json"
mkdir -p "$DATA_DIR"

if [ -z "${NODE_TOKEN:-}" ]; then
  echo "[entrypoint] NODE_TOKEN is empty: node registration will be unauthenticated" >&2
fi
if [ -z "${ADMIN_TOKEN:-}" ]; then
  echo "[entrypoint] ADMIN_TOKEN is empty: the management API will be unauthenticated (dev only)" >&2
fi

if [ ! -f "$CFG" ]; then
  cat > "$CFG" <<EOF
{
  "api_addr": ":8080",
  "gateway_addr": ":6379",
  "db_path": "$DATA_DIR/idlemem-server.db",
  "default_replicas": 1,
  "customer_price_per_gb_hour": 0.0004,
  "supplier_price_per_gb_hour": 0.00032,
  "admin_token": "${ADMIN_TOKEN:-}",
  "node_token": "${NODE_TOKEN:-}"
}
EOF
  echo "[entrypoint] wrote $CFG"
else
  echo "[entrypoint] reusing existing $CFG"
fi

exec /usr/local/bin/idlemem-server -config "$CFG"
