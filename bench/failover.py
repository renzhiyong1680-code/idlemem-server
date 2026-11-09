#!/usr/bin/env python3
"""闲存云掉线自愈耗时测量（仅标准库）。

做法：先记录目标节点承载的分片，杀掉该 agent 进程，然后轮询 /api/v1/shard 与
/api/v1/nodes，记录三个时间点并输出 JSON：

  detected_ms  控制面第一次发现"该分片不再由目标节点服务"（状态离开 active 或换了节点）
  recovered_ms 该分片重新回到 active（通常已在别的节点上）
  node_offline_ms 目标节点被判定 offline

注意：心跳超时是 30s，所以 detected_ms 一般在 30s 上下；杀掉进程后立刻查 API 看到的
仍是旧状态，不能拿它当"已自愈"，脚本会一直等到真的发生变化。

用法：
  python bench/failover.py --api http://127.0.0.1:8080 --token <admin-token> \
      --node-id node-3 --pid 50372
"""

import argparse
import json
import os
import subprocess
import time
import urllib.request


def api_get(url, token):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read().decode())


def kill_pid(pid):
    if os.name == "nt":
        subprocess.run(["taskkill", "/PID", str(pid), "/F"], capture_output=True)
    else:
        os.kill(pid, 9)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://127.0.0.1:8080")
    ap.add_argument("--token", required=True)
    ap.add_argument("--node-id", required=True)
    ap.add_argument("--pid", type=int, required=True)
    ap.add_argument("--interval", type=float, default=0.25)
    ap.add_argument("--timeout", type=float, default=120.0)
    args = ap.parse_args()

    shards = api_get(args.api + "/api/v1/shard", args.token)
    mine = [s for s in shards if s.get("node_id") == args.node_id]
    if not mine:
        print(json.dumps({"error": "no shard on node " + args.node_id}, ensure_ascii=False))
        return

    before = {s["id"]: {"status": s["status"], "node_id": s["node_id"]} for s in mine}

    t0 = time.perf_counter()
    kill_pid(args.pid)
    killed_at = time.perf_counter()

    timeline = []
    detected_ms = recovered_ms = node_offline_ms = None
    left_original = False
    last_snapshot = None
    deadline = time.perf_counter() + args.timeout

    while time.perf_counter() < deadline:
        try:
            cur = {s["id"]: s for s in api_get(args.api + "/api/v1/shard", args.token)}
            nodes = {n["id"]: n["status"] for n in api_get(args.api + "/api/v1/nodes", args.token)}
        except Exception as e:
            timeline.append({"t_ms": round((time.perf_counter() - t0) * 1000), "error": str(e)})
            time.sleep(args.interval)
            continue

        states = {}
        for sid in before:
            s = cur.get(sid)
            states[sid] = (s["status"] + "@" + s["node_id"]) if s else "missing"

        snapshot = json.dumps([states, nodes.get(args.node_id)], sort_keys=True)
        if snapshot != last_snapshot:
            last_snapshot = snapshot
            timeline.append({
                "t_ms": round((time.perf_counter() - t0) * 1000),
                "states": states,
                "node_status": nodes.get(args.node_id),
            })

        if node_offline_ms is None and nodes.get(args.node_id) == "offline":
            node_offline_ms = round((time.perf_counter() - killed_at) * 1000)

        moved = all((sid in cur and cur[sid]["status"] != "active") or
                    (sid in cur and cur[sid]["node_id"] != args.node_id)
                    for sid in before)
        if moved:
            left_original = True
        if detected_ms is None and left_original:
            detected_ms = round((time.perf_counter() - killed_at) * 1000)

        if left_original and all(cur.get(sid, {}).get("status") == "active" for sid in before):
            recovered_ms = round((time.perf_counter() - killed_at) * 1000)
            break

        time.sleep(args.interval)

    after = {}
    try:
        for s in api_get(args.api + "/api/v1/shard", args.token):
            if s["id"] in before:
                after[s["id"]] = {"status": s["status"], "node_id": s["node_id"]}
    except Exception:
        pass

    print(json.dumps({
        "node_id": args.node_id,
        "pid": args.pid,
        "shards_before": before,
        "shards_after": after,
        "detected_ms": detected_ms,
        "recovered_ms": recovered_ms,
        "node_offline_ms": node_offline_ms,
        "timeline": timeline,
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
