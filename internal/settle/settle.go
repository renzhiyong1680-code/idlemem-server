// Package settle 负责账期出账：把累计 GB·h 转为一张期账单，并用「水位」标记已结算部分。
//
// 设计要点：
//   - 累计计量记录只增不减，历史全部保留可查；
//   - 每次出账只结算「当前累计 − 已结算水位」，因此可反复调用、支持日结/月结；
//   - 出账与水位推进在同一流程内完成，重复调用不会重复计费。
package settle

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/xiancuncloud/idlemem-server/internal/billing"
	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

// ErrNothingToBill 没有新增可结算用量时返回。
var ErrNothingToBill = errors.New("no unbilled usage in current period")

// Invoice 一张期账单。
type Invoice struct {
	ID          string                   `json:"id"`
	PeriodStart string                   `json:"period_start,omitempty"`
	PeriodEnd   string                   `json:"period_end"`
	Price       billing.Price            `json:"price"`
	Tenants     []billing.TenantBill     `json:"tenants"`
	Nodes       []billing.NodeSettlement `json:"nodes"`
	Totals      billing.Totals           `json:"totals"`
	CreatedAt   string                   `json:"created_at"`
}

// Settler 出账器。
type Settler struct {
	store  *store.Store
	price  billing.Price
	logger *log.Logger
	mu     sync.Mutex // 串行化出账，避免并发重复结算
}

// New 创建出账器。price 为零值时使用默认单价。
func New(st *store.Store, price billing.Price, logger *log.Logger) *Settler {
	return &Settler{store: st, price: price.Normalize(), logger: logger}
}

// Price 返回当前生效单价。
func (s *Settler) Price() billing.Price { return s.price }

// Settle 结算一个账期并返回期账单。
//
// 用量口径三项（tenant / node_used / node）分别与结算水位对比，
// 只有超出水位的增量参与本账单；在线供给（node）只进入 SupplyGBHours 参考值。
func (s *Settler) Settle(now time.Time) (*Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	deltas, periodStart, err := s.collectDeltas()
	if err != nil {
		return nil, err
	}
	if len(deltas.tenant) == 0 && len(deltas.nodeUsed) == 0 && len(deltas.node) == 0 {
		return nil, ErrNothingToBill
	}

	rep := billing.Compute(s.price, deltas.tenant, deltas.nodeUsed, deltas.node)
	inv := &Invoice{
		ID:        "inv-" + now.Format("20060102-150405") + "-" + randSuffix(),
		PeriodEnd: now.Format(time.RFC3339),
		Price:     s.price,
		Tenants:   rep.Tenants,
		Nodes:     rep.Nodes,
		Totals:    rep.Totals,
		CreatedAt: now.Format(time.RFC3339),
	}
	if !periodStart.IsZero() {
		inv.PeriodStart = periodStart.Format(time.RFC3339)
	}

	payload, err := json.Marshal(inv)
	if err != nil {
		return nil, err
	}
	if err := s.store.UpsertInvoice(inv.ID, payload); err != nil {
		return nil, err
	}

	// 推进水位到当前累计值：本次已结算的用量不会进入下一张账单。
	marks := map[string]store.Baseline{}
	mark := func(scope model.UsageScope, recs []*model.UsageRecord) {
		for _, r := range recs {
			cur, err := s.store.GetUsage(scope, r.ID)
			if err != nil || cur == nil {
				continue
			}
			marks[string(scope)+":"+r.ID] = store.Baseline{GBHours: cur.GBHours, At: now}
		}
	}
	mark(model.UsageScopeTenant, deltas.tenant)
	mark(model.UsageScopeNodeUsed, deltas.nodeUsed)
	mark(model.UsageScopeNode, deltas.node)
	if err := s.store.SetBaselines(marks); err != nil {
		return nil, err
	}

	s.logger.Printf("settle: invoice %s issued (revenue=%.10f payout=%.10f commission=%.10f)",
		inv.ID, rep.Totals.CustomerRevenueCNY, rep.Totals.SupplierPayoutCNY, rep.Totals.CommissionCNY)
	return inv, nil
}

type usageDeltas struct {
	tenant   []*model.UsageRecord
	nodeUsed []*model.UsageRecord
	node     []*model.UsageRecord
}

// collectDeltas 计算各 scope 相对结算水位的增量，并给出账期起点。
func (s *Settler) collectDeltas() (usageDeltas, time.Time, error) {
	var out usageDeltas
	var earliest time.Time
	seen := false

	collect := func(scope model.UsageScope) ([]*model.UsageRecord, error) {
		cur, err := s.store.ListUsage(scope)
		if err != nil {
			return nil, err
		}
		base, err := s.store.ListBaselines(scope)
		if err != nil {
			return nil, err
		}
		recs := make([]*model.UsageRecord, 0, len(cur))
		for _, r := range cur {
			if r == nil || r.GBHours <= 0 {
				continue
			}
			b := base[r.ID]
			delta := r.GBHours - b.GBHours
			if delta <= 0 {
				continue
			}
			recs = append(recs, &model.UsageRecord{Scope: scope, ID: r.ID, GBHours: delta, UpdatedAt: r.UpdatedAt})
			if b.At.After(time.Time{}) && (!seen || b.At.Before(earliest)) {
				earliest = b.At
				seen = true
			}
		}
		return recs, nil
	}

	var err error
	if out.tenant, err = collect(model.UsageScopeTenant); err != nil {
		return out, earliest, err
	}
	if out.nodeUsed, err = collect(model.UsageScopeNodeUsed); err != nil {
		return out, earliest, err
	}
	if out.node, err = collect(model.UsageScopeNode); err != nil {
		return out, earliest, err
	}
	return out, earliest, nil
}

