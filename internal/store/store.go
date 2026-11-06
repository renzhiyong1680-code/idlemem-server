// Package store 提供基于 buntdb 的持久化存储。
//
// buntdb 是纯 Go、零 CGO 的嵌入式 KV 数据库，适合单实例控制面
// 的 MVP 阶段；后续规模扩大可平滑替换为 PostgreSQL 等。
package store

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/buntdb"

	"github.com/xiancuncloud/idlemem-server/internal/model"
)

// key 前缀命名空间。
const (
	nsNode    = "node:"
	nsTenant  = "tenant:"
	nsOrder   = "order:"
	nsShard   = "shard:"
	nsUsage   = "usage:"
	nsBase    = "baseline:"
	nsInvoice = "invoice:"
)

// Store 控制面持久化存储。
type Store struct {
	mu   sync.Mutex
	db   *buntdb.DB
	path string
}

// Open 打开（或创建）数据库文件。
func Open(path string) (*Store, error) {
	db, err := buntdb.Open(path)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func decode[T any](raw string) (T, error) {
	var v T
	err := json.Unmarshal([]byte(raw), &v)
	return v, err
}

// ---------- 节点 ----------

// UpsertNode 写入或更新节点。
func (s *Store) UpsertNode(n *model.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := encode(n)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsNode+n.ID, raw, nil)
		return err
	})
}

// GetNode 读取节点；不存在时返回 nil。
func (s *Store) GetNode(id string) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *model.Node
	err := s.db.View(func(tx *buntdb.Tx) error {
		raw, err := tx.Get(nsNode + id)
		if err == buntdb.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		n, derr := decode[model.Node](raw)
		if derr != nil {
			return derr
		}
		out = &n
		return nil
	})
	return out, err
}

// ListNodes 返回全部节点（按 ID 排序）。
func (s *Store) ListNodes() ([]*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Node
	err := s.db.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(nsNode+"*", func(k, v string) bool {
			n, err := decode[model.Node](v)
			if err == nil {
				out = append(out, &n)
			}
			return true
		})
	})
	return out, err
}

// DeleteNode 删除节点。
func (s *Store) DeleteNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, err := tx.Delete(nsNode + id)
		return err
	})
}

// ---------- 租户 ----------

// UpsertTenant 写入或更新租户。
func (s *Store) UpsertTenant(t *model.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := encode(t)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsTenant+t.ID, raw, nil)
		return err
	})
}

// GetTenant 读取租户；不存在时返回 nil。
func (s *Store) GetTenant(id string) (*model.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *model.Tenant
	err := s.db.View(func(tx *buntdb.Tx) error {
		raw, err := tx.Get(nsTenant + id)
		if err == buntdb.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		t, derr := decode[model.Tenant](raw)
		if derr != nil {
			return derr
		}
		out = &t
		return nil
	})
	return out, err
}

// ListTenants 返回全部租户。
func (s *Store) ListTenants() ([]*model.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Tenant
	err := s.db.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(nsTenant+"*", func(k, v string) bool {
			t, err := decode[model.Tenant](v)
			if err == nil {
				out = append(out, &t)
			}
			return true
		})
	})
	return out, err
}

// ---------- 订单 ----------

// UpsertOrder 写入或更新订单。
func (s *Store) UpsertOrder(o *model.Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := encode(o)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsOrder+o.ID, raw, nil)
		return err
	})
}

// GetOrder 读取订单。
func (s *Store) GetOrder(id string) (*model.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *model.Order
	err := s.db.View(func(tx *buntdb.Tx) error {
		raw, err := tx.Get(nsOrder + id)
		if err == buntdb.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		o, derr := decode[model.Order](raw)
		if derr != nil {
			return derr
		}
		out = &o
		return nil
	})
	return out, err
}

// ListOrders 返回全部订单。
func (s *Store) ListOrders() ([]*model.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Order
	err := s.db.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(nsOrder+"*", func(k, v string) bool {
			o, err := decode[model.Order](v)
			if err == nil {
				out = append(out, &o)
			}
			return true
		})
	})
	return out, err
}

// ---------- 分片 ----------

// UpsertShard 写入或更新分片。
func (s *Store) UpsertShard(sh *model.Shard) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := encode(sh)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsShard+sh.ID, raw, nil)
		return err
	})
}

// GetShard 读取分片。
func (s *Store) GetShard(id string) (*model.Shard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *model.Shard
	err := s.db.View(func(tx *buntdb.Tx) error {
		raw, err := tx.Get(nsShard + id)
		if err == buntdb.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		sh, derr := decode[model.Shard](raw)
		if derr != nil {
			return derr
		}
		out = &sh
		return nil
	})
	return out, err
}

// ListShards 返回全部分片。
func (s *Store) ListShards() ([]*model.Shard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Shard
	err := s.db.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(nsShard+"*", func(k, v string) bool {
			sh, err := decode[model.Shard](v)
			if err == nil {
				out = append(out, &sh)
			}
			return true
		})
	})
	return out, err
}

