// Package api 提供 REST 管理 API：节点注册/心跳、租户、订单与查询。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/xiancuncloud/idlemem-core/heartbeat"

	"github.com/xiancuncloud/idlemem-server/internal/billing"
	"github.com/xiancuncloud/idlemem-server/internal/meter"
	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/node"
	"github.com/xiancuncloud/idlemem-server/internal/scheduler"
	"github.com/xiancuncloud/idlemem-server/internal/settle"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

// Server REST API 服务。
type Server struct {
	store     *store.Store
	registry  *node.Registry
	scheduler *scheduler.Scheduler
	meter     *meter.Meter
	price     billing.Price
	settler   *settle.Settler
	mux       *http.ServeMux
	logger    *log.Logger

	// adminToken 管理面访问令牌；nodeToken 节点注册/心跳令牌。空表示不校验。
	adminToken string
	nodeToken  string
}

// New 创建 API 服务。price 为全零值时使用默认单价。
func New(s *store.Store, reg *node.Registry, sch *scheduler.Scheduler, mtr *meter.Meter, price billing.Price, logger *log.Logger) *Server {
	srv := &Server{
		store:     s,
		registry:  reg,
		scheduler: sch,
		meter:     mtr,
		price:     price.Normalize(),
		settler:   settle.New(s, price, logger),
		logger:    logger,
		mux:       http.NewServeMux(),
	}
	srv.routes()
	return srv
}

func (s *Server) routes() {
	// 节点侧接口
	s.mux.HandleFunc("POST /api/v1/nodes/register", s.handleNodeRegister)
	s.mux.HandleFunc("POST /api/v1/nodes/heartbeat", s.handleNodeHeartbeat)
	s.mux.HandleFunc("GET /api/v1/nodes", s.handleListNodes)

	// 租户接口
	s.mux.HandleFunc("POST /api/v1/tenants", s.handleCreateTenant)
	s.mux.HandleFunc("GET /api/v1/tenants", s.handleListTenants)
	s.mux.HandleFunc("POST /api/v1/tenants/{id}/suspend", s.handleSuspendTenant)
	s.mux.HandleFunc("POST /api/v1/tenants/{id}/resume", s.handleResumeTenant)

	// 订单接口
	s.mux.HandleFunc("POST /api/v1/orders", s.handleCreateOrder)
	s.mux.HandleFunc("GET /api/v1/orders", s.handleListOrders)

	// 分片查询
	s.mux.HandleFunc("GET /api/v1/shard", s.handleListShards)

	// 计量（GB·h，计费依据）
	s.mux.HandleFunc("GET /api/v1/usage/nodes", s.handleUsageNodes)
	s.mux.HandleFunc("GET /api/v1/usage/tenants", s.handleUsageTenants)

	// 计费账单（GB·h 折算金额：客户账单/节点结算/平台佣金）
	s.mux.HandleFunc("GET /api/v1/billing", s.handleBilling)

	// 账期出账：把增量用量出一张期账单（可反复调用，日结/月结均可）
	s.mux.HandleFunc("POST /api/v1/billing/settle", s.handleSettle)
	s.mux.HandleFunc("GET /api/v1/billing/invoices", s.handleListInvoices)
	s.mux.HandleFunc("GET /api/v1/billing/invoices/{id}", s.handleGetInvoice)
	s.mux.HandleFunc("GET /api/v1/billing/export.csv", s.handleExportCSV)

	// 客户自助：单租户计费状态（累计/已出账/待结算）
	s.mux.HandleFunc("GET /api/v1/tenants/{id}/billing", s.handleTenantBilling)
	// 供给端自助：单节点承载用量与收益
	s.mux.HandleFunc("GET /api/v1/nodes/{id}/earnings", s.handleNodeEarnings)

	// 健康检查与概览
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /api/v1/summary", s.handleSummary)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
}

// Handler 返回可挂载的 http.Handler。设置了令牌时自动启用鉴权中间件。
func (s *Server) Handler() http.Handler {
	if s.adminToken == "" && s.nodeToken == "" {
		return s.mux
	}
	return s.authMiddleware(s.mux)
}

// ---------- 节点 ----------

type registerReq struct {
	NodeID  string `json:"node_id"`
	Version string `json:"version"`
}

