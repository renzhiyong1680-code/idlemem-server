package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xiancuncloud/idlemem-core/heartbeat"

	"github.com/xiancuncloud/idlemem-server/internal/billing"
	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/node"
	"github.com/xiancuncloud/idlemem-server/internal/scheduler"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := node.New(st, 30*time.Second, time.Hour)
	sch := scheduler.New(st, reg, 1)
	srv := New(st, reg, sch, nil, billing.Price{}, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestRegisterAndHeartbeat(t *testing.T) {
	ts := newTestServer(t)

	resp, _ := postJSON(t, ts.URL+"/api/v1/nodes/register", map[string]any{
		"node_id": "node-1",
		"version": "0.1.0",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d", resp.StatusCode)
	}

	resp, out := postJSON(t, ts.URL+"/api/v1/nodes/heartbeat", heartbeat.Snapshot{
		NodeID:        "node-1",
		Version:       "0.1.0",
		Status:        heartbeat.StatusOnline,
		CapacityBytes: 8 << 30,
		TotalBytes:    32 << 30,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat status = %d", resp.StatusCode)
	}
	if out["status"] != string(model.NodeOnline) {
		t.Fatalf("status = %v, want online", out["status"])
	}
	if cap := out["capacity_bytes"].(float64); cap != float64(8<<30) {
		t.Fatalf("capacity = %v", cap)
	}
}

func TestCreateTenantAndOrder(t *testing.T) {
	ts := newTestServer(t)

	// 先注册一个在线节点。
	postJSON(t, ts.URL+"/api/v1/nodes/heartbeat", heartbeat.Snapshot{
		NodeID:        "node-1",
		Version:       "0.1.0",
		Status:        heartbeat.StatusOnline,
		CapacityBytes: 16 << 30,
	})

	// 建租户。
	resp, tOut := postJSON(t, ts.URL+"/api/v1/tenants", map[string]any{
		"name":  "test-tenant",
		"token": "secret-token",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tenant status = %d", resp.StatusCode)
	}
	tenantID := tOut["id"].(string)

	// 建订单（分配分片）。
	resp, oOut := postJSON(t, ts.URL+"/api/v1/orders", map[string]any{
		"tenant_id":      tenantID,
		"capacity_bytes": 2 << 30,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order status = %d, body=%v", resp.StatusCode, oOut)
	}
	shards, ok := oOut["shards"].([]any)
	if !ok || len(shards) == 0 {
		t.Fatalf("no shards allocated: %v", oOut)
	}
}

func TestCreateOrder_NoNodes(t *testing.T) {
	ts := newTestServer(t)
	resp, tOut := postJSON(t, ts.URL+"/api/v1/tenants", map[string]any{"name": "x"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tenant: %d", resp.StatusCode)
	}
	tenantID := tOut["id"].(string)

	resp, _ = postJSON(t, ts.URL+"/api/v1/orders", map[string]any{
		"tenant_id":      tenantID,
		"capacity_bytes": 1 << 30,
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("order without nodes status = %d, want 503", resp.StatusCode)
	}
}

func TestSummary(t *testing.T) {
	ts := newTestServer(t)
	postJSON(t, ts.URL+"/api/v1/nodes/heartbeat", heartbeat.Snapshot{
		NodeID:        "node-1",
		Version:       "0.1.0",
		Status:        heartbeat.StatusOnline,
		CapacityBytes: 8 << 30,
	})

	resp, err := http.Get(ts.URL + "/api/v1/summary")
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("summary status = %d", resp.StatusCode)
	}
}
