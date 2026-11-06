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

	"github.com/renzhiyong1680-code/idlemem-server/internal/model"
	"github.com/renzhiyong1680-code/idlemem-server/internal/store"
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
	return c.readReply()
}

// readReply 解析一条 RESP 回复，支持 simple string / error / integer /
// bulk string / null / 数组（数组元素以 \r\n 连接）。
func (c *respClient) readReply() (string, error) {
	// 读首行：+OK / -ERR / :N / $len / *n
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
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return line, err
		}
		if n < 0 {
			return "*-1", nil
		}
		items := make([]string, 0, n)
		for i := 0; i < n; i++ {
			it, err := c.readReply()
			if err != nil {
				return line, err
			}
			items = append(items, it)
		}
		return "*" + strconv.Itoa(n) + "\r\n" + strings.Join(items, "\r\n"), nil
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
// resetCachesForTest 清掉容量与租户快照缓存，让测试里改的配额立即生效。
func (g *Gateway) resetCachesForTest(id string) {
	g.quota.mu.Lock()
	delete(g.quota.capAt, id)
	g.quota.mu.Unlock()
	g.snaps.mu.Lock()
	delete(g.snaps.byID, id)
	g.snaps.mu.Unlock()
}

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

// fakeNode 极简内存 KV 节点，模拟 Agent 的 RESP 端口。
type fakeNode struct {
	ln   net.Listener
	data map[string]string
	ttl  map[string]int64 // key -> ttl 秒
}

func startFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake node listen: %v", err)
	}
	f := &fakeNode{ln: ln, data: map[string]string{}, ttl: map[string]int64{}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeNode) serve(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "*") {
			return
		}
		n, _ := strconv.Atoi(line[1:])
		args := make([]string, 0, n)
		for i := 0; i < n; i++ {
			hdr, _ := br.ReadString('\n')
			blen, _ := strconv.Atoi(strings.TrimRight(hdr, "\r\n")[1:])
			body := make([]byte, blen+2)
			if _, err := io.ReadFull(br, body); err != nil {
				return
			}
			args = append(args, string(body[:blen]))
		}
		if len(args) == 0 {
			return
		}
		verb := strings.ToUpper(args[0])
		switch verb {
		case "SET":
			f.data[args[1]] = args[2]
			if len(args) >= 5 && strings.ToUpper(args[3]) == "EX" {
				sec, _ := strconv.ParseInt(args[4], 10, 64)
				f.ttl[args[1]] = sec
			}
			conn.Write([]byte("+OK\r\n"))
		case "GET":
			v, ok := f.data[args[1]]
			if !ok {
				conn.Write([]byte("$-1\r\n"))
			} else {
				conn.Write([]byte(fmt.Sprintf("$%d\r\n%s\r\n", len(v), v)))
			}
		case "DEL":
			if _, ok := f.data[args[1]]; ok {
				delete(f.data, args[1])
				delete(f.ttl, args[1])
				conn.Write([]byte(":1\r\n"))
			} else {
				conn.Write([]byte(":0\r\n"))
			}
		case "EXPIRE":
			if _, ok := f.data[args[1]]; ok {
				sec, _ := strconv.ParseInt(args[2], 10, 64)
				f.ttl[args[1]] = sec
				conn.Write([]byte(":1\r\n"))
			} else {
				conn.Write([]byte(":0\r\n"))
			}
		case "TTL":
			sec, ok := f.ttl[args[1]]
			if !ok {
				if _, has := f.data[args[1]]; has {
					conn.Write([]byte(":-1\r\n"))
				} else {
					conn.Write([]byte(":-2\r\n"))
				}
			} else {
				conn.Write([]byte(fmt.Sprintf(":%d\r\n", sec)))
			}
		default:
			conn.Write([]byte("-ERR unsupported\r\n"))
		}
	}
}

