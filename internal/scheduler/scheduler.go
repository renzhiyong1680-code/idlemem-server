// Package scheduler 负责节点打分、订单分片分配与掉线自愈。
package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/node"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

// Scheduler 调度器：消费订单，把容量分片分配到最优节点。
type Scheduler struct {
	store    *store.Store
	registry *node.Registry

	// DefaultReplicas 默认副本数。
	DefaultReplicas int
	// MinShardBytes 单分片最小容量，避免碎片化（默认 512MiB）。
	MinShardBytes uint64
}

// New 创建调度器。
func New(s *store.Store, reg *node.Registry, defaultReplicas int) *Scheduler {
	return &Scheduler{
		store:           s,
		registry:        reg,
		DefaultReplicas: defaultReplicas,
		MinShardBytes:   512 << 20,
	}
}

// PlaceOrder 为一个订单执行分片分配。
// 返回创建的分片列表；若当前无可用节点，返回 ErrNoNodes。
func (sc *Scheduler) PlaceOrder(o *model.Order) ([]*model.Shard, error) {
	replicas := o.Replicas
	if replicas <= 0 {
		replicas = sc.DefaultReplicas
	}

	avail, err := sc.availableNodes()
	if err != nil {
		return nil, err
	}
	if len(avail) == 0 {
		return nil, ErrNoNodes
	}

	// 每个分片一个节点，副本数表示总份数（同一订单多份数据）。
	shards := make([]*model.Shard, 0, replicas)
	per := o.CapacityBytes / uint64(replicas)
	if per == 0 {
		per = o.CapacityBytes
	}
	if per < sc.MinShardBytes {
		per = sc.MinShardBytes
	}

	picked := sc.pickNodes(avail, replicas)
	for i, n := range picked {
		sh := &model.Shard{
			ID:         newID("shard"),
			OrderID:    o.ID,
			TenantID:   o.TenantID,
			NodeID:     n.ID,
			KeyPrefix:  fmt.Sprintf("%s:%s:", o.TenantID, o.ID),
			Bytes:      per,
			Status:     model.ShardActive,
			AssignedAt: time.Now(),
		}
		if err := sc.store.UpsertShard(sh); err != nil {
			return nil, err
		}
		// 分片占用了节点容量，更新节点已用容量。
		n.UsedBytes += sh.Bytes
		if err := sc.store.UpsertNode(n); err != nil {
			return nil, err
		}
		_ = i
		shards = append(shards, sh)
	}

	o.Status = model.OrderActive
	if err := sc.store.UpsertOrder(o); err != nil {
		return nil, err
	}
	return shards, nil
}

// ReleaseExpired 回收已到期的订单：删除其全部分片、退回节点已用容量，
// 并把订单置为 expired。返回回收的订单数与分片数。
//
// 释放是幂等的：已 expired 的订单不会被重复处理。
// ReleaseOrder 释放单个订单：删除其全部分片、退回节点已用容量，并把订单置为 expired。
// 既用于到期回收，也用于下单分配失败时的回滚——避免一张未交付的订单继续占用租户配额。
// 返回释放的分片数量。
func (sc *Scheduler) ReleaseOrder(orderID string) (int, error) {
	o, err := sc.store.GetOrder(orderID)
	if err != nil {
		return 0, err
	}
	if o == nil {
		return 0, nil
	}
	shards, err := sc.store.ListShardsByOrder(orderID)
	if err != nil {
		return 0, err
	}
	released := 0
	for _, sh := range shards {
		// 退回节点已用容量（不低零，避免与 Agent 上报的实际占用冲突）。
		if n, gerr := sc.store.GetNode(sh.NodeID); gerr == nil && n != nil {
			if n.UsedBytes >= sh.Bytes {
				n.UsedBytes -= sh.Bytes
			} else {
				n.UsedBytes = 0
			}
			if uerr := sc.store.UpsertNode(n); uerr != nil {
				return released, uerr
			}
		}
		if derr := sc.store.DeleteShard(sh.ID); derr != nil {
			return released, derr
		}
		released++
	}
	o.Status = model.OrderExpired
	if uerr := sc.store.UpsertOrder(o); uerr != nil {
		return released, uerr
	}
	return released, nil
}

func (sc *Scheduler) ReleaseExpired(now time.Time) (int, int, error) {
	orders, err := sc.store.ListOrders()
	if err != nil {
		return 0, 0, err
	}

	releasedOrders, releasedShards := 0, 0
	for _, o := range orders {
		if o.Status == model.OrderExpired || o.ExpireAt.IsZero() || now.Before(o.ExpireAt) {
			continue
		}
		n, rerr := sc.ReleaseOrder(o.ID)
		releasedShards += n
		if rerr != nil {
			return releasedOrders, releasedShards, rerr
		}
		releasedOrders++
	}
	return releasedOrders, releasedShards, nil
}

// Rebalance 扫描异常分片并自愈：
//   - 分片所在节点离线 → 标记 degraded，尝试迁移到其他在线节点；
//   - 迁移成功 → 分片置为 migrating 并最终 active（MVP 直接置 active）。
//
// 返回修复的分片数量。
func (sc *Scheduler) Rebalance() (int, error) {
	now := time.Now()
	nodes, err := sc.store.ListNodes()
	if err != nil {
		return 0, err
	}
	byID := make(map[string]*model.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}

	shards, err := sc.store.ListShards()
	if err != nil {
		return 0, err
	}

	fixed := 0
	for _, sh := range shards {
		if sh.Status != model.ShardActive {
			continue
		}
		n, ok := byID[sh.NodeID]
		if ok && !sc.registry.IsOffline(n, now) {
			continue
		}
		// 节点离线/不存在，尝试迁移。
		target, err := sc.findReplacement(sh, byID, now)
		if err != nil {
			sh.Status = model.ShardLost
			if uerr := sc.store.UpsertShard(sh); uerr != nil {
				return fixed, uerr
			}
			continue
		}
		sh.NodeID = target.ID
		sh.Status = model.ShardActive
		sh.AssignedAt = now
		if err := sc.store.UpsertShard(sh); err != nil {
			return fixed, err
		}
		fixed++
	}
	return fixed, nil
}

// availableNodes 返回当前可承接新订单的节点（online 且有富余容量）。
func (sc *Scheduler) availableNodes() ([]*model.Node, error) {
	now := time.Now()
	nodes, err := sc.store.ListNodes()
	if err != nil {
		return nil, err
	}
	out := make([]*model.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Status != model.NodeOnline {
			continue
		}
		if sc.registry.IsOffline(n, now) {
			continue
		}
		if n.CapacityBytes <= n.UsedBytes {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// pickNodes 按信誉分降序选出前 n 个节点。
func (sc *Scheduler) pickNodes(nodes []*model.Node, n int) []*model.Node {
	sorted := make([]*model.Node, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Score > sorted[j].Score
	})
	if n > len(sorted) {
		n = len(sorted)
	}
	return sorted[:n]
}

// findReplacement 找一个可承接该分片的节点（排除原节点）。
func (sc *Scheduler) findReplacement(sh *model.Shard, byID map[string]*model.Node, now time.Time) (*model.Node, error) {
	avail, err := sc.availableNodes()
	if err != nil {
		return nil, err
	}
	// 排除原节点与已离线的。
	for _, n := range avail {
		if n.ID == sh.NodeID {
			continue
		}
		if uint64(n.CapacityBytes-n.UsedBytes) >= sh.Bytes {
			n.UsedBytes += sh.Bytes
			_ = sc.store.UpsertNode(n)
			return n, nil
		}
	}
	return nil, ErrNoNodes
}

// newID 生成随机 ID。
func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}
