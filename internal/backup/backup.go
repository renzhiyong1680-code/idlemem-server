// Package backup 导出与恢复控制面元数据。
//
// 覆盖范围：节点、租户、订单、分片、三档计量累计、账期结算水位、期账单。
// 恢复采用 Upsert 语义（不删除现有记录），用于灾难重建、跨环境搬移与回归演练。
//
// 注意：网关本地缓存与节点上的 KV 数据不在备份范围内——缓存本身是
// Best-Effort 语义，恢复后由 Agent 侧 TTL/淘汰自然收敛。
package backup

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/renzhiyong1680-code/idlemem-server/internal/model"
	"github.com/renzhiyong1680-code/idlemem-server/internal/store"
)

// FormatVersion 备份文件格式版本。
const FormatVersion = "1"

// Snapshot 一份完整备份。
type Snapshot struct {
	Format    string                     `json:"format"`
	CreatedAt time.Time                  `json:"created_at"`
	Nodes     []*model.Node              `json:"nodes"`
	Tenants   []*model.Tenant            `json:"tenants"`
	Orders    []*model.Order             `json:"orders"`
	Shards    []*model.Shard             `json:"shards"`
	Usage     []*model.UsageRecord       `json:"usage"`
	Baselines map[string]store.Baseline  `json:"baselines"` // key = "<scope>:<id>"
	Invoices  map[string]json.RawMessage `json:"invoices"`  // 期账单 ID → 原始 JSON
}

// Result 恢复结果统计。
type Result struct {
	Nodes     int `json:"nodes"`
	Tenants   int `json:"tenants"`
	Orders    int `json:"orders"`
	Shards    int `json:"shards"`
	Usage     int `json:"usage"`
	Baselines int `json:"baselines"`
	Invoices  int `json:"invoices"`
}

// Export 读取全部控制面元数据。
func Export(st *store.Store, now time.Time) (*Snapshot, error) {
	s := &Snapshot{
		Format:    FormatVersion,
		CreatedAt: now,
		Baselines: make(map[string]store.Baseline),
		Invoices:  make(map[string]json.RawMessage),
	}

	nodes, err := st.ListNodes()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	tenants, err := st.ListTenants()
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	orders, err := st.ListOrders()
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	shards, err := st.ListShards()
	if err != nil {
		return nil, fmt.Errorf("list shards: %w", err)
	}
	s.Nodes, s.Tenants, s.Orders, s.Shards = nodes, tenants, orders, shards

	for _, scope := range []model.UsageScope{
		model.UsageScopeNode, model.UsageScopeNodeUsed, model.UsageScopeTenant,
	} {
		recs, err := st.ListUsage(scope)
		if err != nil {
			return nil, fmt.Errorf("list usage %s: %w", scope, err)
		}
		s.Usage = append(s.Usage, recs...)

		bl, err := st.ListBaselines(scope)
		if err != nil {
			return nil, fmt.Errorf("list baselines %s: %w", scope, err)
		}
		for id, b := range bl {
			s.Baselines[string(scope)+":"+id] = b
		}
	}

	invoices, err := st.ListInvoiceMap()
	if err != nil {
		return nil, fmt.Errorf("list invoices: %w", err)
	}
	for id, raw := range invoices {
		s.Invoices[id] = json.RawMessage(raw)
	}
	return s, nil
}

// Restore 将备份写回（Upsert 语义，不删除现有记录）。
func Restore(st *store.Store, s *Snapshot) (Result, error) {
	var r Result
	if s.Format != FormatVersion {
		return r, fmt.Errorf("unsupported backup format %q (want %q)", s.Format, FormatVersion)
	}

	for _, n := range s.Nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if err := st.UpsertNode(n); err != nil {
			return r, fmt.Errorf("restore node %s: %w", n.ID, err)
		}
		r.Nodes++
	}
	for _, t := range s.Tenants {
		if t == nil || t.ID == "" {
			continue
		}
		if err := st.UpsertTenant(t); err != nil {
			return r, fmt.Errorf("restore tenant %s: %w", t.ID, err)
		}
		r.Tenants++
	}
	for _, o := range s.Orders {
		if o == nil || o.ID == "" {
			continue
		}
		if err := st.UpsertOrder(o); err != nil {
			return r, fmt.Errorf("restore order %s: %w", o.ID, err)
		}
		r.Orders++
	}
	for _, sh := range s.Shards {
		if sh == nil || sh.ID == "" {
			continue
		}
		if err := st.UpsertShard(sh); err != nil {
			return r, fmt.Errorf("restore shard %s: %w", sh.ID, err)
		}
		r.Shards++
	}
	for _, u := range s.Usage {
		if u == nil || u.ID == "" {
			continue
		}
		if err := st.UpsertUsage(u); err != nil {
			return r, fmt.Errorf("restore usage %s/%s: %w", u.Scope, u.ID, err)
		}
		r.Usage++
	}
	if len(s.Baselines) > 0 {
		if err := st.SetBaselines(s.Baselines); err != nil {
			return r, fmt.Errorf("restore baselines: %w", err)
		}
		r.Baselines = len(s.Baselines)
	}
	for id, raw := range s.Invoices {
		if err := st.UpsertInvoice(id, []byte(raw)); err != nil {
			return r, fmt.Errorf("restore invoice %s: %w", id, err)
		}
		r.Invoices++
	}
	return r, nil
}