// newTestGatewayWithShard 建网关 + 租户 + 订单 + 分片（分片指向 fakeNode 所在节点）。
func newTestGatewayWithShard(t *testing.T) (*Gateway, *fakeNode) {
	t.Helper()
	g := newTestGateway(t)
	fn := startFakeNode(t)

	// 节点：KVAddr 指向 fakeNode 端口。
	n := &model.Node{
		ID:      "node-1",
		IP:      "127.0.0.1",
		Status:  model.NodeOnline,
		KVAddr:  fn.ln.Addr().String(),
		Version: "0.1.0",
	}
	if err := g.store.UpsertNode(n); err != nil {
		t.Fatalf("upsert node: %v", err)
	}
	o := &model.Order{
		ID:            "order-1",
		TenantID:      "tenant-1",
		CapacityBytes: 1 << 30,
		Replicas:      1,
		Status:        model.OrderActive,
	}
	if err := g.store.UpsertOrder(o); err != nil {
		t.Fatalf("upsert order: %v", err)
	}
	sh := &model.Shard{
		ID:        "shard-1",
		OrderID:   "order-1",
		TenantID:  "tenant-1",
		NodeID:    "node-1",
		KeyPrefix: "tenant-1:order-1:",
		Bytes:     1 << 30,
		Status:    model.ShardActive,
	}
	if err := g.store.UpsertShard(sh); err != nil {
		t.Fatalf("upsert shard: %v", err)
	}
	return g, fn
}

func TestGateway_ForwardSetGetDel(t *testing.T) {
	g, fn := newTestGatewayWithShard(t)
	addr := startGateway(t, g)
	c := newRESPClient(t, addr)
	_, _ = c.cmd("AUTH", "tok-1")

	// SET：应同时落到本地缓存与 fakeNode（多副本转发）。
	if got, err := c.cmd("SET", "k", "v"); err != nil || got != "+OK" {
		t.Fatalf("set: %q, %v", got, err)
	}
	if v, ok := fn.data["tenant-1:k"]; !ok || v != "v" {
		t.Fatalf("node data = %q,%v; want v,true", v, ok)
	}

	// GET：本地优先，应返回 v。
	if got, err := c.cmd("GET", "k"); err != nil || !strings.HasPrefix(got, "$1\r\nv") {
		t.Fatalf("get: %q, %v", got, err)
	}

	// DEL：转发删除节点数据。
	if got, err := c.cmd("DEL", "k"); err != nil || got != ":1" {
		t.Fatalf("del: %q, %v", got, err)
	}
	if _, ok := fn.data["tenant-1:k"]; ok {
		t.Fatal("node data should be deleted after forward del")
	}
	if got, err := c.cmd("GET", "k"); err != nil || got != "$-1" {
		t.Fatalf("get after del: %q, %v", got, err)
	}
}

func TestGateway_GetMissForwardsToNode(t *testing.T) {
	g, fn := newTestGatewayWithShard(t)
	addr := startGateway(t, g)
	c := newRESPClient(t, addr)
	_, _ = c.cmd("AUTH", "tok-1")

	// 节点上有数据、本地缓存没有（模拟缓存未命中需回源）。
	fn.data["tenant-1:src"] = "from-node"
	if got, err := c.cmd("GET", "src"); err != nil || !strings.HasPrefix(got, "$9\r\nfrom-node") {
		t.Fatalf("get miss -> node: %q, %v", got, err)
	}
}

func TestGateway_ForwardTTL(t *testing.T) {
	g, fn := newTestGatewayWithShard(t)
	addr := startGateway(t, g)
	c := newRESPClient(t, addr)
	_, _ = c.cmd("AUTH", "tok-1")

	// SET EX 60，转发后节点 TTL 应为 60；本地也有记录（先本地）。
	if got, err := c.cmd("SET", "tmp", "x", "EX", "60"); err != nil || got != "+OK" {
		t.Fatalf("set ex: %q, %v", got, err)
	}
	if got, err := c.cmd("TTL", "tmp"); err != nil || (got != ":59" && got != ":60") {
		t.Fatalf("ttl: %q, %v", got, err)
	}
	// 验证节点侧也有 TTL。
	if sec, ok := fn.ttl["tenant-1:tmp"]; !ok || sec != 60 {
		t.Fatalf("node ttl = %d,%v; want 60,true", sec, ok)
	}
}
