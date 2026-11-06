package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer 最小 RESP 服务端：模拟网关的 AUTH/SET/GET/DEL/EXPIRE/TTL。
type fakeServer struct {
	ln    net.Listener
	mu    sync.Mutex
	data  map[string]string
	quit  bool
	conns int
}

func newFakeServer(t *testing.T, token string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fs := &fakeServer{ln: ln, data: map[string]string{}}
	t.Cleanup(func() { _ = ln.Close() })
	go fs.serve(token)
	return fs
}

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

func (f *fakeServer) serve(token string) {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		go f.handle(conn, token)
	}
}

func (f *fakeServer) handle(conn net.Conn, token string) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	authed := token == ""
	for {
		args, err := readCommand(br)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		switch strings.ToUpper(args[0]) {
		case "AUTH":
			if len(args) == 2 && args[1] == token {
				authed = true
				fmt.Fprint(conn, "+OK\r\n")
			} else {
				fmt.Fprint(conn, "-ERR invalid token\r\n")
			}
		case "PING":
			fmt.Fprint(conn, "+PONG\r\n")
		default:
			if !authed {
				fmt.Fprint(conn, "-NOAUTH Authentication required\r\n")
				continue
			}
			f.mu.Lock()
			switch strings.ToUpper(args[0]) {
			case "SET":
				f.data[args[1]] = args[2]
				fmt.Fprint(conn, "+OK\r\n")
			case "GET":
				if v, ok := f.data[args[1]]; ok {
					fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(v), v)
				} else {
					fmt.Fprint(conn, "$-1\r\n")
				}
			case "DEL":
				n := 0
				for _, k := range args[1:] {
					if _, ok := f.data[k]; ok {
						delete(f.data, k)
						n++
					}
				}
				fmt.Fprintf(conn, ":%d\r\n", n)
			case "TTL":
				if _, ok := f.data[args[1]]; ok {
					fmt.Fprint(conn, ":59\r\n")
				} else {
					fmt.Fprint(conn, ":-1\r\n")
				}
			case "EXPIRE":
				fmt.Fprint(conn, ":1\r\n")
			default:
				fmt.Fprint(conn, "-ERR unsupported\r\n")
			}
			f.mu.Unlock()
		}
	}
}

func readCommand(br *bufio.Reader) ([]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("bad command %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hdr, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		l, err := strconv.Atoi(hdr[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := br.Read(buf); err != nil {
			return nil, err
		}
		out = append(out, string(buf[:l]))
	}
	return out, nil
}

func TestClientBasic(t *testing.T) {
	fs := newFakeServer(t, "tok")
	c, err := Dial(fs.addr(), "tok")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := c.Set(ctx, "k1", []byte("v1"), time.Minute); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, err := c.Get(ctx, "k1")
	if err != nil || string(v) != "v1" {
		t.Fatalf("get = %q, err %v", v, err)
	}
	d, err := c.TTL(ctx, "k1")
	if err != nil || d != 59*time.Second {
		t.Fatalf("ttl = %v, err %v", d, err)
	}
	n, err := c.Del(ctx, "k1")
	if err != nil || n != 1 {
		t.Fatalf("del = %d, err %v", n, err)
	}
	if _, err := c.Get(ctx, "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing err = %v, want ErrNotFound", err)
	}
}

func TestClientBadToken(t *testing.T) {
	fs := newFakeServer(t, "tok")
	if _, err := Dial(fs.addr(), "wrong"); err == nil {
		t.Fatal("dial with wrong token should fail")
	}
}

// TestClientReconnect 服务端断开后，下一条命令应自动重连并重新鉴权。
func TestClientReconnect(t *testing.T) {
	fs := newFakeServer(t, "tok")
	c, err := Dial(fs.addr(), "tok")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// 从服务端侧强制断开当前连接。
	fs.mu.Lock()
	fs.data["k"] = "v"
	fs.mu.Unlock()
	c.mu.Lock()
	_ = c.conn.Close()
	c.mu.Unlock()

	ctx := context.Background()
	v, err := c.Get(ctx, "k")
	if err != nil || string(v) != "v" {
		t.Fatalf("get after reconnect = %q, err %v", v, err)
	}
}

func TestPool(t *testing.T) {
	fs := newFakeServer(t, "tok")
	p, err := NewPool(fs.addr(), "tok", 2)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	a, b := p.Get(), p.Get()
	if a == nil || b == nil || a == b {
		t.Fatal("pool should hand out distinct clients")
	}
	if err := a.Set(context.Background(), "pk", []byte("pv"), 0); err != nil {
		t.Fatalf("pool set: %v", err)
	}
}
