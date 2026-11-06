// Package meter 提供 GB·h 计量：周期采样在线容量并累计，供计费与供给结算。
//
// 计量口径：
//   - 节点维度：在线节点的可贡献容量（CapacityBytes）× 在线时长（供给能力指标）；
//   - 节点承载维度：节点上活跃分片容量（Shard.Bytes）× 时长（供给结算依据，只对已售出容量付费）；
//   - 租户维度：租户活跃分片容量（Shard.Bytes）× 时长（客户计费依据）；
//   - 单位换算：GB = 1024^3 字节，h = 3600 秒。
package meter

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/renzhiyong1680-code/idlemem-server/internal/model"
	"github.com/renzhiyong1680-code/idlemem-server/internal/store"
)

// GB 1GB 字节数。
const GB = 1024 * 1024 * 1024

// Meter 计量器：按固定间隔采样并累加 GB·h 到持久化存储。
type Meter struct {
	store    *store.Store
	interval time.Duration
	logger   *log.Logger
	mu       sync.Mutex // 保护 tick 期间的累加（tick 与外部读不冲突，防御重复调用）
}

// New 创建计量器。interval 为采样周期（推荐 60s；越小粒度越细、写入越频繁）。
func New(st *store.Store, interval time.Duration, logger *log.Logger) *Meter {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &Meter{store: st, interval: interval, logger: logger}
}

// Run 启动周期计量，直到 ctx 取消。
func (m *Meter) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	m.logger.Printf("meter: gb-hour metering started (interval=%s)", m.interval)
	m.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

// tick 采样一次：累加节点与租户的 GB·h。
func (m *Meter) tick() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 本次采样对应的时长（小时）。
	hours := m.interval.Seconds() / 3600.0

	// 节点维度：在线节点的可贡献容量。
	nodes, err := m.store.ListNodes()
	if err != nil {
		m.logger.Printf("meter: list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		if n.Status != model.NodeOnline {
			continue
		}
		m.accumulate(model.UsageScopeNode, n.ID, float64(n.CapacityBytes), hours)
	}

	// 租户维度：活跃分片容量（按租户归并）；节点承载维度：活跃分片容量（按节点归并，供供给结算）。
	shards, err := m.store.ListShards()
	if err != nil {
		m.logger.Printf("meter: list shards: %v", err)
		return
	}
	byTenant := map[string]uint64{}
	byNode := map[string]uint64{}
	for _, sh := range shards {
		if sh.Status != model.ShardActive {
			continue
		}
		byTenant[sh.TenantID] += sh.Bytes
		if sh.NodeID != "" {
			byNode[sh.NodeID] += sh.Bytes
		}
	}
	for tid, bytes := range byTenant {
		m.accumulate(model.UsageScopeTenant, tid, float64(bytes), hours)
	}
	// 供给结算只对节点实际承载的容量付费：供大于求时不产生倒挂支出。
	for nid, bytes := range byNode {
		m.accumulate(model.UsageScopeNodeUsed, nid, float64(bytes), hours)
	}
}

// accumulate 把「容量 bytes 保持 hours 小时」累加到对应记录。
func (m *Meter) accumulate(scope model.UsageScope, id string, bytes, hours float64) {
	rec, err := m.store.GetUsage(scope, id)
	if err != nil {
		m.logger.Printf("meter: get usage %s/%s: %v", scope, id, err)
		return
	}
	if rec == nil {
		rec = &model.UsageRecord{Scope: scope, ID: id}
	}
	rec.GBHours += bytes / GB * hours
	rec.UpdatedAt = time.Now()
	if err := m.store.UpsertUsage(rec); err != nil {
		m.logger.Printf("meter: upsert usage %s/%s: %v", scope, id, err)
	}
}

// Snapshot 返回某范围的全部计量记录（供 API 查询）。
func (m *Meter) Snapshot(scope model.UsageScope) ([]*model.UsageRecord, error) {
	return m.store.ListUsage(scope)
}
