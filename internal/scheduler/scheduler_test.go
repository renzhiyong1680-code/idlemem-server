package scheduler

import (
	"testing"
	"time"

	"github.com/xiancuncloud/idlemem-core/heartbeat"

	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/node"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

func newTestEnv(t *testing.T) (*store.Store, *node.Registry, *Scheduler) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := node.New(st, 30*time.Second, time.Hour)
	sch := New(st, reg, 1)
	return st, reg, sch
}

// addNode 直接向存储写入一个在线节点（模拟已通过心跳注册）。
func addNode(t *testing.T, st *store.Store, id string, capBytes uint64, score float64) {
	t.Helper()
	now := time.Now()
	n := &model.Node{
		ID:            id,
		IP:            "127.0.0.1",
		Version:       "0.1.0",
		Status:        model.NodeOnline,
		CapacityBytes: capBytes,
		Score:         score,
		HeartbeatCnt:  10,
		FirstSeen:     now.Add(-time.Hour),
		LastSeen:      now,
	}
	if err := st.UpsertNode(n); err != nil {
		t.Fatalf("add node: %v", err)
	}
}

// heartbeatNode 通过注册表心跳链路注册一个节点。
func heartbeatNode(t *testing.T, reg *node.Registry, id string, capBytes uint64) {
	t.Helper()
	if _, err := reg.Heartbeat(heartbeat.Snapshot{
		NodeID:        id,
		Version:       "0.1.0",
		Status:        heartbeat.StatusOnline,
		CapacityBytes: capBytes,
	}, "127.0.0.1"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

// TestReleaseExpired 验证到期订单的分片被删除、节点已用容量退回、订单置为 expired。
func TestReleaseExpired(t *testing.T) {
	st, _, sch := newTestEnv(t)
	addNode(t, st, "node-a", 8<<30, 100)

	now := time.Now()
	o := &model.Order{
		ID:            "order-exp",
		TenantID:      "tenant-1",
		CapacityBytes: 1 << 30,
		Replicas:      1,
		Status:        model.OrderActive,
		CreatedAt:     now.Add(-2 * time.Hour),
		ExpireAt:      now.Add(-time.Hour), // 已过期
	}
	if _, err := sch.PlaceOrder(o); err != nil {
		t.Fatalf("place: %v", err)
	}
	shards, _ := st.ListShardsByOrder(o.ID)
	if len(shards) != 1 {
		t.Fatalf("shards = %d, want 1", len(shards))
	}
	before, _ := st.GetNode("node-a")
	if before.UsedBytes == 0 {
		t.Fatal("node used bytes should increase after placement")
	}

	orders, released, err := sch.ReleaseExpired(now)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if orders != 1 || released != 1 {
		t.Fatalf("released orders/shards = %d/%d, want 1/1", orders, released)
	}

	after, _ := st.GetNode("node-a")
	if after.UsedBytes != 0 {
		t.Fatalf("node used = %d, want 0 after release", after.UsedBytes)
	}
	if left, _ := st.ListShardsByOrder(o.ID); len(left) != 0 {
		t.Fatalf("shards left = %d, want 0", len(left))
	}
	got, _ := st.GetOrder(o.ID)
	if got.Status != model.OrderExpired {
		t.Fatalf("order status = %v, want expired", got.Status)
	}

	// 幂等：再跑一次不应重复回收。
	if orders2, _, _ := sch.ReleaseExpired(now); orders2 != 0 {
		t.Fatalf("second release = %d, want 0", orders2)
	}
}

func TestPlaceOrder_PicksHighestScoreNode(t *testing.T) {
	st, reg, sch := newTestEnv(t)
	_ = reg
	// 两个节点：score 高者应被选中。
	addNode(t, st, "node-a", 8<<30, 100)
	addNode(t, st, "node-b", 8<<30, 200)

	o := &model.Order{
		ID:            "order-1",
		TenantID:      "tenant-1",
		CapacityBytes: 1 << 30,
		Replicas:      1,
		Status:        model.OrderActive,
		CreatedAt:     time.Now(),
	}
	shards, err := sch.PlaceOrder(o)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if len(shards) != 1 {
		t.Fatalf("shards = %d, want 1", len(shards))
	}
	if shards[0].NodeID != "node-b" {
		t.Fatalf("shard assigned to %s, want node-b", shards[0].NodeID)
	}
	if shards[0].KeyPrefix == "" {
		t.Fatal("shard should have key prefix")
	}
}

func TestPlaceOrder_NoNodes(t *testing.T) {
	_, _, sch := newTestEnv(t)
	o := &model.Order{
		ID:            "order-1",
		TenantID:      "tenant-1",
		CapacityBytes: 1 << 30,
		Replicas:      1,
		Status:        model.OrderActive,
		CreatedAt:     time.Now(),
	}
	if _, err := sch.PlaceOrder(o); err != ErrNoNodes {
		t.Fatalf("err = %v, want ErrNoNodes", err)
	}
}

func TestPlaceOrder_IgnoresOfflineNode(t *testing.T) {
	st, reg, sch := newTestEnv(t)
	// node-a 在线，node-b 离线。
	addNode(t, st, "node-a", 8<<30, 100)
	nb := &model.Node{
		ID:            "node-b",
		Status:        model.NodeOffline,
		CapacityBytes: 8 << 30,
		Score:         999,
		LastSeen:      time.Now().Add(-time.Hour),
		FirstSeen:     time.Now().Add(-2 * time.Hour),
	}
	if err := st.UpsertNode(nb); err != nil {
		t.Fatalf("upsert offline node: %v", err)
	}

	o := &model.Order{
		ID:            "order-1",
		TenantID:      "tenant-1",
		CapacityBytes: 1 << 30,
		Replicas:      1,
		Status:        model.OrderActive,
		CreatedAt:     time.Now(),
	}
	shards, err := sch.PlaceOrder(o)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if shards[0].NodeID != "node-a" {
		t.Fatalf("shard assigned to %s, want node-a", shards[0].NodeID)
	}
	_ = reg
}

func TestRebalance_MigratesOfflineShard(t *testing.T) {
	st, reg, sch := newTestEnv(t)
	// node-a 在线（目标），node-b 挂了但承载一个分片。
	addNode(t, st, "node-a", 16<<30, 50)

	now := time.Now()
	nb := &model.Node{
		ID:            "node-b",
		Status:        model.NodeOnline,
		CapacityBytes: 8 << 30,
		Score:         10,
		LastSeen:      now.Add(-time.Hour), // 心跳超时（30s）
		FirstSeen:     now.Add(-2 * time.Hour),
	}
	if err := st.UpsertNode(nb); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	sh := &model.Shard{
		ID:         "shard-1",
		OrderID:    "order-1",
		TenantID:   "tenant-1",
		NodeID:     "node-b",
		KeyPrefix:  "tenant-1:order-1:",
		Bytes:      1 << 30,
		Status:     model.ShardActive,
		AssignedAt: now,
	}
	if err := st.UpsertShard(sh); err != nil {
		t.Fatalf("upsert shard: %v", err)
	}

	fixed, err := sch.Rebalance()
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if fixed != 1 {
		t.Fatalf("fixed = %d, want 1", fixed)
	}
	got, err := st.GetShard("shard-1")
	if err != nil || got == nil {
		t.Fatalf("get shard: %v, %v", got, err)
	}
	if got.NodeID != "node-a" {
		t.Fatalf("shard migrated to %s, want node-a", got.NodeID)
	}
	if got.Status != model.ShardActive {
		t.Fatalf("shard status = %s, want active", got.Status)
	}
	_ = reg
}

func TestHeartbeat_And_Sweep(t *testing.T) {
	st, reg, _ := newTestEnv(t)
	// 注册并心跳一个节点。
	heartbeatNode(t, reg, "node-live", 8<<30)
	// 一个心跳超时的"假在线"节点（直接写入，LastSeen 很旧）。
	old := &model.Node{
		ID:            "node-dead",
		Status:        model.NodeOnline,
		CapacityBytes: 8 << 30,
		LastSeen:      time.Now().Add(-time.Minute),
		FirstSeen:     time.Now().Add(-time.Hour),
	}
	if err := st.UpsertNode(old); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	marked, _, err := reg.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(marked) != 1 || marked[0] != "node-dead" {
		t.Fatalf("marked = %v, want [node-dead]", marked)
	}
	// node-live 应仍在线。
	n, err := st.GetNode("node-live")
	if err != nil || n == nil {
		t.Fatalf("node-live missing: %v", err)
	}
	if n.Status != model.NodeOnline {
		t.Fatalf("node-live status = %s, want online", n.Status)
	}
}
