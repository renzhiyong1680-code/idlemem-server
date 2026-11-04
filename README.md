# 闲存云调度控制面（idlemem-server）

闲存云（XianCun Cloud）分布式闲置内存缓存 P2P 平台的**平台后端 / 调度控制面**。
与 Agent 核心库 [idlemem-core](../idlemem-core) 配套使用：Agent 上报心跳与容量，
控制面负责节点注册、调度分配、掉线自愈，并通过 Redis 协议网关向企业客户提供服务。

> ⚠️ 当前为 **MVP 单实例** 版本：调度器为单实例，多控制面水平扩展属于下一阶段演进。

## 架构

```
企业客户 (redis-cli / go-redis / 任意 Redis 客户端)
        │  Redis 协议 (AUTH 租户 Token)
        ▼
┌─────────────────┐    ┌──────────────────────────────────┐
│  REST 管理 API  │    │   Redis 协议网关 (redcon)        │
│  :8080          │    │   :6379                           │
│ 节点注册/心跳    │    │  租户隔离 · 键空间隔离 · 限流    │
│ 租户/订单管理    │    │  本地缓存 + 按分片转发到节点     │
└────────┬────────┘    └────────────────┬─────────────────┘
         │                              │
         ▼                              ▼
┌───────────────────────────────────────────────────────────┐
│  调度器 Scheduler      ┌─────────────┐   Agent 节点       │
│   节点信誉分            │  KV 数据面   │   (idlemem-core)   │
│   订单分片分配          │  转发+回源   │   Redis 协议端口   │
│   掉线检测 → 自愈迁移   └──────┬──────┘   :16380            │
└──────────────┬────────────────┴────────────┘
               ▼
┌───────────────────────────────────────────────────────────┐
│  持久化存储 (buntdb 嵌入式 KV)  节点 / 租户 / 订单 / 分片  │
└───────────────────────────────────────────────────────────┘
```

### KV 数据面（网关 → 节点转发）

网关在本地缓存之外，还会把数据**转发到承载分片的 Agent 节点**：

- **SET / DEL / EXPIRE**：写入本地缓存，同时转发到该租户所有在线分片节点（多副本）；
- **GET / TTL**：本地命中直接返回；本地 miss 时**回源到任一在线分片节点**；
- 节点地址来自 Agent 心跳上报的 `kv_addr`（节点 `KVAddr` 字段）；
- 节点全部不可达时，命令仍基于本地缓存正常服务（Best-Effort），转发失败记日志。

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
    "reported_at": "2026-11-03T15:00:00+08:00",
    "kv_addr": "0.0.0.0:16380"
  }'
```

也可以直接运行 Agent 外壳（会自动注册 + 心跳 + KV 端口）：

```bash
# 在 Agent 侧
go run ../idlemem-core/cmd/xiancun-agent -server http://127.0.0.1:8080 -kv :16380
```

### 第二步：创建租户（企业客户）

```bash
curl -X POST http://127.0.0.1:8080/api/v1/tenants \
  -H 'Content-Type: application/json' \
  -d '{"name": "demo-tenant"}'
# => {"id":"...","name":"demo-tenant","token":"<自动生成32位token>",...}
```

> Token 未提供时自动生成（32 位十六进制），用于网关 AUTH 鉴权。

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
| GET | `/api/v1/usage/nodes` | 节点维度 GB·h 计量（供给结算依据） |
| GET | `/api/v1/usage/tenants` | 租户维度 GB·h 计量（客户计费依据） |
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
| `meter_interval` | `60s` | GB·h 计量采样周期（0=关闭计量） |

配置文件示例 `config.json`：

```json
{
  "api_addr": ":8080",
  "gateway_addr": ":6379",
  "db_path": "idlemem-server.db",
  "heartbeat_timeout": 30000000000,
  "heartbeat_evict_after": 86400000000000,
  "default_replicas": 1,
  "default_ttl": 0,
  "meter_interval": 60000000000
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

## GB·h 计量（计费/结算）

后台按 `meter_interval`（默认 60s）周期采样并累计 **GB·h**（1 GB 保持 1 小时）：

- **节点维度**：在线节点的可贡献容量 × 在线时长 → 供给者结算依据；
- **租户维度**：活跃分片容量 × 时长 → 企业客户计费依据；
- 查询：`GET /api/v1/usage/nodes`、`GET /api/v1/usage/tenants`。

商业口径示例（见《闲存云商业计划书》）：

| 角色 | 单价（元/GB·h） | 计量来源 |
|---|---|---|
| 企业客户 | 0.0004 | `usage/tenants` |
| 供给者结算 | 0.00032 | `usage/nodes` |
| 平台佣金 | 0.00008（≈20%） | 差值 |

计量记录持久化于 `idlemem-server.db`（`usage:*` 键空间），进程重启不丢失。

## 目录结构

```
idlemem-server/
├── cmd/server/            # 主入口（装配 + 后台扫描循环 + 计量）
├── internal/
│   ├── config/            # 配置加载
│   ├── model/             # 数据模型（节点/租户/订单/分片/计量）
│   ├── store/             # buntdb 持久化层
│   ├── node/              # 节点注册表（注册/心跳/超时判定/清扫）
│   ├── scheduler/         # 调度器（打分/分配/自愈）
│   ├── meter/             # GB·h 计量（周期采样累加，计费/结算依据）
│   ├── api/               # REST 管理 API
│   └── gateway/           # Redis 协议网关（redcon）+ 节点转发（forward.go）
└── go.mod
```

## 部署

云主机（Linux/systemd）一键部署见 [`deploy/`](deploy/)：

```bash
sudo ./deploy/install-server.sh --binary ./bin/idlemem-server
```

包含 systemd 单元、生产配置模板与部署/卸载说明。

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
