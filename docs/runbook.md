# 运维手册（Runbook）

面向"接了这个平台每天要照看它的人"。所有命令都来自仓库里已有的工具，不依赖外部系统。

## 一、每日巡检

```powershell
# Windows
.\scripts\healthcheck.ps1 -Server http://127.0.0.1:8080 -AdminToken $env:XIANCUN_ADMIN_TOKEN -BackupDir D:\backup

# Linux（等价，直接 curl）
curl -sf $S/healthz
curl -s -H "X-Admin-Token: $T" $S/api/v1/summary
curl -s -H "X-Admin-Token: $T" $S/api/v1/orders
curl -s -H "X-Admin-Token: $T" $S/api/v1/tenants
curl -s $S/metrics | grep '^xiancun_'
```

退出码：`0` 全部正常、`1` 有需关注项、`2` 有失败项。把它放进 cron / 计划任务，
返回值非 0 就发一条告警，比每天人工点一遍可靠。

巡检覆盖六件事：控制面存活、节点在线率、订单临近到期、租户停服、计量指标产出、备份新鲜度。

## 二、常见故障处理

### 控制面起不来 / 探活失败

1. `GET /healthz` 是否 200；不是就先看进程和日志（端口占用是最常见原因）。
2. 配置里 `db_path` 是相对路径，**以进程工作目录为基准**——从服务方式启动时容易指错，
   导致读到一份空库、看到"数据全没了"。先用绝对路径确认。
3. 令牌开了校验但没给令牌，管理接口会一律 401；`/healthz` 不受影响，所以"探活正常但接口全 401"基本就是令牌问题。

### 节点全掉线 / 在线率为 0

1. 先在节点本机跑 `xiancun-agent -check`，它会一次性验证内存、端口、控制面心跳与令牌。
2. 端口检查用的是"先 dial 再 Listen"——Windows 上单纯 Listen 会把被占用误判为成功。
3. 心跳超时默认 30s，节点重启后要等下一次心跳才会重新 online，不用立刻判定为故障。
4. 节点侧 NAT/端口映射无法在节点本机自证，必须在另一台机器上用
   `xiancun-client -addr <公网地址>:<端口> ping` 验证，能返回 `PONG` 才算通。

### 客户写入报 `capacity exceeded`

这是容量闸门在起作用，口径是"活跃订单容量之和，且不超过 `quota_bytes`"：

1. `xiancun-cli order list` 看该租户还有多少 active 订单、是否已到期被回收；
2. 到期订单会回收分片并降低上限，客户需要重新下单；
3. 覆盖写按净增量算，同一 key 反复改写不会翻倍占用；`DEL` 立即释放。

### 某张订单状态异常

- `expired`：已到期回收，或**下单时没有可用节点被自动回滚**（返回 503 且不占配额，可安全重试）；
- `degraded`：部分分片节点离线，调度器会尝试补副本；长期不恢复说明供给不足。

### 账单对不上

1. 三档计量口径不同，别混用：
   - `node`：在线可贡献容量 × 时长，只作供给能力参考，**不结算**；
   - `node_used`：节点实际承载活跃分片 × 时长，**供给结算依据**；
   - `tenant`：租户活跃分片 × 时长，**客户计费依据**。
2. 出账是"累计用量 − 已结算水位"，重复出账返回 409 属正常保护；
3. 对账明细用 `xiancun-cli billing export -o 对账.csv`（UTF-8 BOM，Excel 直接打开）。

### 数据损坏 / 误操作

```bash
xiancun-cli -server $S -admin-token $T backup restore -f backup.json
```

恢复是 Upsert 语义、幂等，不删现有记录；格式版本不符会直接拒绝。
注意备份**不含**节点上的 KV 数据与网关本地缓存——缓存按 Best-Effort 语义重建，不做数据迁移。

## 三、发布流程

```powershell
.\scripts\release.ps1 -Version 0.1.0        # 产出 15 个二进制 + SHA256SUMS
```

1. 目标机先 `sha256sum -c SHA256SUMS` 核对完整性；
2. 控制面与 Agent 分开升级，先升 Agent（向后兼容），再升控制面；
3. 升级前 `backup export` 一次，升级后 `healthcheck.ps1` 跑一遍；
4. Windows 托盘/macOS 菜单栏需要各自平台带 CGO 构建，不能用交叉编译产物。

## 四、值班时需要知道的边界

- 节点上的 KV 数据是缓存语义：Best-Effort、可丢、不做持久化承诺；
- `model.Node.UsedBytes` 同时被调度累加和 Agent 心跳写入，节点重启后心跳会把它上报为真实占用，
  与调度侧的累加值可能短暂不一致——判断容量时以订单与分片为准；
- 停服/改配额对已登录长连接的最长生效延迟是 10 秒（租户快照 TTL）。
