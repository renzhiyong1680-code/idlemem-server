package settle

import (
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/xiancuncloud/idlemem-server/internal/billing"
	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

func newTestSettler(t *testing.T) (*Settler, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, billing.Price{}, log.New(io.Discard, "", 0)), st
}

func putUsage(t *testing.T, st *store.Store, scope model.UsageScope, id string, gbHours float64) {
	t.Helper()
	if err := st.UpsertUsage(&model.UsageRecord{Scope: scope, ID: id, GBHours: gbHours, UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("upsert usage: %v", err)
	}
}

// TestSettle_FirstInvoiceAndNoDoubleBilling 验证首次出账金额正确、重复出账不会重复计费。
func TestSettle_FirstInvoiceAndNoDoubleBilling(t *testing.T) {
	s, st := newTestSettler(t)

	putUsage(t, st, model.UsageScopeTenant, "t1", 1000)
	putUsage(t, st, model.UsageScopeNodeUsed, "n1", 1000)
	putUsage(t, st, model.UsageScopeNode, "n1", 8000)

	inv, err := s.Settle(time.Now())
	if err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if len(inv.Tenants) != 1 || len(inv.Nodes) != 1 {
		t.Fatalf("rows = %d/%d, want 1/1", len(inv.Tenants), len(inv.Nodes))
	}
	if got := inv.Totals.CustomerRevenueCNY; diff(got, 0.4) > 1e-9 {
		t.Fatalf("revenue = %v, want 0.4", got)
	}
	if got := inv.Totals.SupplierPayoutCNY; diff(got, 0.32) > 1e-9 {
		t.Fatalf("payout = %v, want 0.32", got)
	}
	if got := inv.Totals.CommissionCNY; diff(got, 0.08) > 1e-9 {
		t.Fatalf("commission = %v, want 0.08", got)
	}

	// 用量未增长：第二次出账应返回 ErrNothingToBill。
	if _, err := s.Settle(time.Now()); !errors.Is(err, ErrNothingToBill) {
		t.Fatalf("second settle err = %v, want ErrNothingToBill", err)
	}
}

// TestSettle_Incremental 验证累计量增长后只结算增量。
func TestSettle_Incremental(t *testing.T) {
	s, st := newTestSettler(t)

	putUsage(t, st, model.UsageScopeTenant, "t1", 1000)
	if _, err := s.Settle(time.Now()); err != nil {
		t.Fatalf("first settle: %v", err)
	}

	// 累计涨到 1600：本期增量 600 GB·h → 0.24 元。
	putUsage(t, st, model.UsageScopeTenant, "t1", 1600)
	inv, err := s.Settle(time.Now())
	if err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if got := inv.Totals.TenantGBHours; diff(got, 600) > 1e-9 {
		t.Fatalf("period gb_hours = %v, want 600", got)
	}
	if got := inv.Totals.CustomerRevenueCNY; diff(got, 0.24) > 1e-9 {
		t.Fatalf("period revenue = %v, want 0.24", got)
	}
}

// TestSettle_ListAndGet 验证历史账单可查。
func TestSettle_ListAndGet(t *testing.T) {
	s, st := newTestSettler(t)
	putUsage(t, st, model.UsageScopeTenant, "t1", 500)

	inv, err := s.Settle(time.Now())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d, err %v", len(list), err)
	}
	got, err := s.Get(inv.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.ID != inv.ID || len(got.Tenants) != 1 {
		t.Fatalf("get mismatch: %+v", got)
	}
	if missing, err := s.Get("nope"); err != nil || missing != nil {
		t.Fatalf("missing invoice = %v, err %v", missing, err)
	}
}

func diff(a, b float64) float64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}
