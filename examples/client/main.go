// Command client 是企业客户接入闲存云网关的示例客户端。
//
// 网关（gateway）对外暴露标准 Redis 协议（端口默认 :6379），因此任何
// Redis 客户端（redis-cli、各语言 SDK）都可以直接接入；本示例用纯标准库
// 手写最小 RESP 客户端，展示接入所需的最少步骤：
//
//  1. AUTH <tenant-token>          认证（连接级状态，后续命令复用连接）
//  2. SET <tenant>:<key> <value>   写入（落分片节点，多副本）
//  3. GET <tenant>:<key>           读取（本地命中或转发回源节点）
//  4. TTL / DEL                    过期与删除（同步节点）
//
// 用法：
//
//	go run ./examples/client -addr 127.0.0.1:6379 -token <tenant-token>
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
)

func main() {
	var (
		addr  = flag.String("addr", "127.0.0.1:6379", "gateway address")
		token = flag.String("token", "", "tenant token (from POST /api/v1/tenants)")
		key   = flag.String("key", "greeting", "cache key (auto prefixed with tenant id)")
		value = flag.String("value", "hello from xiancun cloud", "cache value")
	)
	flag.Parse()
	if *token == "" {
		log.Fatalf("usage: client -token <tenant-token> [-addr 127.0.0.1:6379]")
	}

	conn, err := net.Dial("tcp", *addr)
	if err != nil {
		log.Fatalf("dial %s: %v", *addr, err)
	}
	defer conn.Close()
	c := &client{conn: conn, r: bufio.NewReader(conn)}

	// 1. 认证。
	authLine, authErr := c.send("AUTH", *token)
	if err := c.expect(authLine, authErr, "OK"); err != nil {
		log.Fatalf("auth: %v", err)
	}
	log.Printf("auth: ok (token=%s)", maskToken(*token))

	// 2. 写入（TTL 60s）。
	full := *key
	setLine, setErr := c.send("SET", full, *value, "EX", "60")
	if err := c.expect(setLine, setErr, "OK"); err != nil {
		log.Fatalf("set: %v", err)
	}
	log.Printf("set: %s = %q (ttl 60s)", full, *value)

	// 3. 读取。
	got, err := c.bulk(c.send("GET", full))
	if err != nil {
		log.Fatalf("get: %v", err)
	}
	if got != *value {
		log.Fatalf("get: mismatch: got %q, want %q", got, *value)
	}
	log.Printf("get: %s = %q", full, got)

	// 4. TTL 与删除。
	ttl, err := c.send("TTL", full)
	if err != nil {
		log.Fatalf("ttl: %v", err)
	}
	log.Printf("ttl: %s = %s seconds", full, ttl)

	n, err := c.send("DEL", full)
	if err != nil {
		log.Fatalf("del: %v", err)
	}
	log.Printf("del: %s -> %s key(s) removed", full, n)
	log.Printf("client demo done.")
}

// client 最小 RESP 客户端（单连接）。
type client struct {
	conn net.Conn
	r    *bufio.Reader
}

// send 发送命令并返回一行简单响应（已剥类型前缀）。
func (c *client) send(args ...string) (string, error) {
	parts := []string{fmt.Sprintf("*%d\r\n", len(args))}
	for _, a := range args {
		parts = append(parts, fmt.Sprintf("$%d\r\n%s\r\n", len(a), a))
	}
	if _, err := c.conn.Write([]byte(strings.Join(parts, ""))); err != nil {
		return "", err
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) > 1 && (line[0] == '+' || line[0] == '-' || line[0] == ':' || line[0] == '$') {
		line = line[1:]
	}
	return line, nil
}

// bulk 读取 bulk 响应体（$-1/空 返回空）。
func (c *client) bulk(line string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if line == "$-1" || line == "-1" || line == "" {
		return "", nil
	}
	var blen int
	if _, err := fmt.Sscanf(line, "%d", &blen); err != nil || blen < 0 {
		return "", fmt.Errorf("bad bulk header %q", line)
	}
	body := make([]byte, blen+2)
	if _, err := readFull(c.r, body); err != nil {
		return "", err
	}
	return string(body[:blen]), nil
}

// expect 校验响应与预期一致（不一致视为错误响应）。
func (c *client) expect(line string, err error, want string) error {
	if err != nil {
		return err
	}
	if line != want {
		return fmt.Errorf("unexpected reply %q (want %q)", line, want)
	}
	return nil
}

func readFull(r *bufio.Reader, dst []byte) (int, error) {
	total := 0
	for total < len(dst) {
		n, err := r.Read(dst[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func maskToken(t string) string {
	if len(t) <= 4 {
		return "****"
	}
	return t[:2] + strings.Repeat("*", len(t)-4) + t[len(t)-2:]
}
