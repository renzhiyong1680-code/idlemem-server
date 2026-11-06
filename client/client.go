// Package client 是企业接入闲存云的控制面网关 Go SDK。
//
// 用法：
//
//	c, err := client.Dial("127.0.0.1:6379", token)
//	if err != nil { ... }
//	defer c.Close()
//
//	err = c.Set(ctx, "user:1", []byte(`{"name":"alice"}`), time.Hour)
//	v, err := c.Get(ctx, "user:1")
//
// 设计取舍：
//   - 单连接串行化 + 互斥锁，简单可靠；高并发场景请开多个 Client（或用 Pool）。
//   - 网络错误时重建连接并重新 AUTH，再重放一次命令；所有支持的命令都是幂等的，
//     因此重放不会产生副作用。
//   - 鉴权失败（token 错误、租户停服）立即返回错误，不做重试，避免掩盖问题。
package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound 键不存在（或已过期）。
var ErrNotFound = errors.New("client: key not found")

// Option 配置客户端。
type Option func(*Client)

// WithDialTimeout 设置建连超时，默认 3s。
func WithDialTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.dialTimeout = d
		}
	}
}

// WithTimeout 设置单条命令的超时，默认 5s。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// Client 网关客户端，并发安全。
type Client struct {
	addr        string
	token       string
	dialTimeout time.Duration
	timeout     time.Duration

	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
}

// Dial 建立连接并完成 AUTH 鉴权。
func Dial(addr, token string, opts ...Option) (*Client, error) {
	c := &Client{
		addr:        addr,
		token:       token,
		dialTimeout: 3 * time.Second,
		timeout:     5 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	if err := c.connect(context.Background()); err != nil {
		return nil, err
	}
	return c, nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.br = nil, nil
	return err
}

// connect 建连并鉴权；调用方需持有 c.mu。
func (c *Client) connect(ctx context.Context) error {
	d := net.Dialer{Timeout: c.dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("client: dial %s: %w", c.addr, err)
	}
	c.conn, c.br = conn, bufio.NewReader(conn)

	if c.token == "" {
		return nil
	}
	resp, err := c.roundTrip("AUTH", c.token)
	if err != nil {
		_ = conn.Close()
		c.conn, c.br = nil, nil
		return fmt.Errorf("client: auth: %w", err)
	}
	if s, ok := resp.(string); !ok || !strings.EqualFold(s, "OK") {
		_ = conn.Close()
		c.conn, c.br = nil, nil
		return fmt.Errorf("client: auth rejected: %v", resp)
	}
	return nil
}

// do 执行一条命令，网络错误时重连并重放一次。
// 鉴权/业务错误直接返回，不重试。
func (c *Client) do(ctx context.Context, args ...string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		if err := c.connect(ctx); err != nil {
			return nil, err
		}
	}
	resp, err := c.roundTrip(args...)
	if err == nil {
		return resp, nil
	}
	var reerr *replyError
	if errors.As(err, &reerr) {
		// 服务端明确返回的错误：不重试。
		return nil, err
	}

	// 传输层失败：丢弃旧连接，重建并重放一次。
	_ = c.conn.Close()
	c.conn, c.br = nil, nil
	if cerr := c.connect(ctx); cerr != nil {
		return nil, fmt.Errorf("client: reconnect after %v: %w", err, cerr)
	}
	return c.roundTrip(args...)
}

// roundTrip 写一条命令并读取回复。
func (c *Client) roundTrip(args ...string) (any, error) {
	if c.conn == nil {
		return nil, errors.New("client: not connected")
	}
	if c.timeout > 0 {
		_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return nil, err
	}
	return readReply(c.br)
}

// replyError 服务端返回的 -ERR 回复。
type replyError struct{ msg string }

func (e *replyError) Error() string { return e.msg }

// readReply 解析一条 RESP 回复。
func readReply(br *bufio.Reader) (any, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("client: empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, &replyError{msg: line[1:]}
	case ':':
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("client: bad integer reply %q", line)
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("client: bad bulk length %q", line)
		}
		if n < 0 {
			return nil, nil // nil bulk
		}
		buf := make([]byte, n+2) // 含结尾 \r\n
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("client: bad array length %q", line)
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			v, err := readReply(br)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("client: unexpected reply %q", line)
	}
}

// Ping 健康检查。
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, "PING")
	if err != nil {
		return err
	}
	if s, ok := resp.(string); !ok || !strings.EqualFold(s, "PONG") {
		return fmt.Errorf("client: unexpected ping reply %v", resp)
	}
	return nil
}

// Set 写入键值；ttl<=0 表示不设置过期时间。
func (c *Client) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	args := []string{"SET", key, string(value)}
	if ttl > 0 {
		sec := int64(ttl / time.Second)
		if sec <= 0 {
			sec = 1 // 亚秒级 TTL 向上取整到 1s
		}
		args = append(args, "EX", strconv.FormatInt(sec, 10))
	}
	resp, err := c.do(ctx, args...)
	if err != nil {
		return err
	}
	if s, ok := resp.(string); !ok || !strings.EqualFold(s, "OK") {
		return fmt.Errorf("client: unexpected set reply %v", resp)
	}
	return nil
}

// Get 读取键值；不存在返回 ErrNotFound。
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.do(ctx, "GET", key)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, ErrNotFound
	}
	s, ok := resp.(string)
	if !ok {
		return nil, fmt.Errorf("client: unexpected get reply %v", resp)
	}
	return []byte(s), nil
}

// Del 删除一个或多个键，返回实际删除数量。
func (c *Client) Del(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	resp, err := c.do(ctx, append([]string{"DEL"}, keys...)...)
	if err != nil {
		return 0, err
	}
	n, ok := resp.(int64)
	if !ok {
		return 0, fmt.Errorf("client: unexpected del reply %v", resp)
	}
	return n, nil
}

// Expire 设置过期时间，返回是否生效（键不存在返回 false）。
func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	sec := int64(ttl / time.Second)
	if sec <= 0 {
		sec = 1
	}
	resp, err := c.do(ctx, "EXPIRE", key, strconv.FormatInt(sec, 10))
	if err != nil {
		return false, err
	}
	n, ok := resp.(int64)
	if !ok {
		return false, fmt.Errorf("client: unexpected expire reply %v", resp)
	}
	return n == 1, nil
}

// TTL 返回剩余存活时间；键不存在返回 ErrNotFound，永不过期返回 -1。
func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	resp, err := c.do(ctx, "TTL", key)
	if err != nil {
		return 0, err
	}
	n, ok := resp.(int64)
	if !ok {
		return 0, fmt.Errorf("client: unexpected ttl reply %v", resp)
	}
	if n < 0 {
		return -1, nil
	}
	return time.Duration(n) * time.Second, nil
}

// Pool 是一组 Client 的简易负载分发器，适合并发写入场景。
type Pool struct {
	clients []*Client
	next    uint64
	mu      sync.Mutex
}

// NewPool 创建 size 个连接（size<=0 时取 4）。
func NewPool(addr, token string, size int, opts ...Option) (*Pool, error) {
	if size <= 0 {
		size = 4
	}
	p := &Pool{clients: make([]*Client, 0, size)}
	for i := 0; i < size; i++ {
		c, err := Dial(addr, token, opts...)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.clients = append(p.clients, c)
	}
	return p, nil
}

// Get 取一条连接（轮询）。
func (p *Pool) Get() *Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.clients[p.next%uint64(len(p.clients))]
	p.next++
	return c
}

// Close 关闭所有连接。
func (p *Pool) Close() error {
	var first error
	for _, c := range p.clients {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
