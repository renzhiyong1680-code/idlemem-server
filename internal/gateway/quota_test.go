package gateway

import (
	"strings"
	"testing"
)

// TestGateway_CapacityLimit 验证"买了多少就能写多少"：覆盖写不重复计费、
// 删除后释放、无配额（quota=0 且无订单）时不限制。
func TestGateway_CapacityLimit(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)

	ten, err := g.store.GetTenant("tenant-1")
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	ten.QuotaBytes = 100
	if err := g.store.UpsertTenant(ten); err != nil {
		t.Fatalf("update tenant: %v", err)
	}
	// 清掉容量缓存，让新的配额立即生效。
	g.quota.mu.Lock()
	delete(g.quota.capAt, ten.ID)
	g.quota.mu.Unlock()

	c := newRESPClient(t, addr)
	if got, err := c.cmd("AUTH", "tok-1"); err != nil || got != "+OK" {
		t.Fatalf("auth: %q, %v", got, err)
	}

	b60 := strings.Repeat("a", 60)
	b100 := strings.Repeat("b", 100)

	if got, _ := c.cmd("SET", "k1", b60); got != "+OK" {
		t.Fatalf("first set should pass: %q", got)
	}
	// 60 + 60 > 100，必须拒绝。
	if got, _ := c.cmd("SET", "k2", b60); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("second set should be rejected: %q", got)
	}

	// 覆盖同一个 key（等长）不增加占用。
	if got, _ := c.cmd("SET", "k1", b60); got != "+OK" {
		t.Fatalf("overwrite with same size should pass: %q", got)
	}
	// 覆盖成大值刚好到上限。
	if got, _ := c.cmd("SET", "k1", b100); got != "+OK" {
		t.Fatalf("overwrite up to limit should pass: %q", got)
	}
	// 已经满额，新 key 1 字节也不允许。
	if got, _ := c.cmd("SET", "k3", "x"); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("set beyond limit should be rejected: %q", got)
	}

	// 删除后释放，可以再写。
	if got, _ := c.cmd("DEL", "k1"); got != ":1" {
		t.Fatalf("del: %q", got)
	}
	if got, _ := c.cmd("SET", "k3", "x"); got != "+OK" {
		t.Fatalf("set after delete should pass: %q", got)
	}
}

// TestGateway_NoQuotaUnlimited 未配置配额（quota=0、无订单）时不做写入限制，
// 保证本地开发与试用体验。
func TestGateway_NoQuotaUnlimited(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)

	c := newRESPClient(t, addr)
	if got, err := c.cmd("AUTH", "tok-1"); err != nil || got != "+OK" {
		t.Fatalf("auth: %q, %v", got, err)
	}
	if got, _ := c.cmd("SET", "big", strings.Repeat("x", 2048)); got != "+OK" {
		t.Fatalf("unlimited set: %q", got)
	}
}
