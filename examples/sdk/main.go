// 企业接入示例（推荐方式）：使用官方 Go SDK 访问闲存云缓存网关。
//
// 运行：
//
//	go run ./examples/sdk -addr 127.0.0.1:6379 -token <租户 Token>
//
// SDK 相比手写 RESP 的优势：自动重连与重新鉴权、超时控制、错误语义化
// （键不存在返回 ErrNotFound）、连接池。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/renzhiyong1680-code/idlemem-server/client"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "网关地址 host:port")
	token := flag.String("token", "", "租户 Token")
	key := flag.String("key", "demo:user:1", "演示 key")
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "缺少 -token（在控制面创建租户时返回）")
		os.Exit(2)
	}
	if *key == "" {
		fmt.Fprintln(os.Stderr, "-key 不能为空")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, err := client.Dial(*addr, *token, client.WithTimeout(5*time.Second))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()
	log.Println("connected and authenticated")

	if err := c.Ping(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}
	log.Println("ping ok")

	// 写入：带 60s TTL 的会话缓存。
	value := []byte(`{"name":"alice","plan":"pro"}`)
	if err := c.Set(ctx, *key, value, time.Minute); err != nil {
		log.Fatalf("set: %v", err)
	}
	log.Printf("set: %s (ttl 60s)", *key)

	got, err := c.Get(ctx, *key)
	if err != nil {
		log.Fatalf("get: %v", err)
	}
	log.Printf("get: %s", got)

	ttl, err := c.TTL(ctx, *key)
	if err != nil {
		log.Fatalf("ttl: %v", err)
	}
	log.Printf("ttl: %v", ttl)

	// 键不存在时返回 ErrNotFound，便于业务分支处理。
	if _, err := c.Get(ctx, *key+":missing"); errors.Is(err, client.ErrNotFound) {
		log.Println("missing key correctly reported as ErrNotFound")
	} else {
		log.Fatalf("missing key err = %v, want ErrNotFound", err)
	}

	n, err := c.Del(ctx, *key)
	if err != nil {
		log.Fatalf("del: %v", err)
	}
	log.Printf("del: %d key(s) removed", n)
	log.Println("sdk demo done.")
}