func (s *Server) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "node_id is required")
		return
	}
	ip := clientIP(r)
	n, err := s.registry.Register(req.NodeID, ip, req.Version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleNodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	var snap heartbeat.Snapshot
	if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if snap.NodeID == "" {
		writeError(w, http.StatusBadRequest, "node_id is required")
		return
	}
	ip := clientIP(r)
	n, err := s.registry.Heartbeat(snap, ip)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if nodes == nil {
		nodes = []*model.Node{}
	}
	writeJSON(w, http.StatusOK, nodes)
}

// ---------- 租户 ----------

type createTenantReq struct {
	Name       string `json:"name"`
	Token      string `json:"token"`
	QuotaBytes uint64 `json:"quota_bytes"`
}

func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	t := &model.Tenant{
		ID:         newID(),
		Name:       req.Name,
		Token:      req.Token,
		QuotaBytes: req.QuotaBytes,
		Status:     model.TenantActive,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	// Token 未提供时自动生成，保证企业客户可立即 AUTH 接入。
	if t.Token == "" {
		t.Token = randomToken()
	}
	if err := s.store.UpsertTenant(t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := s.store.ListTenants()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tenants == nil {
		tenants = []*model.Tenant{}
	}
	writeJSON(w, http.StatusOK, tenants)
}

// setTenantStatus 切换租户状态（停服/恢复），停服后网关鉴权立即拒绝。
func (s *Server) setTenantStatus(w http.ResponseWriter, r *http.Request, st model.TenantStatus) {
	id := r.PathValue("id")
	t, err := s.store.GetTenant(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if t == nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if t.Status == st {
		writeJSON(w, http.StatusOK, t)
		return
	}
	t.Status = st
	t.UpdatedAt = time.Now()
	if err := s.store.UpsertTenant(t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Printf("tenant %s status -> %s", t.ID, st)
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleSuspendTenant(w http.ResponseWriter, r *http.Request) {
	s.setTenantStatus(w, r, model.TenantSuspended)
}

func (s *Server) handleResumeTenant(w http.ResponseWriter, r *http.Request) {
	s.setTenantStatus(w, r, model.TenantActive)
}

// ---------- 订单 ----------

type createOrderReq struct {
	TenantID      string  `json:"tenant_id"`
	CapacityBytes uint64  `json:"capacity_bytes"`
	Replicas      int     `json:"replicas"`
	ExpireHours   float64 `json:"expire_hours"` // >0 时到期自动释放容量；0 表示长期
}

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	var req createOrderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.TenantID == "" || req.CapacityBytes == 0 {
		writeError(w, http.StatusBadRequest, "tenant_id and capacity_bytes are required")
		return
	}
	ten, err := s.store.GetTenant(req.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ten == nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if ten.Status == model.TenantSuspended {
		writeError(w, http.StatusConflict, "tenant suspended")
		return
	}
	// 配额校验：活跃订单容量之和 + 本次容量不得超过配额（0 表示不限）。
	if ten.QuotaBytes > 0 {
		orders, lerr := s.store.ListOrders()
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, lerr.Error())
			return
		}
		var used uint64
		for _, o := range orders {
			if o.TenantID == req.TenantID && o.Status == model.OrderActive {
				used += o.CapacityBytes
			}
		}
		if used+req.CapacityBytes > ten.QuotaBytes {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("quota exceeded: used=%d requested=%d quota=%d", used, req.CapacityBytes, ten.QuotaBytes))
			return
		}
	}
	o := &model.Order{
		ID:            newID(),
		TenantID:      req.TenantID,
		CapacityBytes: req.CapacityBytes,
		Replicas:      req.Replicas,
		Status:        model.OrderActive,
		CreatedAt:     time.Now(),
	}
	if req.ExpireHours > 0 {
		o.ExpireAt = time.Now().Add(time.Duration(req.ExpireHours * float64(time.Hour)))
	}
	if err := s.store.UpsertOrder(o); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	shards, err := s.scheduler.PlaceOrder(o)
	if err != nil {
		// 分配失败：订单保留但无分片，调用方稍后重试。
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"order":  o,
		"shards": shards,
	})
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.store.ListOrders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if orders == nil {
		orders = []*model.Order{}
	}
	writeJSON(w, http.StatusOK, orders)
}

func (s *Server) handleListShards(w http.ResponseWriter, r *http.Request) {
	shards, err := s.store.ListShards()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if shards == nil {
		shards = []*model.Shard{}
	}
	writeJSON(w, http.StatusOK, shards)
}

