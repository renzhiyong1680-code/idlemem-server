# 更新日志

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [未发布]

### 新增

- `NOTICE`：著作权归属声明。明确三条授权路径（AGPL-3.0 开源 / 商业闭源另签 / 贡献走 CLA），并写明软著登记材料与身份证明**不随仓库分发、不进入版本历史**。该文件不含任何身份证件号码、住址、电话。

## [v0.1.2] - 2026-11-10

> 本版**没有 Go 源码变更**（`internal/` 未动），服务端二进制与 `v0.1.1` 等价；本版的价值是把多节点起步脚本、Benchmark 与自愈证据、文档口径一起纳入版本号。

### 新增

- `bench/bench.py`：零依赖 RESP 压测客户端（延迟分位与多线程吞吐），不需要 `redis-py`
- `bench/failover.py`：掉线自愈计时脚本，杀节点前后全程轮询分片状态，输出检测/恢复耗时与时间线
- `BENCHMARK.md`：本机实测的延迟、吞吐、自愈耗时，含环境、原始数字与复现命令
- `docs/掉线自愈验证.md`：一次真实节点掉线的完整命令、原始输出与已知边界
- `scripts/quickstart.ps1` 的 `-Nodes N`：一条命令起多节点集群（第 i 个节点用 `16380 + i`，各 agent 独立 node id 与日志）
- `CLA.md`：贡献者许可协议（中英双语十节），贡献者保留著作权，仅授予项目方在开源与商业授权两种模式下的使用许可
- `CONTRIBUTING.md`、`SECURITY.md`、`.github/ISSUE_TEMPLATE/`：贡献流程、漏洞报告通道与 Issue 模板
- README 顶部增加三仓导航与项目介绍入口
- `scripts/quickstart.ps1`：Windows 一条命令起整套本地环境（下载 `v0.1.1` 二进制 → 生成令牌 → 起控制面与一个 Agent → 建 demo 租户与订单 → 打印 `redis-cli` 用法），`-Stop` 可完整回收
- `scripts/quickstart.sh`：Linux / macOS 的同流程版本
- `Dockerfile`（`server` / `agent` 两个 target）、`docker-compose.yml`、`scripts/docker-*.sh`：容器化起步，Agent 按 `go.mod` 锁定的 core 版本构建
- `docs/assets/architecture.svg`：三仓协作架构图，README 的「架构」段改为引用该图
- README「架构」段补一句 AI 基站（AI-RAN）场景落位，链接 core 仓的 `ai-ran-architecture.svg`

### 变更

- `quickstart.ps1 -Force` 现在会清掉**所有** agent 的 KV 端口（原来只清第一个，`-Nodes 3` 会被 16381/16382 的残留占住），并把上一轮元数据库归档为 `idlemem-server.db.bak-<时间戳>` 后再起新环境，避免旧节点记录把 `summary` 显示成 `3 online / 4 total`
- README「快速起步」补多节点用法与自愈验证方法；分片端点统一写作 `/api/v1/shard`（单数）
- 核心库依赖由 `idlemem-core v0.1.0` 升级为 `v0.1.1`（两版代码一致，仅许可证变更）
- README「快速开始」重写为「快速起步」，按门槛给出四种方式（一条命令 / Linux 脚本 / Docker Compose / 源码构建）
- README 中核心库版本表述由 `v0.1.0` 更正为 `v0.1.1`

### 修复

- `quickstart.ps1` 的等待循环用 `$nodeList` 承接节点数组，但 PowerShell 变量名不区分大小写，原名 `$nodes` 会把参数 `$Nodes`（要起几个节点）覆盖成数组，判断永远不成立，导致三个节点都已在线仍打印 `only 0/3 agent(s) online`
- 同一台机器起多个 agent 时，默认 node id 由主机名 + MAC 生成，三个 agent 全撞成同一个 id，控制面只认到 1 个节点；现在脚本给每个 agent 显式传 `-id node-i`
- 出包 job 检出的核心库源码未指定版本，默认取 core 默认分支的最新提交，一旦 core 有新提交而本仓未升级依赖，打出的 `agent` 会与 `server` 版本错配。现在先从 `go.mod` 解析核心库版本，再按该 tag 检出源码

## [v0.1.1] - 2026-11-06

仅许可证变更，代码与 `v0.1.0` 完全一致。

### 变更

- 许可证由 MIT 变更为 AGPL-3.0（GNU Affero General Public License v3.0），保留后续商业授权选项；
  `v0.1.0` 及更早版本仍按 MIT 发布，该授权不可撤销。
- 新增 `COMMERCIAL.md`：商业授权的适用场景、授权形式与联系方式

## [v0.1.0] - 2026-11-06

首个公开版本：闲存云调度控制面（MVP 单实例）。企业客户用任意 Redis 客户端接入，
节点主人用 `xiancun-agent` 贡献闲置内存。

### 控制面与调度

- 节点注册与心跳：在线/挂起/离线状态、容量与信誉分、掉线检测与自愈迁移
- 订单与分片：按容量售卖、按副本数切分、到期自动回收
- 调度容量按**活跃分片的承诺量**判定，`lost` 分片不再占承诺，避免节点重启后超卖
- 下单失败（无可用节点）整单回滚，不占租户配额，客户可安全重试
- 副本数缺省取配置默认值，响应返回实际生效值

### Redis 协议网关

- 租户隔离 + 键空间隔离 + 按订单容量做写入闸门（覆盖写按净增量判定）
- 网关本地缓存 + 按分片转发到节点：多副本写、本地 miss 回源、节点全不可达时降级为本地服务
  （Best-Effort 语义）
- 命令集：`PING/QUIT/AUTH/SET[EX]/SETEX/GETSET/APPEND/STRLEN/TYPE/GET/DEL/EXISTS/MGET/MSET/INCR/EXPIRE/TTL/INFO`

### 计量与计费

- GB·h 计量按三档口径分别采样累加：`node`（在线可贡献容量，仅参考，不结算）、
  `node_used`（节点承载活跃分片，供给结算依据）、`tenant`（租户活跃分片，客户计费依据）
- 供给结算与平台佣金（客户 0.0004 元/GB·h，供给 0.00032，佣金 0.00008）
- 账期发票与结算水位、租户自助账单、CSV 对账导出

### 多租户治理

- 配额校验、租户停服/恢复（对已登录长连接最长 10 秒生效）
- 客户侧令牌与运维侧令牌分离：管理面 `X-Admin-Token`/`Bearer`，节点侧 `X-Node-Token`

### 可观测与运维

- Prometheus `/metrics`、`/api/v1/summary`（含 `nodes.committed`，区分"卖出多少"与"实际用了多少"）
- `scripts/healthcheck.ps1`：六项巡检，退出码 0 正常 / 1 有关注项 / 2 有失败
- `docs/runbook.md`：每日巡检、常见故障处理、发布流程、值班边界
- `scripts/release.ps1`：`server|cli|agent × 五平台` 共 15 个产物 + `SHA256SUMS`
- 备份导出/恢复（控制面元数据）

### 接入工具

- `xiancun-cli`：客户与运维命令行
- 官方 Go SDK（`client/`）与两个零依赖示例（`examples/client`、`examples/sdk`）

### 工程

- `.gitattributes` 固定 LF 行尾
- CI：每次提交 `vet` + `test` + `build`；打 `v*` tag 时交叉编译并上传产物

### 已知限制

- 调度器为单实例，多控制面水平扩展属下一阶段
- 控制面元数据存 buntdb 单文件，未做多副本
- 迁移依赖 `../idlemem-core` 同级目录（本地联合开发形态），正式发布按版本号依赖
