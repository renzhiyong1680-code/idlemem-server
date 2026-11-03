package gateway

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/buntdb"

	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

// respClient 最小 RESP 协议客户端（仅测试用，避免引入 redis 客户端依赖）。
type respClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func newRESPClient(t *testing.T, addr string) *respClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &respClient{conn: conn, br: bufio.NewReader(conn)}
}

func (c *respClient) cmd(args ...string) (string, error) {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		return "", err
	}
	// 读首行：+OK / -ERR / :N / $len
	line, err := c.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return "", fmt.Errorf("empty reply")
	}
	switch line[0] {
	case '+', '-', ':':
		return line, nil
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return line, err
		}
		if n < 0 {
			return "$-1", nil // null bulk
		}
		body := make([]byte, n+2)
		if _, err := io.ReadFull(c.br, body); err != nil {
			return line, err
		}
		return "$" + strconv.Itoa(n) + "\r\n" + string(body[:n]), nil
	default:
		return line, fmt.Errorf("unexpected reply: %q", line)
	}
}

// startGateway 启动网关并返回连接地址。
func startGateway(t *testing.T, g *Gateway) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = g.srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ten := &model.Tenant{
		ID:        "tenant-1",
		Name:      "test",
		Token:     "tok-1",
		CreatedAt: time.Now(),
	}
	if err := st.UpsertTenant(ten); err != nil {
		t.Fatalf("upsert tenant: %v", err)
	}

	g, err := New("127.0.0.1:0", st, ":memory:", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestGateway_SetGet(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)
	c := newRESPClient(t, addr)

	got, err := c.cmd("AUTH", "tok-1")
	if err != nil || got != "+OK" {
		t.Fatalf("auth: %q, %v", got, err)
	}
	got, err = c.cmd("SET", "k", "v")
	if err != nil || got != "+OK" {
		t.Fatalf("set: %q, %v", got, err)
	}
	got, err = c.cmd("GET", "k")
	if err != nil || !strings.HasPrefix(got, "$1\r\nv") {
		t.Fatalf("get: %q, %v", got, err)
	}
}

func TestGateway_AuthRequired(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)
	c := newRESPClient(t, addr)

	got, err := c.cmd("SET", "k", "v")
	if err != nil || !strings.HasPrefix(got, "-NOAUTH") {
		t.Fatalf("set without auth: %q, %v", got, err)
	}
}

func TestGateway_InvalidToken(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)
	c := newRESPClient(t, addr)

	got, err := c.cmd("AUTH", "wrong-token")
	if err != nil || !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("auth with bad token: %q, %v", got, err)
	}
}

func TestGateway_TenantIsolation(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)

	// 租户1写入。
	c1 := newRESPClient(t, addr)
	if got, err := c1.cmd("AUTH", "tok-1"); err != nil || got != "+OK" {
		t.Fatalf("auth t1: %q, %v", got, err)
	}
	if got, err := c1.cmd("SET", "secret", "42"); err != nil || got != "+OK" {
		t.Fatalf("set t1: %q, %v", got, err)
	}

	// 新增租户2。
	st := g.store
	ten2 := &model.Tenant{ID: "tenant-2", Name: "two", Token: "tok-2", CreatedAt: time.Now()}
	if err := st.UpsertTenant(ten2); err != nil {
		t.Fatalf("upsert t2: %v", err)
	}

	// 租户2 GET 不到租户1的 key。
	c2 := newRESPClient(t, addr)
	if got, err := c2.cmd("AUTH", "tok-2"); err != nil || got != "+OK" {
		t.Fatalf("auth t2: %q, %v", got, err)
	}
	if got, err := c2.cmd("GET", "secret"); err != nil || got != "$-1" {
		t.Fatalf("get cross-tenant: %q, %v", got, err)
	}

	// 底层缓存键空间验证。
	var t1Found, t2Found bool
	_ = g.cache.View(func(tx *buntdb.Tx) error {
		return tx.Ascend("", func(k, v string) bool {
			if k == "tenant-1:secret" {
				t1Found = true
			}
			if k == "tenant-2:secret" {
				t2Found = true
			}
			return true
		})
	})
	if !t1Found {
		t.Fatal("tenant-1 key missing")
	}
	if t2Found {
		t.Fatal("tenant-2 key leaked into cache")
	}
}

func TestGateway_TTL(t *testing.T) {
	g := newTestGateway(t)
	addr := startGateway(t, g)
	c := newRESPClient(t, addr)
	_, _ = c.cmd("AUTH", "tok-1")

	if got, err := c.cmd("SET", "tmp", "x", "EX", "2"); err != nil || got != "+OK" {
		t.Fatalf("set with ttl: %q, %v", got, err)
	}
	if got, err := c.cmd("TTL", "tmp"); err != nil || (got != ":1" && got != ":2") {
		t.Fatalf("ttl: %q, %v", got, err)
	}
	if got, err := c.cmd("DEL", "tmp"); err != nil || got != ":1" {
		t.Fatalf("del: %q, %v", got, err)
	}
	if got, err := c.cmd("TTL", "tmp"); err != nil || got != ":-2" {
		t.Fatalf("ttl after del: %q, %v", got, err)
	}
}