// List 返回历史期账单（按生成时间升序）。
func (s *Settler) List() ([]*Invoice, error) {
	raws, err := s.store.ListInvoices()
	if err != nil {
		return nil, err
	}
	inv := make([]*Invoice, 0, len(raws))
	for _, raw := range raws {
		var v Invoice
		if err := json.Unmarshal(raw, &v); err != nil {
			continue // 跳过损坏记录，不影响其他账单可读
		}
		inv = append(inv, &v)
	}
	sort.SliceStable(inv, func(i, j int) bool { return inv[i].CreatedAt < inv[j].CreatedAt })
	return inv, nil
}

// Get 按 ID 读取期账单；不存在返回 nil。
func (s *Settler) Get(id string) (*Invoice, error) {
	raw, err := s.store.GetInvoice(id)
	if err != nil || raw == nil {
		return nil, err
	}
	var v Invoice
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode invoice %s: %w", id, err)
	}
	return &v, nil
}

// TenantSummary 单个租户的自助账单视图：累计用量、已出账部分、待结算部分。
type TenantSummary struct {
	TenantID          string        `json:"tenant_id"`
	Price             billing.Price `json:"price"`
	TotalGBHours      float64       `json:"total_gb_hours"`      // 累计计量
	InvoicedGBHours   float64       `json:"invoiced_gb_hours"`   // 已出账
	PendingGBHours    float64       `json:"pending_gb_hours"`    // 待出账
	PendingAmountCNY  float64       `json:"pending_amount_cny"`  // 待出账金额
	InvoicedAmountCNY float64       `json:"invoiced_amount_cny"` // 已出账金额
	InvoiceCount      int           `json:"invoice_count"`
}

// TenantSummary 汇总某租户的计费状态，供客户自助查询。
func (s *Settler) TenantSummary(tenantID string) (*TenantSummary, error) {
	out := &TenantSummary{TenantID: tenantID, Price: s.price}
	rec, err := s.store.GetUsage(model.UsageScopeTenant, tenantID)
	if err != nil {
		return nil, err
	}
	if rec != nil {
		out.TotalGBHours = rec.GBHours
	}

	invs, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, inv := range invs {
		for _, b := range inv.Tenants {
			if b.TenantID != tenantID {
				continue
			}
			out.InvoicedGBHours += b.GBHours
			out.InvoicedAmountCNY += b.AmountCNY
			out.InvoiceCount++
		}
	}

	out.PendingGBHours = out.TotalGBHours - out.InvoicedGBHours
	if out.PendingGBHours < 0 {
		// 出账与计量的采样时点存在微小差异，钳到 0 避免负账单。
		out.PendingGBHours = 0
	}
	out.PendingAmountCNY = out.PendingGBHours * s.price.CustomerPerGBHour
	return out, nil
}

// ExportRow 对账 CSV 的一行（客户账单与节点结算共用同一张表）。
type ExportRow struct {
	Type         string  `json:"type"`
	ObjectID     string  `json:"object_id"`
	GBHours      float64 `json:"gb_hours"`
	UnitPriceCNY float64 `json:"unit_price_cny"`
	AmountCNY    float64 `json:"amount_cny"`
}

// ExportRows 导出当前累计的对账明细（全部租户 + 全部节点承载），供财务核对。
func (s *Settler) ExportRows() (rows []ExportRow, totals ExportRow, err error) {
	tenantUsage, err := s.store.ListUsage(model.UsageScopeTenant)
	if err != nil {
		return nil, totals, err
	}
	nodeUsed, err := s.store.ListUsage(model.UsageScopeNodeUsed)
	if err != nil {
		return nil, totals, err
	}
	for _, u := range tenantUsage {
		if u == nil || u.GBHours <= 0 {
			continue
		}
		rows = append(rows, ExportRow{
			Type:         "tenant",
			ObjectID:     u.ID,
			GBHours:      u.GBHours,
			UnitPriceCNY: s.price.CustomerPerGBHour,
			AmountCNY:    u.GBHours * s.price.CustomerPerGBHour,
		})
	}
	for _, u := range nodeUsed {
		if u == nil || u.GBHours <= 0 {
			continue
		}
		rows = append(rows, ExportRow{
			Type:         "node",
			ObjectID:     u.ID,
			GBHours:      u.GBHours,
			UnitPriceCNY: s.price.SupplierPerGBHour,
			AmountCNY:    u.GBHours * s.price.SupplierPerGBHour,
		})
	}
	for _, r := range rows {
		totals.GBHours += r.GBHours
		totals.AmountCNY += r.AmountCNY
	}
	totals.Type = "total"
	totals.ObjectID = fmt.Sprintf("rows=%d", len(rows))
	return rows, totals, nil
}

func randSuffix() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}
