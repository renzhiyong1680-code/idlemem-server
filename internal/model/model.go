// Package model 定义调度控制面的核心数据模型。
package model

import "time"

// NodeStatus 节点状态。
type NodeStatus string

const (
	// NodeOnline 在线并贡献中。
	NodeOnline NodeStatus = "online"
	// NodeSuspended 在线但本机高负载，暂停贡献。
	NodeSuspended NodeStatus = "suspended"
	// NodeOffline 心跳超时，判定离线。
	NodeOffline NodeStatus = "offline"
	// NodePending 已注册但尚未收到首次心跳。
	NodePending NodeStatus = "pending"
)

// Node 一个贡献节点（对应 Agent 端）。
type Node struct {
	ID            string     `json:"id"`
	IP            string     `json:"ip"`
	Version       string     `json:"version"`
	Status        NodeStatus `json:"status"`
	CapacityBytes uint64     `json:"capacity_bytes"`  // 当前可贡献容量
	UsedBytes     uint64     `json:"used_bytes"`      // KV 节点实际占用
	SystemUsedPct float64    `json:"system_used_pct"` // 本机内存使用率
	TotalBytes    uint64     `json:"total_bytes"`     // 本机物理内存
	Score         float64    `json:"score"`           // 信誉分
	HeartbeatCnt  uint64     `json:"heartbeat_cnt"`   // 连续心跳次数
	KVAddr        string     `json:"kv_addr"`         // 节点 KV 服务监听地址（网关转发用）
	LastSeen      time.Time  `json:"last_seen"`
	FirstSeen     time.Time  `json:"first_seen"`
}

// Tenant 租户（企业客户）。
type Tenant struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Token      string    `json:"token"`
	QuotaBytes uint64    `json:"quota_bytes"` // 配额上限，0 不限制
	CreatedAt  time.Time `json:"created_at"`
}

// OrderStatus 订单状态。
type OrderStatus string

const (
	OrderActive   OrderStatus = "active"
	OrderExpired  OrderStatus = "expired"
	OrderDegraded OrderStatus = "degraded" // 部分分片节点离线
)

// Order 内存资源订单（客户购买的一定容量缓存服务）。
type Order struct {
	ID            string      `json:"id"`
	TenantID      string      `json:"tenant_id"`
	CapacityBytes uint64      `json:"capacity_bytes"`
	Replicas      int         `json:"replicas"` // 副本数
	Status        OrderStatus `json:"status"`
	CreatedAt     time.Time   `json:"created_at"`
	ExpireAt      time.Time   `json:"expire_at"` // 空值表示长期
}

// ShardStatus 分片状态。
type ShardStatus string

const (
	ShardActive    ShardStatus = "active"
	ShardDegraded  ShardStatus = "degraded"
	ShardMigrating ShardStatus = "migrating"
	ShardLost      ShardStatus = "lost"
)

// Shard 一个分片：订单容量切分到具体节点的分配单元。
type Shard struct {
	ID         string      `json:"id"`
	OrderID    string      `json:"order_id"`
	TenantID   string      `json:"tenant_id"`
	NodeID     string      `json:"node_id"`
	KeyPrefix  string      `json:"key_prefix"` // 归属该分片的 key 前缀
	Bytes      uint64      `json:"bytes"`      // 分片容量
	Status     ShardStatus `json:"status"`
	AssignedAt time.Time   `json:"assigned_at"`
}

// UsageScope 计量对象范围。
type UsageScope string

const (
	// UsageScopeNode 节点维度：按节点在线可贡献容量 × 时长累计（供给能力指标）。
	UsageScopeNode UsageScope = "node"
	// UsageScopeNodeUsed 节点承载维度：按节点实际承载的活跃分片容量 × 时长累计（供给结算依据）。
	UsageScopeNodeUsed UsageScope = "node_used"
	// UsageScopeTenant 租户维度：按租户活跃分片容量 × 时长累计（计费依据）。
	UsageScopeTenant UsageScope = "tenant"
)

// UsageRecord 计量累计记录（GB·h）。
// 供计费使用：如售价 0.0004 元/GB·h，则费用 = GBHours × 单价。
type UsageRecord struct {
	Scope     UsageScope `json:"scope"`
	ID        string     `json:"id"`         // 节点 ID 或租户 ID
	GBHours   float64    `json:"gb_hours"`   // 累计 GB·h
	UpdatedAt time.Time  `json:"updated_at"` // 最近一次采样时间
}
