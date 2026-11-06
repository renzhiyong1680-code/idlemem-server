# 更新日志

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

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
