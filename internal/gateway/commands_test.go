package gateway

import (
	"strings"
	"testing"
)

// TestGateway_MGetMSetIncrExists 覆盖客户端常用的批量与计数命令。
func TestGateway_MGetMSetIncrExists(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)

	c := newRESPClient(t, addr)
	if got, err := c.cmd("AUTH", "tok-1"); err != nil || got != "+OK" {
		t.Fatalf("auth: %q, %v", got, err)
	}

	// MSET 批量写。
	if got, err := c.cmd("MSET", "a", "1", "b", "2"); err != nil || got != "+OK" {
		t.Fatalf("mset: %q, %v", got, err)
	}
	// 参数不成对必须报错。
	if got, _ := c.cmd("MSET", "x"); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("mset with odd args should fail: %q", got)
	}

	// MGET 一次取多个，缺失的 key 返回 null。
	got, err := c.cmd("MGET", "a", "b", "missing")
	if err != nil {
		t.Fatalf("mget: %v", err)
	}
	if !strings.HasPrefix(got, "*3") {
		t.Fatalf("mget should return 3 elements: %q", got)
	}
	if !strings.Contains(got, "$1\r\n1") || !strings.Contains(got, "$1\r\n2") || !strings.Contains(got, "$-1") {
		t.Fatalf("mget payload unexpected: %q", got)
	}

	// EXISTS 计数。
	if got, _ := c.cmd("EXISTS", "a", "b", "missing"); got != ":2" {
		t.Fatalf("exists: %q", got)
	}

	// INCR 从 0 开始，连续自增。
	if got, _ := c.cmd("INCR", "counter"); got != ":1" {
		t.Fatalf("first incr: %q", got)
	}
	if got, _ := c.cmd("INCR", "counter"); got != ":2" {
		t.Fatalf("second incr: %q", got)
	}

	// 非数字值自增必须报错，且不破坏原值。
	if _, err := c.cmd("SET", "word", "abc"); err != nil {
		t.Fatalf("set word: %v", err)
	}
	if got, _ := c.cmd("INCR", "word"); !strings.HasPrefix(got, "-ERR value is not an integer") {
		t.Fatalf("incr non-integer: %q", got)
	}
	if got, _ := c.cmd("GET", "word"); !strings.Contains(got, "abc") {
		t.Fatalf("word value should be intact: %q", got)
	}

	// 批量写同样受容量约束：整批超额时一个都不写。
	ten, err := g.store.GetTenant("tenant-1")
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	ten.QuotaBytes = 20
	if err := g.store.UpsertTenant(ten); err != nil {
		t.Fatalf("update tenant: %v", err)
	}
	g.resetCachesForTest(ten.ID)

	if got, _ := c.cmd("MSET", "big1", strings.Repeat("z", 30), "big2", strings.Repeat("z", 30)); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("mset beyond capacity should fail: %q", got)
	}
	if got, _ := c.cmd("EXISTS", "big1", "big2"); got != ":0" {
		t.Fatalf("failed mset must not write anything: %q", got)
	}
}