// ListShardsByOrder 返回某订单的全部分片。
func (s *Store) ListShardsByOrder(orderID string) ([]*model.Shard, error) {
	all, err := s.ListShards()
	if err != nil {
		return nil, err
	}
	var out []*model.Shard
	for _, sh := range all {
		if sh.OrderID == orderID {
			out = append(out, sh)
		}
	}
	return out, nil
}

// ListShardsByNode 返回落在某节点上的全部分片。
func (s *Store) ListShardsByNode(nodeID string) ([]*model.Shard, error) {
	all, err := s.ListShards()
	if err != nil {
		return nil, err
	}
	var out []*model.Shard
	for _, sh := range all {
		if sh.NodeID == nodeID {
			out = append(out, sh)
		}
	}
	return out, nil
}

// UpdateShardNode 迁移分片到新节点。
func (s *Store) UpdateShardNode(shardID, newNodeID string, st model.ShardStatus) error {
	sh, err := s.GetShard(shardID)
	if err != nil {
		return err
	}
	if sh == nil {
		return buntdb.ErrNotFound
	}
	sh.NodeID = newNodeID
	sh.Status = st
	sh.AssignedAt = time.Now()
	return s.UpsertShard(sh)
}

// ---------- 计量（GB·h） ----------

// UpsertUsage 写入或更新计量记录。
func (s *Store) UpsertUsage(r *model.UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := encode(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsUsage+string(r.Scope)+":"+r.ID, raw, nil)
		return err
	})
}

// GetUsage 读取计量记录；不存在时返回 nil。
func (s *Store) GetUsage(scope model.UsageScope, id string) (*model.UsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *model.UsageRecord
	err := s.db.View(func(tx *buntdb.Tx) error {
		raw, err := tx.Get(nsUsage + string(scope) + ":" + id)
		if err == buntdb.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		r, derr := decode[model.UsageRecord](raw)
		if derr != nil {
			return derr
		}
		out = &r
		return nil
	})
	return out, err
}

// ListUsage 返回某范围内的全部计量记录（按 ID 排序）。
func (s *Store) ListUsage(scope model.UsageScope) ([]*model.UsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.UsageRecord
	err := s.db.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(nsUsage+string(scope)+":*", func(k, v string) bool {
			r, err := decode[model.UsageRecord](v)
			if err == nil {
				out = append(out, &r)
			}
			return true
		})
	})
	return out, err
}

// ---------- 结算基线（账期出账用） ----------

// Baseline 累计计量的已结算水位。
type Baseline struct {
	GBHours float64   `json:"gb_hours"`
	At      time.Time `json:"at"`
}

func baselineKey(scope model.UsageScope, id string) string {
	return nsBase + string(scope) + ":" + id
}

// GetBaseline 读取某对象的结算水位；不存在时返回零值。
func (s *Store) GetBaseline(scope model.UsageScope, id string) (Baseline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Baseline
	err := s.db.View(func(tx *buntdb.Tx) error {
		raw, err := tx.Get(baselineKey(scope, id))
		if err == buntdb.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		b, derr := decode[Baseline](raw)
		if derr != nil {
			return derr
		}
		out = b
		return nil
	})
	return out, err
}

// SetBaselines 批量写入结算水位（单事务，避免出账中途部分生效）。
func (s *Store) SetBaselines(items map[string]Baseline) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *buntdb.Tx) error {
		for k, v := range items {
			raw, err := encode(v)
			if err != nil {
				return err
			}
			if _, _, err := tx.Set(nsBase+k, raw, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListBaselines 返回某范围全部对象的结算水位，key 为对象 ID。
func (s *Store) ListBaselines(scope model.UsageScope) (map[string]Baseline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Baseline{}
	err := s.db.View(func(tx *buntdb.Tx) error {
		prefix := nsBase + string(scope) + ":"
		return tx.AscendKeys(prefix+"*", func(k, v string) bool {
			b, err := decode[Baseline](v)
			if err == nil {
				out[strings.TrimPrefix(k, prefix)] = b
			}
			return true
		})
	})
	return out, err
}

// ---------- 期账单 ----------

// UpsertInvoice 写入期账单原始 JSON。
func (s *Store) UpsertInvoice(id string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsInvoice+id, string(payload), nil)
		return err
	})
}

// GetInvoice 读取期账单原始 JSON；不存在时返回 nil。
func (s *Store) GetInvoice(id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	err := s.db.View(func(tx *buntdb.Tx) error {
		raw, err := tx.Get(nsInvoice + id)
		if err == buntdb.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		out = []byte(raw)
		return nil
	})
	return out, err
}

// ListInvoices 返回全部期账单原始 JSON（按 ID 排序，即按生成时间排序）。
func (s *Store) ListInvoices() ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	err := s.db.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(nsInvoice+"*", func(k, v string) bool {
			out = append(out, []byte(v))
			return true
		})
	})
	return out, err
}
