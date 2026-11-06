// Command xiancun-cli 是闲存云的命令行工具，覆盖两类使用者：
//
//	客户/运维：租户与订单管理、账单查询、对账导出（走控制面 REST API）
//	应用开发者：直接对网关读写缓存（走官方 client SDK）
//
// 用法：
//
//	xiancun-cli -server http://127.0.0.1:8080 -admin-token <token> tenant create -name acme -quota 10G
//	xiancun-cli -server http://127.0.0.1:8080 -admin-token <token> order create -tenant <id> -capacity 2G -expire 720h
//	xiancun-cli -server http://127.0.0.1:8080 -admin-token <token> billing summary
//	xiancun-cli kv -addr 127.0.0.1:6379 -token <tenant-token> set user:1 '{"name":"alice"}' -ttl 60
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xiancuncloud/idlemem-server/client"
)

const usageText = `闲存云 CLI

全局参数：
  -server        控制面地址（默认 http://127.0.0.1:8080）
  -admin-token   管理面令牌（控制面开启鉴权时必填，也可用 XIANCUN_ADMIN_TOKEN）
  -timeout       请求超时（默认 15s）

命令：
  tenant create -name <名字> [-quota 10G]   创建租户（返回 id 与 token）
  tenant list                               租户列表
  tenant suspend <id> | tenant resume <id>  停服 / 恢复
  tenant billing <id>                       单租户账单（累计/已出账/待结算）

  order create -tenant <id> -capacity 2G [-replicas 1] [-expire 720h]
                                            下单购买容量（-expire 省略表示长期）
  order list                                订单与状态

  node list                                 节点列表与容量
  node earnings <id>                        单节点承载用量与收益

  billing summary                           平台计费总览（客户账单/节点结算/佣金）
  billing settle                            出账一期（增量水位结算）
  billing invoices                          历史期账单
  billing export -o <文件.csv>              对账明细导出（Excel 可直接打开）

  kv -addr <host:port> -token <租户token> set <key> <value> [-ttl 60]
  kv -addr <host:port> -token <租户token> get <key>
  kv -addr <host:port> -token <租户token> del <key>

容量写法：1024 进制，支持 K/M/G/T，如 2G、512M，也可直接写字节数。
租期写法：Go 时长（720h、30d）或纯小时数。
`

