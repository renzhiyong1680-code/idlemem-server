// Package billing 把 GB·h 计量结果折算为账单/结算单。
//
// 口径（与计量一致）：
//   - 客户账单：租户维度活跃分片 GB·h × 客户单价（元/GB·h）；
//   - 节点结算：节点承载维度（实际承载的活跃分片）GB·h × 供给单价（元/GB·h）；
//     只对已售出并实际承载的容量付费，供大于求时不会出现结算倒挂；
//   - 平台佣金：客户收入 − 供给支出（默认 0.00008 元/GB·h，约 20%）。
//   - 在线供给 GB·h（节点维度）仅作为供给能力参考，不参与结算。
package billing

import (
	"sort"

	"github.com/xiancuncloud/idlemem-server/internal/model"
)

// Price 计费单价（元/GB·h）。
type Price struct {
	CustomerPerGBHour float64 `json:"customer_per_gb_hour"`
	SupplierPerGBHour float64 `json:"supplier_per_gb_hour"`
}

// DefaultPrice 返回商业计划口径的默认价格。
func DefaultPrice() Price {
	return Price{
		CustomerPerGBHour: 0.0004,
		SupplierPerGBHour: 0.00032,
	}
}

// CommissionPerGBHour 平台每 GB·h 佣金（客户售价 − 供给结算价）。
func (p Price) CommissionPerGBHour() float64 {
	return p.CustomerPerGBHour - p.SupplierPerGBHour
}

// Normalize 价格缺省或为负时回落到默认值，避免配置漏填导致零价账单。
func (p Price) Normalize() Price {
	def := DefaultPrice()
	if p.CustomerPerGBHour <= 0 {
		p.CustomerPerGBHour = def.CustomerPerGBHour
	}
	if p.SupplierPerGBHour <= 0 {
		p.SupplierPerGBHour = def.SupplierPerGBHour
	}
	return p
}

// TenantBill 租户账单行（客户应付）。
type TenantBill struct {
	TenantID  string  `json:"tenant_id"`
	GBHours   float64 `json:"gb_hours"`
	AmountCNY float64 `json:"amount_cny"`
}

// NodeSettlement 节点结算行（节点主人应收）。
type NodeSettlement struct {
	NodeID    string  `json:"node_id"`
	GBHours   float64 `json:"gb_hours"`
	PayoutCNY float64 `json:"payout_cny"`
}

// Totals 全平台汇总。
type Totals struct {
	TenantGBHours      float64 `json:"tenant_gb_hours"`
	NodeGBHours        float64 `json:"node_gb_hours"`   // 节点承载（结算依据）
	SupplyGBHours      float64 `json:"supply_gb_hours"` // 在线供给（能力参考，不参与结算）
	CustomerRevenueCNY float64 `json:"customer_revenue_cny"`
	SupplierPayoutCNY  float64 `json:"supplier_payout_cny"`
	CommissionCNY      float64 `json:"commission_cny"`
}

// Report 全平台计费报告。
type Report struct {
	Price   Price            `json:"price"`
	Tenants []TenantBill     `json:"tenants"`
	Nodes   []NodeSettlement `json:"nodes"`
	Totals  Totals           `json:"totals"`
	// GeneratedAt 报告生成时间（RFC3339）。
	GeneratedAt string `json:"generated_at"`
}

// Compute 由计量记录生成计费报告。
// tenantUsage 为客户账单依据，nodeSettleUsage（node_used）为供给结算依据，
// nodeOnlineUsage（node）只累计为供给能力参考，不参与结算。
func Compute(p Price, tenantUsage, nodeSettleUsage, nodeOnlineUsage []*model.UsageRecord) Report {
	p = p.Normalize()
	rep := Report{
		Price:   p,
		Tenants: []TenantBill{},
		Nodes:   []NodeSettlement{},
	}

	for _, u := range tenantUsage {
		if u == nil {
			continue
		}
		amount := u.GBHours * p.CustomerPerGBHour
		rep.Tenants = append(rep.Tenants, TenantBill{
			TenantID:  u.ID,
			GBHours:   u.GBHours,
			AmountCNY: amount,
		})
		rep.Totals.TenantGBHours += u.GBHours
		rep.Totals.CustomerRevenueCNY += amount
	}

	for _, u := range nodeSettleUsage {
		if u == nil {
			continue
		}
		payout := u.GBHours * p.SupplierPerGBHour
		rep.Nodes = append(rep.Nodes, NodeSettlement{
			NodeID:    u.ID,
			GBHours:   u.GBHours,
			PayoutCNY: payout,
		})
		rep.Totals.NodeGBHours += u.GBHours
		rep.Totals.SupplierPayoutCNY += payout
	}

	for _, u := range nodeOnlineUsage {
		if u == nil {
			continue
		}
		rep.Totals.SupplyGBHours += u.GBHours
	}

	rep.Totals.CommissionCNY = rep.Totals.CustomerRevenueCNY - rep.Totals.SupplierPayoutCNY

	// 按金额降序，方便运营快速看到主要客户/主要供给节点。
	sort.Slice(rep.Tenants, func(i, j int) bool { return rep.Tenants[i].AmountCNY > rep.Tenants[j].AmountCNY })
	sort.Slice(rep.Nodes, func(i, j int) bool { return rep.Nodes[i].PayoutCNY > rep.Nodes[j].PayoutCNY })

	return rep
}
