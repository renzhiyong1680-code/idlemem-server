package meter

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/renzhiyong1680-code/idlemem-server/internal/model"
	"github.com/renzhiyong1680-code/idlemem-server/internal/store"
)

func newTestMeter(t *testing.T, interval time.Duration) (*Meter, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, interval, log.New(io.Discard, "", 0)), st
}

// TestMeter_AccumulatesNodeAndTenant 验证一次 tick 的累加口径：
// 10GB 在线容量 × 1 分钟 = 10/60 GB·h；租户 4GB 活跃分片 × 1 分钟 = 4/60 GB·h。
func TestMeter_AccumulatesNodeAndTenant(t *testing.T) {
	m, st := newTestMeter(t, time.Minute)

	// 节点：10GB 在线。
	if err := st.UpsertNode(&model.Node{ID: "n1", Status: model.NodeOnline, CapacityBytes: 10 * GB}); err != nil {
		t.Fatalf("upsert node: %v", err)
	}
	// 离线节点不计。
	if err := st.UpsertNode(&model.Node{ID: "n2", Status: model.NodeOffline, CapacityBytes: 10 * GB}); err != nil {
		t.Fatalf("upsert offline node: %v", err)
	}
	// 租户分片：4GB 活跃（承载于 n1）+ 2GB 丢失（不计）。
	if err := st.UpsertShard(&model.Shard{ID: "s1", TenantID: "t1", NodeID: "n1", Bytes: 4 * GB, Status: model.ShardActive}); err != nil {
		t.Fatalf("upsert shard: %v", err)
	}
	if err := st.UpsertShard(&model.Shard{ID: "s2", TenantID: "t1", NodeID: "n1", Bytes: 2 * GB, Status: model.ShardLost}); err != nil {
		t.Fatalf("upsert shard: %v", err)
	}

	m.tick()

	rec, err := st.GetUsage(model.UsageScopeNode, "n1")
	if err != nil || rec == nil {
		t.Fatalf("node usage: %v, %v", rec, err)
	}
	wantNode := float64(10*GB) / GB * (time.Minute.Seconds() / 3600.0) // 10/60
	if diff := rec.GBHours - wantNode; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("node gb_hours = %v, want %v", rec.GBHours, wantNode)
	}

	// 节点承载维度：只计活跃分片 4GB × 1 分钟 = 4/60 GB·h（供给结算依据）。
	used, err := st.GetUsage(model.UsageScopeNodeUsed, "n1")
	if err != nil || used == nil {
		t.Fatalf("node_used usage: %v, %v", used, err)
	}
	wantUsed := float64(4*GB) / GB * (time.Minute.Seconds() / 3600.0)
	if diff := used.GBHours - wantUsed; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("node_used gb_hours = %v, want %v", used.GBHours, wantUsed)
	}

	// 离线节点不应产生记录。
	if rec, _ := st.GetUsage(model.UsageScopeNode, "n2"); rec != nil {
		t.Fatalf("offline node should not be metered: %+v", rec)
	}

	trec, err := st.GetUsage(model.UsageScopeTenant, "t1")
	if err != nil || trec == nil {
		t.Fatalf("tenant usage: %v, %v", trec, err)
	}
	wantTenant := float64(4*GB) / GB * (time.Minute.Seconds() / 3600.0) // 4/60
	if diff := trec.GBHours - wantTenant; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("tenant gb_hours = %v, want %v", trec.GBHours, wantTenant)
	}
}

// TestMeter_AccumulatesAcrossTicks 验证多次 tick 单调累加。
func TestMeter_AccumulatesAcrossTicks(t *testing.T) {
	m, st := newTestMeter(t, 10*time.Second)
	if err := st.UpsertNode(&model.Node{ID: "n1", Status: model.NodeOnline, CapacityBytes: GB}); err != nil {
		t.Fatalf("upsert node: %v", err)
	}

	m.tick()
	m.tick()
	m.tick()

	rec, err := st.GetUsage(model.UsageScopeNode, "n1")
	if err != nil || rec == nil {
		t.Fatalf("usage: %v, %v", rec, err)
	}
	want := 3 * float64(GB) / GB * (10.0 / 3600.0)
	if diff := rec.GBHours - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("gb_hours = %v, want %v", rec.GBHours, want)
	}
}

// TestMeter_RunLoop 验证 Run 的循环采样（短间隔快速跑两轮）。
func TestMeter_RunLoop(t *testing.T) {
	m, st := newTestMeter(t, 50*time.Millisecond)
	if err := st.UpsertNode(&model.Node{ID: "n1", Status: model.NodeOnline, CapacityBytes: GB}); err != nil {
		t.Fatalf("upsert node: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	time.Sleep(180 * time.Millisecond)
	cancel()
	<-done

	rec, err := st.GetUsage(model.UsageScopeNode, "n1")
	if err != nil || rec == nil {
		t.Fatalf("usage: %v, %v", rec, err)
	}
	if rec.GBHours <= 0 {
		t.Fatalf("expected accumulated usage, got %v", rec.GBHours)
	}
}