// version 由构建脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	var (
		server = flag.String("server", "http://127.0.0.1:8080", "control plane base url")
		token  = flag.String("admin-token", os.Getenv("XIANCUN_ADMIN_TOKEN"), "admin token")
		tmo    = flag.Duration("timeout", 15*time.Second, "request timeout")
		help   = flag.Bool("h", false, "show help")
		ver    = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	flag.Parse()

	if *ver {
		fmt.Printf("xiancun-cli %s\n", version)
		return
	}

	if *help || flag.NArg() == 0 {
		fmt.Fprint(os.Stdout, usageText)
		return
	}

	c := &apiClient{
		base:  strings.TrimRight(*server, "/"),
		token: *token,
		hc:    &http.Client{Timeout: *tmo},
	}

	args := flag.Args()
	var err error
	switch args[0] {
	case "tenant":
		err = cmdTenant(c, args[1:])
	case "order":
		err = cmdOrder(c, args[1:])
	case "node":
		err = cmdNode(c, args[1:])
	case "billing":
		err = cmdBilling(c, args[1:])
	case "kv":
		err = cmdKV(args[1:])
	default:
		err = fmt.Errorf("unknown command %q (run with no args to see usage)", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---------- 控制面客户端 ----------

type apiClient struct {
	base  string
	token string
	hc    *http.Client
}

func (c *apiClient) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Admin-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%d %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func printJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		fmt.Println(string(data))
		return nil
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// ---------- tenant ----------

func cmdTenant(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("tenant: need subcommand (create|list|suspend|resume|billing)")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("tenant create", flag.ExitOnError)
		name := fs.String("name", "", "tenant name (required)")
		quota := fs.String("quota", "", "capacity quota, e.g. 10G (empty = unlimited)")
		_ = fs.Parse(args[1:])
		if *name == "" {
			return errors.New("tenant create: -name is required")
		}
		body := map[string]any{"name": *name}
		if *quota != "" {
			n, err := parseBytes(*quota)
			if err != nil {
				return err
			}
			body["quota_bytes"] = n
		}
		data, err := c.do(http.MethodPost, "/api/v1/tenants", body)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "list":
		data, err := c.do(http.MethodGet, "/api/v1/tenants", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "suspend", "resume":
		if len(args) < 2 {
			return fmt.Errorf("tenant %s: need tenant id", args[0])
		}
		data, err := c.do(http.MethodPost, "/api/v1/tenants/"+args[1]+"/"+args[0], nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "billing":
		if len(args) < 2 {
			return errors.New("tenant billing: need tenant id")
		}
		data, err := c.do(http.MethodGet, "/api/v1/tenants/"+args[1]+"/billing", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	default:
		return fmt.Errorf("tenant: unknown subcommand %q", args[0])
	}
}

// ---------- order ----------

func cmdOrder(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("order: need subcommand (create|list)")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("order create", flag.ExitOnError)
		tenant := fs.String("tenant", "", "tenant id (required)")
		capacity := fs.String("capacity", "", "ordered capacity, e.g. 2G (required)")
		replicas := fs.Int("replicas", 0, "replica count (0 = platform default)")
		expire := fs.String("expire", "", "lease duration, e.g. 720h (empty = long-lived)")
		_ = fs.Parse(args[1:])

		if *tenant == "" || *capacity == "" {
			return errors.New("order create: -tenant and -capacity are required")
		}
		capa, err := parseBytes(*capacity)
		if err != nil {
			return err
		}
		body := map[string]any{"tenant_id": *tenant, "capacity_bytes": capa}
		if *replicas > 0 {
			body["replicas"] = *replicas
		}
		if *expire != "" {
			hours, err := parseHours(*expire)
			if err != nil {
				return err
			}
			body["expire_hours"] = hours
		}
		data, err := c.do(http.MethodPost, "/api/v1/orders", body)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "list":
		data, err := c.do(http.MethodGet, "/api/v1/orders", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	default:
		return fmt.Errorf("order: unknown subcommand %q", args[0])
	}
}

// ---------- node ----------

func cmdNode(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("node: need subcommand (list|earnings)")
	}
	switch args[0] {
	case "list":
		data, err := c.do(http.MethodGet, "/api/v1/nodes", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "earnings":
		if len(args) < 2 {
			return errors.New("node earnings: need node id")
		}
		data, err := c.do(http.MethodGet, "/api/v1/nodes/"+args[1]+"/earnings", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	default:
		return fmt.Errorf("node: unknown subcommand %q", args[0])
	}
}

// ---------- billing ----------

func cmdBilling(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("billing: need subcommand (summary|settle|invoices|export)")
	}
	switch args[0] {
	case "summary":
		data, err := c.do(http.MethodGet, "/api/v1/billing", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "settle":
		data, err := c.do(http.MethodPost, "/api/v1/billing/settle", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "invoices":
		data, err := c.do(http.MethodGet, "/api/v1/billing/invoices", nil)
		if err != nil {
			return err
		}
		return printJSON(data)

	case "export":
		fs := flag.NewFlagSet("billing export", flag.ExitOnError)
		out := fs.String("o", "", "output csv path (default stdout)")
		_ = fs.Parse(args[1:])
		data, err := c.do(http.MethodGet, "/api/v1/billing/export.csv", nil)
		if err != nil {
			return err
		}
		if *out == "" {
			_, _ = os.Stdout.Write(data)
			return nil
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("csv written: %s (%d bytes)\n", *out, len(data))
		return nil

	default:
		return fmt.Errorf("billing: unknown subcommand %q", args[0])
	}
}

// ---------- kv ----------

func cmdKV(args []string) error {
	fs := flag.NewFlagSet("kv", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:6379", "gateway address host:port")
	tenantToken := fs.String("token", "", "tenant token (required)")
	ttl := fs.Duration("ttl", 0, "key ttl, e.g. 60s (0 = no expiry)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	// Go 的 flag 遇到第一个位置参数就停止解析，因此允许 -ttl 写在 `set <key> <value>`
	// 之后：这里手动把它从位置参数中摘出来。
	for i := 0; i < len(rest); i++ {
		if rest[i] == "-ttl" || rest[i] == "--ttl" {
			if i+1 >= len(rest) {
				return errors.New("kv: -ttl needs a value (e.g. -ttl 120s)")
			}
			v, err := time.ParseDuration(rest[i+1])
			if err != nil {
				return fmt.Errorf("kv: invalid -ttl %q", rest[i+1])
			}
			*ttl = v
			rest = append(rest[:i], rest[i+2:]...)
			break
		}
		if strings.HasPrefix(rest[i], "-ttl=") || strings.HasPrefix(rest[i], "--ttl=") {
			v, err := time.ParseDuration(strings.SplitN(rest[i], "=", 2)[1])
			if err != nil {
				return fmt.Errorf("kv: invalid -ttl %q", rest[i])
			}
			*ttl = v
			rest = append(rest[:i], rest[i+1:]...)
			break
		}
	}
	if len(rest) == 0 || *tenantToken == "" {
		return errors.New("kv: need -token and a subcommand (set|get|del)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := client.Dial(*addr, *tenantToken, client.WithTimeout(5*time.Second))
	if err != nil {
		return err
	}
	defer c.Close()

	switch rest[0] {
	case "set":
		if len(rest) < 3 {
			return errors.New("kv set: need <key> <value> (use - to read value from stdin)")
		}
		value := rest[2]
		if value == "-" {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			value = string(raw)
		}
		if err := c.Set(ctx, rest[1], []byte(value), *ttl); err != nil {
			return err
		}
		fmt.Printf("OK %s (%d bytes, ttl %v)\n", rest[1], len(value), *ttl)

	case "get":
		if len(rest) < 2 {
			return errors.New("kv get: need <key>")
		}
		v, err := c.Get(ctx, rest[1])
		if errors.Is(err, client.ErrNotFound) {
			return fmt.Errorf("key not found: %s", rest[1])
		}
		if err != nil {
			return err
		}
		fmt.Println(string(v))

	case "del":
		if len(rest) < 2 {
			return errors.New("kv del: need <key>")
		}
		n, err := c.Del(ctx, rest[1])
		if err != nil {
			return err
		}
		fmt.Printf("%d key(s) removed\n", n)

	default:
		return fmt.Errorf("kv: unknown subcommand %q", rest[0])
	}
	return nil
}

// ---------- 参数解析 ----------

// parseBytes 把 "2G" / "512M" / "1024" 解析成字节数（1024 进制）。
func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	unit := s[len(s)-1]
	mult := uint64(1)
	switch unit {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	case 't', 'T':
		mult = 1 << 40
	}
	num := s
	if mult > 1 {
		num = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if v <= 0 {
		return 0, fmt.Errorf("size must be positive: %q", s)
	}
	return uint64(v * float64(mult)), nil
}

// parseHours 把 "720h" / "30d" / "24" 解析成小时数（订单 expire_hours 使用）。
func parseHours(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return days * 24, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// 允许纯数字表示小时。
		h, perr := strconv.ParseFloat(s, 64)
		if perr != nil || h <= 0 {
			return 0, fmt.Errorf("invalid duration %q (use e.g. 720h or 30d)", s)
		}
		return h, nil
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive: %q", s)
	}
	return d.Hours(), nil
}
