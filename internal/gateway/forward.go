package gateway

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/xiancuncloud/idlemem-server/internal/model"
)

// 转发超时：单个节点不可达时快速失败，避免拖垮网关连接。
const forwardTimeout = 3 * time.Second

// onlineShardNodes 返回某租户所有 active 分片承载的在线节点（去重）。
// 网关据此把数据请求转发到 Agent 节点，实现"分布式缓存"数据面。
func (g *Gateway) onlineShardNodes(tenantID string) ([]*model.Node, error) {
	shards, err := g.store.ListShards()
	if err != nil {
		return nil, err
	}
	nodes, err := g.store.ListNodes()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	seen := make(map[string]bool)
	var out []*model.Node
	for _, sh := range shards {
		if sh.TenantID != tenantID || sh.Status != model.ShardActive {
			continue
		}
		n, ok := byID[sh.NodeID]
		if !ok || n.Status != model.NodeOnline || n.KVAddr == "" {
			continue
		}
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		out = append(out, n)
	}
	return out, nil
}

// nodeDialAddr 计算控制面视角下节点 KV 服务的实际拨号地址：
// 主机用节点 IP（控制面记录的来源地址），端口用节点上报的 KVAddr。
func nodeDialAddr(n *model.Node) string {
	_, port, err := net.SplitHostPort(n.KVAddr)
	if err != nil || port == "" {
		port = "16380"
	}
	host := n.IP
	if host == "" || host == "::1" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// forwardClient 极简 RESP 客户端（标准库实现，零第三方依赖）。
// 仅支持网关转发所需的命令子集。
type forwardClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialResp(addr string) (*forwardClient, error) {
	conn, err := net.DialTimeout("tcp", addr, forwardTimeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(forwardTimeout))
	return &forwardClient{conn: conn, br: bufio.NewReader(conn)}, nil
}

func (c *forwardClient) close() { _ = c.conn.Close() }

// do 发送一条 RESP 数组命令并读取响应。
func (c *forwardClient) do(args ...string) ([]byte, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := c.conn.Write([]byte(sb.String())); err != nil {
		return nil, err
	}
	// 读取响应首字节判断类型。
	line, err := c.br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	switch line[0] {
	case '+': // 简单字符串
		return []byte(strings.TrimSuffix(line, "\r\n")[1:]), nil
	case '-': // 错误
		return nil, fmt.Errorf("%s", strings.TrimSpace(line[1:]))
	case ':': // 整数
		return []byte(line[1 : len(line)-2]), nil
	case '$': // 批量字符串
		n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil // null
		}
		body := make([]byte, n+2)
		if _, err := ioReadFull(c.br, body); err != nil {
			return nil, err
		}
		return body[:n], nil
	default:
		return nil, fmt.Errorf("unexpected response type %q", line[0])
	}
}

// ioReadFull 读取 bufio.Reader 满 len(p) 字节。
func ioReadFull(r *bufio.Reader, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := r.Read(p[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// ---------- 转发辅助 ----------

// forwardSet 把 SET 写入所有在线承载节点；任一成功即视为成功。
// 返回写入成功的节点数。
func (g *Gateway) forwardSet(nodes []*model.Node, key, value string, ttlSec int64) (int, error) {
	var okCnt int
	var lastErr error
	for _, n := range nodes {
		c, err := dialResp(nodeDialAddr(n))
		if err != nil {
			lastErr = err
			continue
		}
		var args []string
		if ttlSec > 0 {
			args = []string{"SET", key, value, "EX", strconv.FormatInt(ttlSec, 10)}
		} else {
			args = []string{"SET", key, value}
		}
		resp, err := c.do(args...)
		_ = resp
		c.close()
		if err != nil {
			lastErr = err
			continue
		}
		okCnt++
	}
	if okCnt == 0 && lastErr != nil {
		return 0, lastErr
	}
	return okCnt, nil
}

// forwardGet 从任一在线承载节点读取 key；全部未命中返回 nil,false。
func (g *Gateway) forwardGet(nodes []*model.Node, key string) ([]byte, bool) {
	for _, n := range nodes {
		c, err := dialResp(nodeDialAddr(n))
		if err != nil {
			continue
		}
		val, err := c.do("GET", key)
		c.close()
		if err != nil {
			continue
		}
		if val == nil {
			continue // 该节点未命中，尝试下一个
		}
		return val, true
	}
	return nil, false
}

// forwardDel 删除所有在线承载节点上的 key，返回删除成功的节点数。
func (g *Gateway) forwardDel(nodes []*model.Node, key string) (int, error) {
	var okCnt int
	var lastErr error
	for _, n := range nodes {
		c, err := dialResp(nodeDialAddr(n))
		if err != nil {
			lastErr = err
			continue
		}
		_, err = c.do("DEL", key)
		c.close()
		if err != nil {
			lastErr = err
			continue
		}
		okCnt++
	}
	if okCnt == 0 && lastErr != nil {
		return 0, lastErr
	}
	return okCnt, nil
}

// forwardExpire 在所有在线承载节点上设置 TTL。
func (g *Gateway) forwardExpire(nodes []*model.Node, key string, sec int64) (int, error) {
	var okCnt int
	var lastErr error
	for _, n := range nodes {
		c, err := dialResp(nodeDialAddr(n))
		if err != nil {
			lastErr = err
			continue
		}
		_, err = c.do("EXPIRE", key, strconv.FormatInt(sec, 10))
		c.close()
		if err != nil {
			lastErr = err
			continue
		}
		okCnt++
	}
	if okCnt == 0 && lastErr != nil {
		return 0, lastErr
	}
	return okCnt, nil
}

// forwardTTL 从任一在线承载节点读取 TTL；返回 (秒, 存在)。
func (g *Gateway) forwardTTL(nodes []*model.Node, key string) (int64, bool) {
	for _, n := range nodes {
		c, err := dialResp(nodeDialAddr(n))
		if err != nil {
			continue
		}
		val, err := c.do("TTL", key)
		c.close()
		if err != nil || val == nil {
			continue
		}
		sec, err := strconv.ParseInt(string(val), 10, 64)
		if err != nil {
			continue
		}
		return sec, true
	}
	return -2, false
}
