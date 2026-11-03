// Package gateway 实现企业客户接入的 Redis 协议网关。
//
// 客户使用标准 Redis 客户端（如 redis-cli、go-redis）连接网关，
// 通过 AUTH 租户 Token 鉴权；网关按租户隔离键空间，数据落于内存缓存
// （进程重启即清空，符合"可丢失缓存"定位）。后续接入 Agent 节点后，
// 网关可演进为"按 key 路由转发到分片节点"的代理模式。
package gateway

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/buntdb"
	"github.com/tidwall/redcon"

	"github.com/xiancuncloud/idlemem-server/internal/model"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

// MaxCommandsPerSec 单连接每秒命令数上限（简单限流）。
const MaxCommandsPerSec = 1000

// Gateway Redis 协议网关。
type Gateway struct {
	addr     string
	store    *store.Store // 控制面元数据（租户校验、分片查询）
	cache    *buntdb.DB   // 内存缓存数据
	srv      *redcon.Server
	logger   *log.Logger
}

// New 创建网关。cachePath 为空时使用纯内存缓存。
func New(addr string, s *store.Store, cachePath string, logger *log.Logger) (*Gateway, error) {
	if cachePath == "" {
		cachePath = ":memory:"
	}
	db, err := buntdb.Open(cachePath)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		addr:   addr,
		store:  s,
		cache:  db,
		logger: logger,
	}
	g.srv = redcon.NewServer(addr, g.handle, g.accept, g.closed)
	return g, nil
}

// ListenAndServe 启动网关并阻塞。
func (g *Gateway) ListenAndServe() error {
	g.logger.Printf("gateway: redis protocol listening on %s", g.addr)
	return g.srv.ListenAndServe()
}

// Close 关闭网关。
func (g *Gateway) Close() error {
	_ = g.cache.Close()
	return g.srv.Close()
}

// addr 返回监听地址。
func (g *Gateway) Addr() string { return g.addr }

// handle 处理单条命令。
func (g *Gateway) handle(conn redcon.Conn, cmd redcon.Command) {
	args := make([]string, len(cmd.Args))
	for i, a := range cmd.Args {
		args[i] = string(a)
	}
	if len(args) == 0 {
		conn.WriteError("ERR empty command")
		return
	}

	// 限流：简单固定窗口（每连接）。
	rl := connRateLimiter(conn)
	if !rl.Allow() {
		conn.WriteError("ERR rate limit exceeded")
		return
	}

	verb := strings.ToUpper(args[0])
	switch verb {
	case "PING":
		conn.WriteString("PONG")
	case "QUIT":
		conn.WriteString("OK")
		_ = conn.Close()
	case "AUTH":
		g.handleAuth(conn, args)
	case "SET":
		g.handleSet(conn, args)
	case "GET":
		g.handleGet(conn, args)
	case "DEL":
		g.handleDel(conn, args)
	case "EXPIRE":
		g.handleExpire(conn, args)
	case "TTL":
		g.handleTTL(conn, args)
	case "INFO":
		g.handleInfo(conn)
	default:
		conn.WriteError("ERR unsupported command: " + verb)
	}
}

// tenant 从连接上下文取租户；未鉴权返回 nil。
func tenant(conn redcon.Conn) *model.Tenant {
	if v := conn.Context(); v != nil {
		if t, ok := v.(*model.Tenant); ok {
			return t
		}
	}
	return nil
}

// handleAuth 校验租户 Token 并绑定到连接。
func (g *Gateway) handleAuth(conn redcon.Conn, args []string) {
	if len(args) < 2 {
		conn.WriteError("ERR wrong number of arguments for 'auth' command")
		return
	}
	token := args[1]
	tenants, err := g.store.ListTenants()
	if err != nil {
		conn.WriteError("ERR internal error")
		return
	}
	for _, t := range tenants {
		if t.Token != "" && t.Token == token {
			conn.SetContext(t)
			conn.WriteString("OK")
			return
		}
	}
	conn.WriteError("ERR invalid token")
}

// namespaceKey 把业务 key 映射为租户隔离的存储 key。
func namespaceKey(t *model.Tenant, key string) string {
	return t.ID + ":" + key
}

func (g *Gateway) handleSet(conn redcon.Conn, args []string) {
	t := tenant(conn)
	if t == nil {
		conn.WriteError("NOAUTH Authentication required")
		return
	}
	if len(args) < 3 {
		conn.WriteError("ERR wrong number of arguments for 'set' command")
		return
	}
	key, value := args[1], args[2]
	var ttl time.Duration
	// 可选 EX 秒。
	if len(args) >= 5 && strings.EqualFold(args[3], "EX") {
		sec, err := strconv.ParseInt(args[4], 10, 64)
		if err != nil || sec <= 0 {
			conn.WriteError("ERR invalid expire time in 'set' command")
			return
		}
		ttl = time.Duration(sec) * time.Second
	}
	nsKey := namespaceKey(t, key)

	var opts *buntdb.SetOptions
	if ttl > 0 {
		opts = &buntdb.SetOptions{Expires: true, TTL: ttl}
	}
	err := g.cache.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(nsKey, value, opts)
		return err
	})
	if err != nil {
		conn.WriteError("ERR " + err.Error())
		return
	}
	conn.WriteString("OK")
}