// ---------- 概览 ----------

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.store.ListNodes()
	orders, _ := s.store.ListOrders()
	shards, _ := s.store.ListShards()

	var online, offline, suspended int
	var totalCap, usedCap uint64
	for _, n := range nodes {
		totalCap += n.CapacityBytes
		usedCap += n.UsedBytes
		switch n.Status {
		case model.NodeOnline:
			online++
		case model.NodeOffline:
			offline++
		case model.NodeSuspended:
			suspended++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": map[string]any{
			"total":     len(nodes),
			"online":    online,
			"suspended": suspended,
			"offline":   offline,
			"capacity":  totalCap,
			"used":      usedCap,
		},
		"orders": len(orders),
		"shards": len(shards),
		"time":   time.Now().Format(time.RFC3339),
	})
}

// handleMetrics 输出 Prometheus 文本格式指标，供 Prometheus/Grafana 抓取。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.store.ListNodes()
	orders, _ := s.store.ListOrders()
	shards, _ := s.store.ListShards()

	statusCount := map[model.NodeStatus]int{}
	var capBytes, usedBytes uint64
	for _, n := range nodes {
		statusCount[n.Status]++
		capBytes += n.CapacityBytes
		usedBytes += n.UsedBytes
	}
	orderCount := map[model.OrderStatus]int{}
	var orderedBytes uint64
	for _, o := range orders {
		orderCount[o.Status]++
		if o.Status == model.OrderActive {
			orderedBytes += o.CapacityBytes
		}
	}
	shardCount := map[model.ShardStatus]int{}
	for _, sh := range shards {
		shardCount[sh.Status]++
	}

	var b strings.Builder
	metric := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	metric("xiancun_nodes_total", "节点数量（按状态）", "gauge")
	for _, st := range []model.NodeStatus{model.NodeOnline, model.NodeOffline, model.NodeSuspended} {
		fmt.Fprintf(&b, "xiancun_nodes_total{status=%q} %d\n", st, statusCount[st])
	}
	metric("xiancun_node_capacity_bytes", "节点上报的可贡献容量合计", "gauge")
	fmt.Fprintf(&b, "xiancun_node_capacity_bytes %d\n", capBytes)
	metric("xiancun_node_used_bytes", "节点已用内存合计", "gauge")
	fmt.Fprintf(&b, "xiancun_node_used_bytes %d\n", usedBytes)

	metric("xiancun_orders_total", "订单数量（按状态）", "gauge")
	for _, st := range []model.OrderStatus{model.OrderActive, model.OrderDegraded, model.OrderExpired} {
		fmt.Fprintf(&b, "xiancun_orders_total{status=%q} %d\n", st, orderCount[st])
	}
	metric("xiancun_ordered_capacity_bytes", "已售出（活跃订单）容量合计", "gauge")
	fmt.Fprintf(&b, "xiancun_ordered_capacity_bytes %d\n", orderedBytes)

	metric("xiancun_shards_total", "分片数量（按状态）", "gauge")
	for _, st := range []model.ShardStatus{model.ShardActive, model.ShardLost} {
		fmt.Fprintf(&b, "xiancun_shards_total{status=%q} %d\n", st, shardCount[st])
	}

	if s.meter != nil {
		metric("xiancun_usage_gb_hours", "累计计量 GB·h（scope=tenant/node_used/node）", "counter")
		for _, scope := range []model.UsageScope{model.UsageScopeTenant, model.UsageScopeNodeUsed, model.UsageScopeNode} {
			recs, err := s.meter.Snapshot(scope)
			if err != nil {
				continue
			}
			var sum float64
			for _, rec := range recs {
				sum += rec.GBHours
			}
			fmt.Fprintf(&b, "xiancun_usage_gb_hours{scope=%q} %.10f\n", scope, sum)
		}
	}

	metric("xiancun_price_cny_per_gb_hour", "当前单价（role=customer/supplier）", "gauge")
	fmt.Fprintf(&b, "xiancun_price_cny_per_gb_hour{role=\"customer\"} %g\n", s.price.CustomerPerGBHour)
	fmt.Fprintf(&b, "xiancun_price_cny_per_gb_hour{role=\"supplier\"} %g\n", s.price.SupplierPerGBHour)

	metric("xiancun_up", "控制面存活标识", "gauge")
	b.WriteString("xiancun_up 1\n")

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// ---------- 工具 ----------

