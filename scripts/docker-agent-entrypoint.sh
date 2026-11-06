#!/bin/sh
# Container entrypoint for one XianCun Cloud agent (contributes idle memory as KV shards).
set -eu

SERVER="${SERVER:-http://server:8080}"
KV_ADDR="${KV_ADDR:-:16380}"

if [ -n "${NODE_TOKEN:-}" ]; then
  exec /usr/local/bin/xiancun-agent -server "$SERVER" -kv "$KV_ADDR" -node-token "$NODE_TOKEN"
fi
exec /usr/local/bin/xiancun-agent -server "$SERVER" -kv "$KV_ADDR"
