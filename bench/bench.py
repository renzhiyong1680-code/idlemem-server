#!/usr/bin/env python3
"""闲存云极简压测客户端（仅标准库，不需要 redis-py）。

用法：

  # 经网关（需租户令牌）
  python bench/bench.py latency --addr 127.0.0.1:6379 --auth <tenant-token> --ops 5000

  # 直连节点 KV 端口（不需要令牌）
  python bench/bench.py latency --addr 127.0.0.1:16380 --ops 5000

  # 吞吐
  python bench/bench.py throughput --addr 127.0.0.1:6379 --auth <token> --threads 20 --seconds 10

输出为一行 JSON，便于贴进文档或做对比。
"""

import argparse
import json
import socket
import statistics
import threading
import time


class Resp:
    """够用的 RESP 客户端：只发定长数组命令，解析单行/批量/整数/数组回复。"""

    def __init__(self, addr, timeout=5.0):
        host, port = addr.rsplit(":", 1)
        self.sock = socket.create_connection((host, int(port)), timeout=timeout)
        self.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        self.buf = b""

    def _readline(self):
        while b"\r\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("connection closed")
            self.buf += chunk
        line, self.buf = self.buf.split(b"\r\n", 1)
        return line

    def _read(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("connection closed")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def command(self, *args):
        payload = b"*%d\r\n" % len(args)
        for a in args:
            b = a if isinstance(a, bytes) else str(a).encode()
            payload += b"$%d\r\n%s\r\n" % (len(b), b)
        self.sock.sendall(payload)
        return self._reply()

    def _reply(self):
        line = self._readline()
        kind, body = line[:1], line[1:]
        if kind == b"+":
            return body.decode()
        if kind == b"-":
            raise RuntimeError(body.decode())
        if kind == b":":
            return int(body)
        if kind == b"$":
            size = int(body)
            if size < 0:
                return None
            data = self._read(size + 2)[:-2]
            return data.decode(errors="replace")
        if kind == b"*":
            count = int(body)
            if count < 0:
                return None
            return [self._reply() for _ in range(count)]
        raise RuntimeError("unexpected reply: %r" % line)

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def pct(values, p):
    if not values:
        return 0.0
    k = max(0, min(len(values) - 1, int(round((p / 100.0) * (len(values) - 1)))))
    return values[k]


def summarize(label, samples_us):
    samples_us.sort()
    return {
        "target": label,
        "ops": len(samples_us),
        "p50_ms": round(pct(samples_us, 50) / 1000.0, 3),
        "p95_ms": round(pct(samples_us, 95) / 1000.0, 3),
        "p99_ms": round(pct(samples_us, 99) / 1000.0, 3),
        "max_ms": round((samples_us[-1] if samples_us else 0) / 1000.0, 3),
        "mean_ms": round(statistics.fmean(samples_us) / 1000.0, 3) if samples_us else 0.0,
    }


def connect(args):
    c = Resp(args.addr, timeout=args.timeout)
    if args.auth:
        c.command("AUTH", args.auth)
    return c


def cmd_latency(args):
    c = connect(args)
    c.command("PING")
    set_us, get_us = [], []
    for i in range(args.ops):
        key = "%s:%d" % (args.prefix, i % args.keyspace)
        t0 = time.perf_counter_ns()
        c.command("SET", key, "v" * args.value_size, "EX", args.ttl)
        t1 = time.perf_counter_ns()
        set_us.append((t1 - t0) / 1000.0)
        t2 = time.perf_counter_ns()
        c.command("GET", key)
        t3 = time.perf_counter_ns()
        get_us.append((t3 - t2) / 1000.0)
    c.close()
    out = {
        "mode": "latency",
        "addr": args.addr,
        "ops": args.ops,
        "sets": summarize("SET", set_us),
        "gets": summarize("GET", get_us),
    }
    print(json.dumps(out, ensure_ascii=False))


def cmd_throughput(args):
    counter = {"set": 0, "get": 0, "errors": 0}
    lock = threading.Lock()
    stop_at = time.perf_counter() + args.seconds

    def worker(idx):
        local_set = local_get = local_err = 0
        try:
            c = connect(args)
            c.command("PING")
            key = "%s:w%d" % (args.prefix, idx)
            while time.perf_counter() < stop_at:
                try:
                    c.command("SET", key, "v" * args.value_size)
                    local_set += 1
                    c.command("GET", key)
                    local_get += 1
                except Exception:
                    local_err += 1
                    break
            c.close()
        except Exception:
            local_err += 1
        with lock:
            counter["set"] += local_set
            counter["get"] += local_get
            counter["errors"] += local_err

    threads = [threading.Thread(target=worker, args=(i,), daemon=True) for i in range(args.threads)]
    started = time.perf_counter()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    elapsed = time.perf_counter() - started
    ops = counter["set"] + counter["get"]
    out = {
        "mode": "throughput",
        "addr": args.addr,
        "threads": args.threads,
        "seconds": round(elapsed, 2),
        "set_ops": counter["set"],
        "get_ops": counter["get"],
        "errors": counter["errors"],
        "total_ops_per_sec": round(ops / elapsed, 1),
        "command_ops_per_sec": round(2 * ops / elapsed, 1),
    }
    print(json.dumps(out, ensure_ascii=False))


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)

    common = dict(
        addr=dict(help="host:port"),
        auth=dict(default="", help="AUTH 令牌（网关必填，节点 KV 端口可省）"),
        prefix=dict(default="bench", help="键前缀"),
        value_size=dict(default=200, type=int, help="值长度（字节）"),
        timeout=dict(default=5.0, type=float),
    )
    subs = {}
    for name in ("latency", "throughput"):
        p = sub.add_parser(name)
        subs[name] = p
        for flag, spec in common.items():
            p.add_argument("--" + flag, **spec)

    subs["latency"].add_argument("--ops", type=int, default=5000)
    subs["latency"].add_argument("--keyspace", type=int, default=1000)
    subs["latency"].add_argument("--ttl", type=int, default=60)
    subs["latency"].set_defaults(func=cmd_latency)

    subs["throughput"].add_argument("--threads", type=int, default=20)
    subs["throughput"].add_argument("--seconds", type=float, default=10.0)
    subs["throughput"].set_defaults(func=cmd_throughput)

    args = ap.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
