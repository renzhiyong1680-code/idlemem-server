// Package node 负责节点注册、心跳更新与在线判定。
package node

import (
	"time"

	"github.com/xiancuncloud/idlemem-core/heartbeat"

	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

// Registry 节点注册表：负责节点的增删改查与状态判定。
type Registry struct {
	store *store.Store
	// HeartbeatTimeout 心跳超时阈值。
	HeartbeatTimeout time.Duration
	// EvictAfter 离线节点清理阈值。
	EvictAfter time.Duration
}

// New 创建节点注册表。
func New(s *store.Store, heartbeatTimeout, evictAfter time.Duration) *Registry {
	return &Registry{
		store:            s,
		HeartbeatTimeout: heartbeatTimeout,
		EvictAfter:       evictAfter,
	}
}

// Register 注册（或幂等更新）一个节点。返回该节点当前状态。
func (r *Registry) Register(id, ip, version string) (*model.Node, error) {
	now := time.Now()
	n, err := r.store.GetNode(id)
	if err != nil {
		return nil, err
	}
	if n == nil {
		n = &model.Node{
			ID:        id,
			IP:        ip,
			Version:   version,
			Status:    model.NodePending,
			FirstSeen: now,
			LastSeen:  now,
		}
	} else {
		// 已注册节点重新上报（例如 Agent 重启），保留首次时间，重置为 pending
		// 直到收到下一次心跳。
		n.IP = ip
		n.Version = version
		n.Status = model.NodePending
		n.LastSeen = now
	}
	if err := r.store.UpsertNode(n); err != nil {
		return nil, err
	}
	return n, nil
}

// Heartbeat 处理一次心跳上报，更新节点状态并返回更新后的节点。
// 心跳与注册可以合并为同一个上报链路，此处也支持"注册即心跳"。
func (r *Registry) Heartbeat(snap heartbeat.Snapshot, ip string) (*model.Node, error) {
	now := time.Now()
	n, err := r.store.GetNode(snap.NodeID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		// 未注册先自动注册。
		n = &model.Node{
			ID:        snap.NodeID,
			IP:        ip,
			Version:   snap.Version,
			Status:    model.NodeOnline,
			FirstSeen: now,
			LastSeen:  now,
		}
	} else {
		n.IP = ip
		n.Version = snap.Version
		n.LastSeen = now
	}

	n.CapacityBytes = snap.CapacityBytes
	n.UsedBytes = snap.UsedBytes
	n.SystemUsedPct = snap.SystemUsedPct
	n.TotalBytes = snap.TotalBytes
	n.KVAddr = snap.KVAddr
	n.HeartbeatCnt++

	switch snap.Status {
	case heartbeat.StatusOnline:
		n.Status = model.NodeOnline
	case heartbeat.StatusSuspended:
		n.Status = model.NodeSuspended
	default:
		n.Status = model.NodeOffline
	}

	// 信誉分：基础分来自连续心跳次数与在线时长，简单加权。
	// 可在此扩展为更精细的稳定性评估。
	uptime := now.Sub(n.FirstSeen)
	score := float64(n.HeartbeatCnt) + uptime.Minutes()*0.1
	if n.Status == model.NodeOnline && n.CapacityBytes > 0 {
		score += 10
	}
	n.Score = score

	if err := r.store.UpsertNode(n); err != nil {
		return nil, err
	}
	return n, nil
}

// IsOffline 判断节点是否心跳超时。
func (r *Registry) IsOffline(n *model.Node, now time.Time) bool {
	return now.Sub(n.LastSeen) > r.HeartbeatTimeout
}

// MarkOffline 将节点标记为离线。
func (r *Registry) MarkOffline(n *model.Node) error {
	n.Status = model.NodeOffline
	return r.store.UpsertNode(n)
}

// Sweep 扫描全部节点：
//   - 心跳超时 → 标记离线；
//   - 离线超过 EvictAfter → 删除（回收）。
//
// 返回本轮被标记离线的节点 ID 和被清理的节点 ID。
func (r *Registry) Sweep() (markedOffline, evicted []string, err error) {
	now := time.Now()
	nodes, err := r.store.ListNodes()
	if err != nil {
		return nil, nil, err
	}
	for _, n := range nodes {
		if n.Status == model.NodeOffline {
			if now.Sub(n.LastSeen) > r.EvictAfter {
				if err := r.store.DeleteNode(n.ID); err != nil {
					return nil, nil, err
				}
				evicted = append(evicted, n.ID)
			}
			continue
		}
		if r.IsOffline(n, now) {
			if err := r.MarkOffline(n); err != nil {
				return nil, nil, err
			}
			markedOffline = append(markedOffline, n.ID)
		}
	}
	return markedOffline, evicted, nil
}
