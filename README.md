# 闲存云调度控制面（idlemem-server）

闲存云（XianCun Cloud）分布式闲置内存缓存 P2P 平台的**平台后端 / 调度控制面**。
与 Agent 核心库 [idlemem-core](../idlemem-core) 配套使用：Agent 上报心跳与容量，
控制面负责节点注册、调度分配、掉线自愈，并通过 Redis 协议网关向企业客户提供服务。

> ⚠️ 当前为 **MVP 单实例** 版本：调度器为单实例，网关数据存于内存缓存
> （进程重启即清空，符合"可丢失缓存"定位）。多控制面水平扩展、网关按 key
> 转发到 Agent 节点，属于下一阶段演进。

## 架构

```
企业客户 (redis-cli / go-redis / 任意 Redis 客户端)
        │  Redis 协议 (AUTH 租户 Token)
        ▼
┌─────────────────┐    ┌──────────────────────────────────┐
│  REST 管理 API  │    │   Redis 协议网关 (redcon)        │
│  :8080          │    │   :6379                           │
│ 节点注册/心跳    │    │  租户隔离 · 键空间隔离 · 限流    │
│ 租户/订单管理    │    │  内存缓存 (buntdb :memory:)      │
└────────┬────────┘    └────────────────┬─────────────────┘
         │                              │
         ▼                              │
┌───────────────────────────────────────▼──────────────────┐
│  调度器 Scheduler                                        │
│   节点信誉分 → 订单分片分配 → 掉线检测 → 分片自愈迁移     │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌───────────────────────────────────────────────────────────┐
│  持久化存储 (buntdb 嵌入式 KV)                            │
│   节点 / 租户 / 订单 / 分片                                │
└───────────────────────────────────────────────────────────┘

Agent (idlemem-core) ── 心跳 POST /api/v1/nodes/heartbeat ──► 控制面
```

## 快速开始

前置：Go 1.22+（本项目使用 Go 1.22 的 `http.ServeMux` 方法路由）。

```bash
# 1. 构建
go build -o bin/idlemem-server ./cmd/server

# 2. 启动（默认配置：API :8080，网关 :6379，数据文件 idlemem-server.db）
./bin/idlemem-server
# 或指定配置文件
./bin/idlemem-server -config config.json
```

### 第一步：注册并心跳一个 Agent 节点

```bash
curl -X POST http://127.0.0.1:8080/api/v1/nodes/heartbeat \
  -H 'Content-Type: application/json' \
  -d '{
    "node_id": "node-1",
    "version": "0.1.0",
    "status": "online",
    "capacity_bytes": 8589934592,
    "used_bytes": 0,
    "system_used_pct": 52.2,
    "total_bytes": 34359738368,
    "uptime_seconds": 3600,
    "reported_at": "2026-11-03T15:00:00+08:00"
  }'
```

### 第二步：创建租户（企业客户）

```bash
curl -X POST http://127.0.0.1:8080/api/v1/tenants \
  -H 'Content-Type: application/json' \
  -d '{"name": "demo-tenant", "token": "secret-token"}'
# => {"id":"...","name":"demo-tenant","token":"secret-token",...}
```

### 第三步：创建订单（购买缓存容量，自动分配分片）

```bash
curl -X POST http://127.0.0.1:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id": "<上一步返回的id>", "capacity_bytes": 2147483648, "replicas": 1}'
```

### 第四步：用标准 Redis 客户端访问缓存

```bash
redis-cli -p 6379
> AUTH secret-token
OK
> SET user:1:profile '{"name":"alice"}'
OK
> GET user:1:profile
{"name":"alice"}
> TTL user:1:profile
-1
> EXPIRE user:1:profile 3600
1
> QUIT
```

支持命令：`AUTH` `PING` `SET [EX]` `GET` `DEL` `EXPIRE` `TTL` `INFO` `QUIT`。

## REST API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/nodes/register` | 注册节点（幂等） |
| POST | `/api/v1/nodes/heartbeat` | 心跳上报（未注册会自动注册） |
| GET | `/api/v1/nodes` | 节点列表 |
| POST | `/api/v1/tenants` | 创建租户 |
| GET | `/api/v1/tenants` | 租户列表 |
| POST | `/api/v1/orders` | 创建订单并分配分片（无可用节点返回 503） |
| GET | `/api/v1/orders` | 订单列表 |
| GET | `/api/v1/shard` | 分片列表 |
| GET | `/api/v1/summary` | 平台概览（节点/容量/订单/分片统计） |
| GET | `/healthz` | 健康检查 |

## 配置

| 字段 | 默认值 | 说明 |
|---|---|---|
| `api_addr` | `:8080` | REST API 监听地址 |
| `gateway_addr` | `:6379` | Redis 网关监听地址 |
| `db_path` | `idlemem-server.db` | 持久化数据库文件 |
| `heartbeat_timeout` | `30s` | 心跳超时（超过判定离线） |
| `heartbeat_evict_after` | `24h` | 离线节点清理周期 |
| `default_replicas` | `1` | 默认分片副本数 |
| `default_ttl` | `0` | 网关 KV 默认 TTL（0=不过期） |

配置文件示例 `config.json`：

```json
{
  "api_addr": ":8080",
  "gateway_addr": ":6379",
  "db_path": "idlemem-server.db",
  "heartbeat_timeout": 30000000000,
  "heartbeat_evict_after": 86400000000000,
  "default_replicas": 1,
  "default_ttl": 0
}
```

> 时长字段使用 Go `time.Duration` 的纳秒整数（如 `30s` = `30000000000`）。

## 调度与自愈机制

- **节点信誉分**：`心跳次数 + 在线分钟×0.1 + 在线且有空闲容量 +10`，后续可扩展稳定性评估。
- **分片分配**：订单容量按副本数切分，按信誉分降序选择可用节点（online、未超时、有富余）。
- **掉线自愈**：后台循环每 `心跳超时/2` 扫描一次：
  1. 心跳超时 → 标记节点 `offline`；
  2. 分片所在节点离线 → 尝试迁移到其他在线节点（`active`）；
  3. 无可用替换节点 → 分片置为 `lost`；
  4. 离线超过 `evict_after` → 节点回收。

## 目录结构

```
idlemem-server/
├── cmd/server/            # 主入口（装配 + 后台扫描循环）
├── internal/
│   ├── config/            # 配置加载
│   ├── model/             # 数据模型（节点/租户/订单/分片）
│   ├── store/             # buntdb 持久化层
│   ├── node/              # 节点注册表（注册/心跳/超时判定/清扫）
│   ├── scheduler/         # 调度器（打分/分配/自愈）
│   ├── api/               # REST 管理 API
│   └── gateway/           # Redis 协议网关（redcon）
└── go.mod
```

## 开发

```bash
go vet ./...
go test ./...          # 单元测试（含端到端 API/网关测试）
go test -race ./...    # 竞态检测
```

## 开源说明

- 模块路径为占位 `github.com/xiancuncloud/idlemem-server`；
- 本地通过 `replace github.com/xiancuncloud/idlemem-core => ../idlemem-core` 引用核心库；
- 上传 GitHub 前请将模块路径、`replace` 与 git 身份改为自己的仓库信息。

## License

MIT
