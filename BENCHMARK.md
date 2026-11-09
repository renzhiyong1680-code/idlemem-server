# 性能基准

本文件里的数字全部是在本机实测出来的，环境和命令都写在下面，别人可以照着重跑。没有实测的部分不写。

## 测试环境

| 项 | 值 |
| --- | --- |
| 日期 | 2026-11-09 |
| CPU | 13th Gen Intel Core i5-13490F（10 核 16 线程） |
| 内存 | 32 GB（实测可用 31.8 GB） |
| 系统 | Windows 10 专业版 10.0.19045 |
| Go | go1.27.3 windows/amd64 |
| 拓扑 | 单机三节点：控制面 `127.0.0.1:8080`、网关 `127.0.0.1:6379`、Agent KV `127.0.0.1:16380/16381/16382` |
| 部署 | `scripts/quickstart.ps1 -BuildLocal -Nodes 3 -Force`，全部走回环地址 |
| 压测端 | Python 3.13.16 + `bench/bench.py`（纯标准库，相当于 `redis-cli` 的串行/多线程压法） |

> 所有流量都在本机回环上，不经过真实网卡，也没有跨机网络延迟。这组数字是**引擎本身的天花板**，不是公网部署的预期值，别直接拿去做对外的性能承诺。

## 延迟（串行单连接，5000 次 SET+GET，值 200 字节，TTL 60s）

| 路径 | 命令 | p50 | p95 | p99 | max | 平均 |
| --- | --- | --- | --- | --- | --- | --- |
| 直连节点 `:16380` | SET | 0.026 ms | 0.046 ms | 0.112 ms | 0.415 ms | 0.030 ms |
| 直连节点 `:16380` | GET | 0.028 ms | 0.050 ms | 0.124 ms | 0.262 ms | 0.032 ms |
| 经网关 `:6379` | SET | 0.209 ms | 0.352 ms | 0.487 ms | 1.634 ms | 0.229 ms |
| 经网关 `:6379` | GET | 0.031 ms | 0.058 ms | 0.108 ms | 0.432 ms | 0.036 ms |

读网关读得出来两个结论：

- **GET 走网关几乎不额外花钱**：p50 从 0.028 ms 到 0.031 ms，只多了路由查表。
- **SET 经网关是直连的 8 倍左右**（p50 0.209 ms）。因为写要把数据落到该租户订单的**每一个副本分片**上，副本越多写越慢——这是设计上换可靠性的代价，不是抖动。

## 吞吐（每线程循环 SET+GET，值 200 字节，10 秒）

| 路径 | 并发 | SET+GET 对数/秒 | 命令数/秒 | 错误 |
| --- | --- | --- | --- | --- |
| 直连节点 `:16380` | 20 线程 | 39,752 | 79,505 | 0 |
| 经网关 `:6379` | 20 线程 | 13,060 | 26,119 | 0 |
| 经网关 `:6379` | 50 线程 | 779 | 1,559 | 0 |

**20 线程是当前网关的拐点。** 并发加到 50 线程时吞吐反而掉到 1/17，而且没有报错——是排队，不是失败。这一项属于**已知瓶颈**，不要再对外说"网关能线性扩展"：

- 网关对同一租户/分片的写路径上有串行化的锁与副本扇出，线程数超过临界点后排队时间盖过了并行收益；
- 节点直连侧没有这个问题（同一台机器 20 线程仍有 7.9 万命令/秒），说明瓶颈在网关进程内，不在 kvstore。

后续要么按分片拆分网关的锁粒度，要么前面加一层连接/请求批处理。这个数字留着当基线。

## 掉线自愈（三节点、订单双副本）

做法见 [`docs/掉线自愈验证.md`](docs/掉线自愈验证.md)，工具是 `bench/failover.py`。

实测一次（`node-2` 被杀，它当时持有两个双副本订单的分片）：

```
detected_ms     34274    # 从 kill 到控制面判定分片不再由 node-2 服务
node_offline_ms 34274    # 节点被判 offline
recovered_ms    34274    # 分片已在 node-1 上重新 active
```

- 34 s 基本就是**心跳超时 30 s + 调度器一轮 tick**，没有额外的兜底延迟；
- 迁移是**重排所有权**：分片从 `node-2` 改判给 `node-1` 并回到 `active`，客户端重新连上来就能继续写；
- **节点内存里的数据不会跟着搬**。kvstore 是纯内存，被杀节点上的键在迁移后读不到（GET 返回空）。所以"自愈"目前救的是**可用性**（分片有人接、能继续写），不是**数据**。要救数据得等持久化/副本同步那一阶段，现在版本明确不提供。

## 复现

```powershell
# 1) 起三节点
powershell -ExecutionPolicy Bypass -File scripts\quickstart.ps1 -BuildLocal -Nodes 3 -Force

# 2) 延迟
python bench\bench.py latency --addr 127.0.0.1:16380 --ops 5000
python bench\bench.py latency --addr 127.0.0.1:6379 --auth <tenant-token> --ops 5000

# 3) 吞吐
python bench\bench.py throughput --addr 127.0.0.1:16380 --threads 20 --seconds 10
python bench\bench.py throughput --addr 127.0.0.1:6379 --auth <tenant-token> --threads 20 --seconds 10

# 4) 自愈（pid 用 Get-NetTCPConnection -State Listen -LocalPort 16382 查）
python bench\failover.py --api http://127.0.0.1:8080 --token <admin-token> --node-id node-3 --pid <pid>
```

`tenant-token` / `admin-token` 由 `quickstart.ps1` 打印，也写在 `quickstart\config.json` 的 `admin_token` 与 demo 租户令牌里。
