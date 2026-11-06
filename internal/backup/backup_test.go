package backup

import (
	"strings"
	"testing"
	"time"

	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seed(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	must(st.UpsertNode(&model.Node{
		ID: "node-1", Status: model.NodeOnline, CapacityBytes: 1 << 30,
		UsedBytes: 1 << 20, TotalBytes: 8 << 30, KVAddr: ":16380", Score: 12.5,
	}), "node")
	must(st.UpsertTenant(&model.Tenant{
		ID: "tenant-1", Name: "acme", Token: "tok", QuotaBytes: 2 << 30,
		Status: model.TenantActive,
	}), "tenant")
	must(st.UpsertOrder(&model.Order{
		ID: "order-1", TenantID: "tenant-1", CapacityBytes: 1 << 30, Replicas: 2,
		Status: model.OrderActive, ExpireAt: now.Add(24 * time.Hour),
	}), "order")
	must(st.UpsertShard(&model.Shard{
		ID: "shard-1", OrderID: "order-1", TenantID: "tenant-1", NodeID: "node-1",
		KeyPrefix: "tenant-1:", Bytes: 1 << 30, Status: model.ShardActive,
	}), "shard")
	must(st.UpsertUsage(&model.UsageRecord{
		Scope: model.UsageScopeTenant, ID: "tenant-1", GBHours: 3.25, UpdatedAt: now,
	}), "usage")
	must(st.SetBaselines(map[string]store.Baseline{
		"tenant:tenant-1": {GBHours: 3.25, At: now},
	}), "baselines")
	must(st.UpsertInvoice("inv-1", []byte(`{"id":"inv-1","amount":0.5}`)), "invoice")
}

func TestExportRestoreRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	src := openStore(t)
	seed(t, src, now)

	snap, err := Export(src, now)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if snap.Format != FormatVersion {
		t.Fatalf("format: %q", snap.Format)
	}
	if len(snap.Nodes) != 1 || len(snap.Tenants) != 1 || len(snap.Orders) != 1 || len(snap.Shards) != 1 {
		t.Fatalf("unexpected sizes: nodes=%d tenants=%d orders=%d shards=%d",
			len(snap.Nodes), len(snap.Tenants), len(snap.Orders), len(snap.Shards))
	}
	if len(snap.Usage) != 1 || len(snap.Baselines) != 1 || len(snap.Invoices) != 1 {
		t.Fatalf("unexpected sizes: usage=%d baselines=%d invoices=%d",
			len(snap.Usage), len(snap.Baselines), len(snap.Invoices))
	}
	if _, ok := snap.Baselines["tenant:tenant-1"]; !ok {
		t.Fatalf("baseline key missing: %v", snap.Baselines)
	}

	// 恢复到空库，逐项核对关键字段。
	dst := openStore(t)
	res, err := Restore(dst, snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Nodes != 1 || res.Tenants != 1 || res.Orders != 1 || res.Shards != 1 ||
		res.Usage != 1 || res.Baselines != 1 || res.Invoices != 1 {
		t.Fatalf("restore counts: %+v", res)
	}

	ten, err := dst.GetTenant("tenant-1")
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if ten.QuotaBytes != 2<<30 || ten.Token != "tok" || ten.Status != model.TenantActive {
		t.Fatalf("tenant mismatch: %+v", ten)
	}
	n, err := dst.GetNode("node-1")
	if err != nil || n.KVAddr != ":16380" || n.CapacityBytes != 1<<30 {
		t.Fatalf("node mismatch: %+v err=%v", n, err)
	}
	sh, err := dst.GetShard("shard-1")
	if err != nil || sh.NodeID != "node-1" || sh.KeyPrefix != "tenant-1:" {
		t.Fatalf("shard mismatch: %+v err=%v", sh, err)
	}
	u, err := dst.GetUsage(model.UsageScopeTenant, "tenant-1")
	if err != nil || u.GBHours != 3.25 {
		t.Fatalf("usage mismatch: %+v err=%v", u, err)
	}
	b, err := dst.GetBaseline(model.UsageScopeTenant, "tenant-1")
	if err != nil || b.GBHours != 3.25 {
		t.Fatalf("baseline mismatch: %+v err=%v", b, err)
	}
	inv, err := dst.GetInvoice("inv-1")
	if err != nil || !strings.Contains(string(inv), `"amount":0.5`) {
		t.Fatalf("invoice mismatch: %q err=%v", string(inv), err)
	}

	// 重复恢复应当幂等（Upsert 语义）。
	if _, err := Restore(dst, snap); err != nil {
		t.Fatalf("second restore: %v", err)
	}
}

func TestRestoreRejectsUnknownFormat(t *testing.T) {
	dst := openStore(t)
	if _, err := Restore(dst, &Snapshot{Format: "9"}); err == nil {
		t.Fatalf("expected format error")
	}
}
