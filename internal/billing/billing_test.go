package billing

import (
	"math"
	"testing"

	"github.com/renzhiyong1680-code/idlemem-server/internal/model"
)

func rec(scope model.UsageScope, id string, gbHours float64) *model.UsageRecord {
	return &model.UsageRecord{Scope: scope, ID: id, GBHours: gbHours}
}

func TestComputeAmounts(t *testing.T) {
	p := DefaultPrice()
	tenants := []*model.UsageRecord{
		rec(model.UsageScopeTenant, "t1", 1000),
		rec(model.UsageScopeTenant, "t2", 250),
	}
	nodeUsed := []*model.UsageRecord{
		rec(model.UsageScopeNodeUsed, "n1", 600),
		rec(model.UsageScopeNodeUsed, "n2", 650),
	}
	nodeOnline := []*model.UsageRecord{
		rec(model.UsageScopeNode, "n1", 5000),
		rec(model.UsageScopeNode, "n2", 7000),
	}

	rep := Compute(p, tenants, nodeUsed, nodeOnline)

	if len(rep.Tenants) != 2 || len(rep.Nodes) != 2 {
		t.Fatalf("rows = %d/%d, want 2/2", len(rep.Tenants), len(rep.Nodes))
	}
	// 排序后首行应为金额最高者：t1 1000 GB·h × 0.0004 = 0.4 元。
	if rep.Tenants[0].TenantID != "t1" {
		t.Fatalf("tenant order = %v, want t1 first", rep.Tenants[0].TenantID)
	}
	if got := rep.Tenants[0].AmountCNY; math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("t1 amount = %v, want 0.4", got)
	}
	if got := rep.Nodes[0].PayoutCNY; math.Abs(got-0.208) > 1e-9 { // 650 × 0.00032
		t.Fatalf("n payout = %v, want 0.208", got)
	}

	// 汇总：客户收入 1250×0.0004=0.5；供给支出 1250×0.00032=0.4；佣金 0.1。
	if got := rep.Totals.CustomerRevenueCNY; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("revenue = %v, want 0.5", got)
	}
	if got := rep.Totals.SupplierPayoutCNY; math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("payout = %v, want 0.4", got)
	}
	if got := rep.Totals.CommissionCNY; math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("commission = %v, want 0.1", got)
	}
	if got := p.CommissionPerGBHour(); math.Abs(got-0.00008) > 1e-12 {
		t.Fatalf("commission per GB·h = %v, want 0.00008", got)
	}
	// 在线供给只作参考：5000+7000 GB·h。
	if got := rep.Totals.SupplyGBHours; math.Abs(got-12000) > 1e-9 {
		t.Fatalf("supply = %v, want 12000", got)
	}
}

// TestComputeNoCommissionInversion 供大于求（在线容量远大于承载）时佣金仍非负。
func TestComputeNoCommissionInversion(t *testing.T) {
	p := DefaultPrice()
	tenants := []*model.UsageRecord{rec(model.UsageScopeTenant, "t1", 1)}
	nodeUsed := []*model.UsageRecord{rec(model.UsageScopeNodeUsed, "n1", 1)}
	nodeOnline := []*model.UsageRecord{rec(model.UsageScopeNode, "n1", 100)}

	rep := Compute(p, tenants, nodeUsed, nodeOnline)
	if rep.Totals.CommissionCNY < 0 {
		t.Fatalf("commission = %v, want >= 0 (payout must follow served capacity)", rep.Totals.CommissionCNY)
	}
	if got := rep.Totals.CommissionCNY; math.Abs(got-0.00008) > 1e-12 {
		t.Fatalf("commission = %v, want 0.00008", got)
	}
}

func TestComputeEmpty(t *testing.T) {
	rep := Compute(DefaultPrice(), nil, nil, nil)
	if rep.Tenants == nil || rep.Nodes == nil {
		t.Fatalf("empty result must use non-nil slices")
	}
	if len(rep.Tenants) != 0 || len(rep.Nodes) != 0 {
		t.Fatalf("empty result rows = %d/%d", len(rep.Tenants), len(rep.Nodes))
	}
}

func TestPriceNormalize(t *testing.T) {
	p := Price{}.Normalize()
	if p.CustomerPerGBHour != 0.0004 || p.SupplierPerGBHour != 0.00032 {
		t.Fatalf("normalize = %+v, want defaults", p)
	}
	// 负数同样回落。
	p = Price{CustomerPerGBHour: -1, SupplierPerGBHour: -1}.Normalize()
	if p.CustomerPerGBHour != 0.0004 {
		t.Fatalf("negative normalize = %+v", p)
	}
}