// ---------- 计量 ----------

// handleUsageNodes 返回节点维度 GB·h 计量（供给结算依据）。
func (s *Server) handleUsageNodes(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not enabled")
		return
	}
	recs, err := s.meter.Snapshot(model.UsageScopeNode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if recs == nil {
		recs = []*model.UsageRecord{}
	}
	writeJSON(w, http.StatusOK, recs)
}

// handleUsageTenants 返回租户维度 GB·h 计量（客户计费依据）。
func (s *Server) handleUsageTenants(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not enabled")
		return
	}
	recs, err := s.meter.Snapshot(model.UsageScopeTenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if recs == nil {
		recs = []*model.UsageRecord{}
	}
	writeJSON(w, http.StatusOK, recs)
}

// handleBilling 返回按单价折算的计费报告（客户账单/节点结算/平台佣金）。
func (s *Server) handleBilling(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not enabled")
		return
	}
	tenantUsage, err := s.meter.Snapshot(model.UsageScopeTenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "tenant usage: "+err.Error())
		return
	}
	nodeUsage, err := s.meter.Snapshot(model.UsageScopeNode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node usage: "+err.Error())
		return
	}
	nodeUsed, err := s.meter.Snapshot(model.UsageScopeNodeUsed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "node used usage: "+err.Error())
		return
	}
	rep := billing.Compute(s.price, tenantUsage, nodeUsed, nodeUsage)
	rep.GeneratedAt = time.Now().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, rep)
}

// handleSettle 出账：结算「累计用量 − 已结算水位」，返回期账单。
func (s *Server) handleSettle(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not enabled")
		return
	}
	inv, err := s.settler.Settle(time.Now())
	if errors.Is(err, settle.ErrNothingToBill) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

func (s *Server) handleListInvoices(w http.ResponseWriter, r *http.Request) {
	inv, err := s.settler.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if inv == nil {
		inv = []*settle.Invoice{}
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *Server) handleGetInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inv, err := s.settler.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "invoice not found")
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

// handleExportCSV 导出对账明细（客户账单 + 节点结算，统一表结构），带 BOM 便于 Excel 直接打开。
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	rows, totals, err := s.settler.ExportRows()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var b strings.Builder
	b.WriteString("\uFEFF") // UTF-8 BOM：Excel 打开中文表头不乱码
	b.WriteString("type,object_id,gb_hours,unit_price_cny,amount_cny\n")
	writeRow := func(row settle.ExportRow) {
		fmt.Fprintf(&b, "%s,%s,%.6f,%.6f,%.10f\n", row.Type, row.ObjectID, row.GBHours, row.UnitPriceCNY, row.AmountCNY)
	}
	for _, row := range rows {
		writeRow(row)
	}
	if len(rows) > 0 {
		writeRow(totals)
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="xiancun-billing-export.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// handleTenantBilling 客户自助：返回某租户的计量与费用状态（含已出账/待结算）。
func (s *Server) handleTenantBilling(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.store.GetTenant(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if t == nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if s.meter == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not enabled")
		return
	}
	sum, err := s.settler.TenantSummary(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":  map[string]any{"id": t.ID, "name": t.Name, "quota_bytes": t.QuotaBytes, "created_at": t.CreatedAt},
		"billing": sum,
	})
}

// handleNodeEarnings 供给端自助：返回某节点的承载用量与收益（已结算/待结算）。
func (s *Server) handleNodeEarnings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.store.GetNode(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == nil {
		writeError(w, http.StatusNotFound, "node not found")
		return
	}
	if s.meter == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not enabled")
		return
	}
	sum, err := s.settler.NodeSummary(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node": map[string]any{
			"id": n.ID, "status": n.Status,
			"capacity_bytes": n.CapacityBytes, "used_bytes": n.UsedBytes,
		},
		"earnings": sum,
	})
}

func clientIP(r *http.Request) string {
	// 优先 X-Forwarded-For / X-Real-IP（反代场景），否则取远端地址。
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func newID() string {
	return "id-" + time.Now().Format("20060102150405") + "-" +
		strings.ReplaceAll(time.Now().Format("15.04.05.000000000"), ".", "")
}

// randomToken 生成 16 字节随机 token（hex 32 字符）。
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极低概率失败时退化为时间戳 token。
		return newID()
	}
	return hex.EncodeToString(b)
}
