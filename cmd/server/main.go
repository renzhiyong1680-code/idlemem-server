// Command server 是闲存云调度控制面（平台后端）的入口。
//
// 启动后提供：
//   - REST 管理 API（节点注册/心跳、租户、订单、分片、概览）
//   - Redis 协议网关（企业客户接入，AUTH 租户隔离）
//   - 后台任务：节点心跳超时扫描（Sweep）与分片自愈（Rebalance）
//
// 配置：-config <json 文件>（缺省使用默认配置）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/renzhiyong1680-code/idlemem-server/internal/api"
	"github.com/renzhiyong1680-code/idlemem-server/internal/billing"
	"github.com/renzhiyong1680-code/idlemem-server/internal/config"
	"github.com/renzhiyong1680-code/idlemem-server/internal/gateway"
	"github.com/renzhiyong1680-code/idlemem-server/internal/meter"
	"github.com/renzhiyong1680-code/idlemem-server/internal/node"
	"github.com/renzhiyong1680-code/idlemem-server/internal/scheduler"
	"github.com/renzhiyong1680-code/idlemem-server/internal/store"
)

// version 由构建脚本通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	cfgPath := flag.String("config", "", "config json file path (optional)")
	genToken := flag.Bool("gen-token", false, "generate a random admin_token/node_token pair and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("idlemem-server %s\n", version)
		return
	}

	if *genToken {
		admin, err := api.GenerateToken()
		if err != nil {
			log.Fatalf("generate token: %v", err)
		}
		node, err := api.GenerateToken()
		if err != nil {
			log.Fatalf("generate token: %v", err)
		}
		fmt.Printf("admin_token: %s\nnode_token: %s\n", admin, node)
		return
	}

	logger := log.New(os.Stdout, "[server] ", log.LstdFlags|log.Lmsgprefix)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}

	// 持久化存储。
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// 节点注册表。
	reg := node.New(st, cfg.HeartbeatTimeout, cfg.HeartbeatEvictAfter)
	// 调度器。
	sch := scheduler.New(st, reg, cfg.DefaultReplicas)
	// 计量器（GB·h，计费/结算依据）。
	mtr := meter.New(st, cfg.MeterInterval, logger)

	// REST API。
	price := billing.Price{
		CustomerPerGBHour: cfg.CustomerPricePerGBHour,
		SupplierPerGBHour: cfg.SupplierPricePerGBHour,
	}.Normalize()
	apiSrv := api.New(st, reg, sch, mtr, price, logger)
	apiSrv.SetTokens(cfg.AdminToken, cfg.NodeToken)
	if cfg.AdminToken == "" {
		logger.Printf("WARNING: admin_token is empty, the management API is unauthenticated (dev only)")
	}
	if cfg.NodeToken == "" {
		logger.Printf("WARNING: node_token is empty, node registration/heartbeat is unauthenticated")
	}
	httpSrv := &http.Server{
		Addr:    cfg.APIAddr,
		Handler: apiSrv.Handler(),
	}

	// Redis 网关。
	gw, err := gateway.New(cfg.GatewayAddr, st, "", logger)
	if err != nil {
		logger.Fatalf("create gateway: %v", err)
	}
	defer gw.Close()

	// 后台任务上下文。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 启动后台扫描：节点超时清扫 + 分片自愈。
	go backgroundLoop(ctx, reg, sch, cfg.HeartbeatTimeout, gw, logger)

	// 启动 GB·h 计量（周期采样在线容量）。
	if cfg.MeterInterval > 0 {
		go mtr.Run(ctx)
	} else {
		logger.Printf("meter: disabled (meter_interval=0)")
	}

	// 启动 HTTP API。
	go func() {
		logger.Printf("api: REST listening on %s", cfg.APIAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("api server: %v", err)
		}
	}()

	// 启动 Redis 网关（阻塞）。
	go func() {
		if err := gw.ListenAndServe(); err != nil {
			logger.Fatalf("gateway server: %v", err)
		}
	}()

	logger.Printf("idlemem-server started (version=%s db=%s)", version, cfg.DBPath)

	<-ctx.Done()
	logger.Printf("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// backgroundLoop 周期性执行节点清扫与分片自愈。
func backgroundLoop(ctx context.Context, reg *node.Registry, sch *scheduler.Scheduler, interval time.Duration, gw *gateway.Gateway, logger *log.Logger) {
	// 心跳超时的一半作为扫描间隔。
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval / 2)
	defer ticker.Stop()

	round := 0
	runOnce := func() {
		round++
		// 每 4 轮（约 1 分钟）校准一次租户已写字节，纠正 TTL 过期造成的计数漂移。
		if gw != nil && round%4 == 0 {
			gw.RecountUsage()
		}
		offline, evicted, err := reg.Sweep()
		if err != nil {
			logger.Printf("sweep error: %v", err)
		}
		if len(offline) > 0 {
			logger.Printf("nodes marked offline: %v", offline)
		}
		if len(evicted) > 0 {
			logger.Printf("nodes evicted: %v", evicted)
		}
		fixed, err := sch.Rebalance()
		if err != nil {
			logger.Printf("rebalance error: %v", err)
		}
		if fixed > 0 {
			logger.Printf("shards rebalanced: %d", fixed)
		}
		orders, shards, err := sch.ReleaseExpired(time.Now())
		if err != nil {
			logger.Printf("order expiry error: %v", err)
		}
		if orders > 0 {
			logger.Printf("orders expired: %d (shards released: %d)", orders, shards)
		}
	}

	runOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
