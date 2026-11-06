package gateway

import (
	"strconv"
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

// TestGateway_SetExGetSetAppendStrlenType 覆盖会话缓存类客户端最常用的几条命令。
func TestGateway_SetExGetSetAppendStrlenType(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)

	c := newRESPClient(t, addr)
	if got, err := c.cmd("AUTH", "tok-1"); err != nil || got != "+OK" {
		t.Fatalf("auth: %q, %v", got, err)
	}

	// SETEX 写入并带过期：TTL 应为 1..10 之间的正整数。
	if got, err := c.cmd("SETEX", "sess", "10", "v1"); err != nil || got != "+OK" {
		t.Fatalf("setex: %q, %v", got, err)
	}
	ttlReply, _ := c.cmd("TTL", "sess")
	n, err := strconv.Atoi(strings.TrimPrefix(ttlReply, ":"))
	if err != nil || n < 1 || n > 10 {
		t.Fatalf("setex should set a ~10s ttl, got %q", ttlReply)
	}

	// 过期秒数非法必须拒绝。
	if got, _ := c.cmd("SETEX", "bad", "0", "x"); !strings.HasPrefix(got, "-ERR invalid expire time") {
		t.Fatalf("setex with zero seconds should fail: %q", got)
	}

	// GETSET 返回旧值，并清除原有过期时间。
	if got, _ := c.cmd("GETSET", "sess", "v2"); got != "$2\r\nv1" {
		t.Fatalf("getset old value: %q", got)
	}
	if got, _ := c.cmd("TTL", "sess"); got != ":-1" {
		t.Fatalf("getset should clear ttl: %q", got)
	}
	// 键不存在时 GETSET 返回 null，但仍然成功写入。
	if got, _ := c.cmd("GETSET", "fresh", "new"); got != "$-1" {
		t.Fatalf("getset on missing key: %q", got)
	}
	if got, _ := c.cmd("GET", "fresh"); !strings.Contains(got, "new") {
		t.Fatalf("getset should still write: %q", got)
	}

	// APPEND 返回追加后的总长度。
	if got, _ := c.cmd("APPEND", "sess", "!"); got != ":3" {
		t.Fatalf("append length: %q", got)
	}
	if got, _ := c.cmd("GET", "sess"); !strings.Contains(got, "v2!") {
		t.Fatalf("append payload: %q", got)
	}
	// 追加不应丢失已有过期时间。
	if got, _ := c.cmd("SETEX", "sess2", "30", "a"); got != "+OK" {
		t.Fatalf("setex sess2: %q", got)
	}
	c.cmd("APPEND", "sess2", "b")
	if got, _ := c.cmd("TTL", "sess2"); got == ":-1" || got == ":-2" {
		t.Fatalf("append should keep ttl: %q", got)
	}

	// STRLEN / TYPE。
	if got, _ := c.cmd("STRLEN", "sess"); got != ":3" {
		t.Fatalf("strlen: %q", got)
	}
	if got, _ := c.cmd("STRLEN", "missing"); got != ":0" {
		t.Fatalf("strlen missing: %q", got)
	}
	if got, _ := c.cmd("TYPE", "sess"); got != "+string" {
		t.Fatalf("type: %q", got)
	}
	if got, _ := c.cmd("TYPE", "missing"); got != "+none" {
		t.Fatalf("type missing: %q", got)
	}

	// 追加同样受容量闸门约束，超额时报错且原值不变。
	ten, err := g.store.GetTenant("tenant-1")
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	ten.QuotaBytes = 10
	if err := g.store.UpsertTenant(ten); err != nil {
			t.Fatalf("update tenant: %v", err)
	}
	g.resetCachesForTest(ten.ID)
	if got, _ := c.cmd("APPEND", "sess", strings.Repeat("z", 30)); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("append beyond capacity should fail: %q", got)
	}
	if got, _ := c.cmd("STRLEN", "sess"); got != ":3" {
		t.Fatalf("failed append must keep old value: %q", got)
	}
}
