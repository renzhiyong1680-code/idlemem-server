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
	// 兜底：配置也没写时按 1 份处理。否则下面按副本数切分容量会整数除零。
	if replicas <= 0 {
		replicas = 1
	}

	// 每个分片一个节点，副本数表示总份数（同一订单多份数据）。
	per := o.CapacityBytes / uint64(replicas)
	if per == 0 {
		per = o.CapacityBytes
	}
	if per < sc.MinShardBytes {
		per = sc.MinShardBytes
	}

	cands, err := sc.candidates()
	if err != nil {
		return nil, err
	}
	picked := pickCandidates(cands, replicas, per)
	if len(picked) == 0 {
		return nil, ErrNoNodes
	}

	shards := make([]*model.Shard, 0, len(picked))
	for _, c := range picked {
		n := c.node
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
		// 这里累加只用于列表展示的"已用容量"估算；调度决策以分片承诺量为准，
		// 因为 Agent 心跳会用真实占用覆盖这个字段（节点重启后归零）。
		n.UsedBytes += sh.Bytes
		if err := sc.store.UpsertNode(n); err != nil {
			return nil, err
		}
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

// candidate 一个可承接分片的节点，以及它实际还能承诺出去的容量。
type candidate struct {
	node *model.Node
	free uint64
}

// allocatedByNode 汇总每个节点上仍占用架构容量的分片字节数。
//
// 这是调度侧判断"节点还剩多少能卖"的唯一依据。不能看 Node.UsedBytes：
// 那个字段会被 Agent 心跳用真实 KV 占用覆盖，节点重启后归零，
// 于是调度以为节点是空的、继续往上卖，造成超卖。
// lost 分片已经不承载数据，不占承诺量。
func (sc *Scheduler) allocatedByNode() (map[string]uint64, error) {
	shards, err := sc.store.ListShards()
	if err != nil {
		return nil, err
	}
	alloc := make(map[string]uint64, len(shards))
	for _, sh := range shards {
		if sh.Status == model.ShardLost {
			continue
		}
		alloc[sh.NodeID] += sh.Bytes
	}
	return alloc, nil
}

// candidates 返回当前可承接新分片的节点（online 且承诺余量大于 0）。
func (sc *Scheduler) candidates() ([]candidate, error) {
	now := time.Now()
	nodes, err := sc.store.ListNodes()
	if err != nil {
		return nil, err
	}
	alloc, err := sc.allocatedByNode()
	if err != nil {
		return nil, err
	}
	out := make([]candidate, 0, len(nodes))
	for _, n := range nodes {
		if n.Status != model.NodeOnline {
			continue
		}
		if sc.registry.IsOffline(n, now) {
			continue
		}
		used := alloc[n.ID]
		if n.CapacityBytes <= used {
			continue
		}
		out = append(out, candidate{node: n, free: n.CapacityBytes - used})
	}
	return out, nil
}

// pickCandidates 按信誉分降序选出前 n 个、且各自余量能放下一个分片的节点。
func pickCandidates(cands []candidate, n int, need uint64) []candidate {
	sorted := make([]candidate, len(cands))
	copy(sorted, cands)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].node.Score > sorted[j].node.Score
	})
	out := make([]candidate, 0, n)
	for _, c := range sorted {
		if len(out) >= n {
			break
		}
		if c.free < need {
			continue
		}
		out = append(out, c)
	}
	return out
}

// findReplacement 找一个可承接该分片的节点（排除原节点）。
func (sc *Scheduler) findReplacement(sh *model.Shard, byID map[string]*model.Node, now time.Time) (*model.Node, error) {
	cands, err := sc.candidates()
	if err != nil {
		return nil, err
	}
	// 排除原节点；余量必须放得下这个分片。
	for _, c := range cands {
		if c.node.ID == sh.NodeID {
			continue
		}
		if c.free < sh.Bytes {
			continue
		}
		c.node.UsedBytes += sh.Bytes
		_ = sc.store.UpsertNode(c.node)
		return c.node, nil
	}
	return nil, ErrNoNodes
}

// newID 生成随机 ID。
func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}
