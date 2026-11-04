// Package api 提供 REST 管理 API：节点注册/心跳、租户、订单与查询。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/xiancuncloud/idlemem-core/heartbeat"

	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/node"
	"github.com/xiancuncloud/idlemem-server/internal/scheduler"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

// Server REST API 服务。
type Server struct {
	store     *store.Store
	registry  *node.Registry
	scheduler *scheduler.Scheduler
	mux       *http.ServeMux
	logger    *log.Logger
}

// New 创建 API 服务。
func New(s *store.Store, reg *node.Registry, sch *scheduler.Scheduler, logger *log.Logger) *Server {
	srv := &Server{
		store:     s,
		registry:  reg,
		scheduler: sch,
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

	// 订单接口
	s.mux.HandleFunc("POST /api/v1/orders", s.handleCreateOrder)
	s.mux.HandleFunc("GET /api/v1/orders", s.handleListOrders)

	// 分片查询
	s.mux.HandleFunc("GET /api/v1/shard", s.handleListShards)

	// 健康检查与概览
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /api/v1/summary", s.handleSummary)
}

// Handler 返回可挂载的 http.Handler。
func (s *Server) Handler() http.Handler { return s.mux }

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
		CreatedAt:  time.Now(),
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

// ---------- 订单 ----------

type createOrderReq struct {
	TenantID      string `json:"tenant_id"`
	CapacityBytes uint64 `json:"capacity_bytes"`
	Replicas      int    `json:"replicas"`
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
	o := &model.Order{
		ID:            newID(),
		TenantID:      req.TenantID,
		CapacityBytes: req.CapacityBytes,
		Replicas:      req.Replicas,
		Status:        model.OrderActive,
		CreatedAt:     time.Now(),
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
		"orders":  len(orders),
		"shards":  len(shards),
		"time":    time.Now().Format(time.RFC3339),
	})
}

// ---------- 工具 ----------

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