func (g *Gateway) handleGet(conn redcon.Conn, args []string) {
	t := tenant(conn)
	if t == nil {
		conn.WriteError("NOAUTH Authentication required")
		return
	}
	if len(args) < 2 {
		conn.WriteError("ERR wrong number of arguments for 'get' command")
		return
	}
	nsKey := namespaceKey(t, args[1])
	var val string
	found := false
	_ = g.cache.View(func(tx *buntdb.Tx) error {
		v, err := tx.Get(nsKey)
		if err == nil {
			val = v
			found = true
		}
		return nil
	})
	if !found {
		conn.WriteNull()
		return
	}
	conn.WriteBulkString(val)
}

func (g *Gateway) handleDel(conn redcon.Conn, args []string) {
	t := tenant(conn)
	if t == nil {
		conn.WriteError("NOAUTH Authentication required")
		return
	}
	if len(args) < 2 {
		conn.WriteError("ERR wrong number of arguments for 'del' command")
		return
	}
	count := 0
	_ = g.cache.Update(func(tx *buntdb.Tx) error {
		for _, k := range args[1:] {
			if _, err := tx.Delete(namespaceKey(t, k)); err == nil {
				count++
			}
		}
		return nil
	})
	conn.WriteInt(count)
}

func (g *Gateway) handleExpire(conn redcon.Conn, args []string) {
	t := tenant(conn)
	if t == nil {
		conn.WriteError("NOAUTH Authentication required")
		return
	}
	if len(args) < 3 {
		conn.WriteError("ERR wrong number of arguments for 'expire' command")
		return
	}
	sec, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		conn.WriteError("ERR value is not an integer or out of range")
		return
	}
	nsKey := namespaceKey(t, args[1])
	var opts *buntdb.SetOptions
	if sec > 0 {
		opts = &buntdb.SetOptions{Expires: true, TTL: time.Duration(sec) * time.Second}
	}
	ok := false
	_ = g.cache.Update(func(tx *buntdb.Tx) error {
		v, err := tx.Get(nsKey)
		if err != nil {
			return nil
		}
		if _, _, err := tx.Set(nsKey, v, opts); err == nil {
			ok = true
		}
		return nil
	})
	if ok {
		conn.WriteInt(1)
	} else {
		conn.WriteInt(0)
	}
}

func (g *Gateway) handleTTL(conn redcon.Conn, args []string) {
	t := tenant(conn)
	if t == nil {
		conn.WriteError("NOAUTH Authentication required")
		return
	}
	if len(args) < 2 {
		conn.WriteError("ERR wrong number of arguments for 'ttl' command")
		return
	}
	nsKey := namespaceKey(t, args[1])
	var ttlSeconds int64 = -2 // 不存在
	_ = g.cache.View(func(tx *buntdb.Tx) error {
		if _, err := tx.Get(nsKey); err != nil {
			return nil // 不存在，保持 -2
		}
		ttl, err := tx.TTL(nsKey)
		if err != nil {
			return nil
		}
		if ttl < 0 {
			ttlSeconds = -1 // 存在但不过期
			return nil
		}
		ttlSeconds = int64(ttl.Seconds())
		return nil
	})
	conn.WriteInt(int(ttlSeconds))
}

func (g *Gateway) handleInfo(conn redcon.Conn) {
	var entries, keys int
	_ = g.cache.View(func(tx *buntdb.Tx) error {
		keys, _ = tx.Len()
		return tx.Ascend("", func(k, v string) bool {
			entries++
			return true
		})
	})
	conn.WriteBulkString("idlemem-gateway\r\nkeys:" + strconv.Itoa(keys) + "\r\nentries:" + strconv.Itoa(entries) + "\r\n")
}

// accept 连接建立时初始化限流器。
func (g *Gateway) accept(conn redcon.Conn) bool {
	conn.SetContext(newLimiter(MaxCommandsPerSec))
	return true
}

// closed 连接关闭时清理。
func (g *Gateway) closed(conn redcon.Conn, err error) {
	// 无状态，无需清理。
}

// rateLimiter 简单固定窗口限流器。
type rateLimiter struct {
	windowStart time.Time
	count       int
	limit       int
}

func newLimiter(limit int) *rateLimiter {
	return &rateLimiter{windowStart: time.Now(), limit: limit}
}

func (rl *rateLimiter) Allow() bool {
	now := time.Now()
	if now.Sub(rl.windowStart) >= time.Second {
		rl.windowStart = now
		rl.count = 0
	}
	rl.count++
	return rl.count <= rl.limit
}

// connRateLimiter 从连接上下文取限流器。
func connRateLimiter(conn redcon.Conn) *rateLimiter {
	if v := conn.Context(); v != nil {
		if rl, ok := v.(*rateLimiter); ok {
			return rl
		}
	}
	return newLimiter(MaxCommandsPerSec)
}

// ErrNoTenant 未鉴权。
var ErrNoTenant = errors.New("gateway: tenant not authenticated")
