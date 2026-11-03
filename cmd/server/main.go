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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xiancuncloud/idlemem-server/internal/api"
	"github.com/xiancuncloud/idlemem-server/internal/config"
	"github.com/xiancuncloud/idlemem-server/internal/gateway"
	"github.com/xiancuncloud/idlemem-server/internal/node"
	"github.com/xiancuncloud/idlemem-server/internal/scheduler"
	"github.com/xiancuncloud/idlemem-server/internal/store"
)

func main() {
	cfgPath := flag.String("config", "", "config json file path (optional)")
	flag.Parse()

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

	// REST API。
	apiSrv := api.New(st, reg, sch, logger)
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
	go backgroundLoop(ctx, reg, sch, cfg.HeartbeatTimeout, logger)

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

	logger.Printf("idlemem-server started (db=%s)", cfg.DBPath)

	<-ctx.Done()
	logger.Printf("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// backgroundLoop 周期性执行节点清扫与分片自愈。
func backgroundLoop(ctx context.Context, reg *node.Registry, sch *scheduler.Scheduler, interval time.Duration, logger *log.Logger) {
	// 心跳超时的一半作为扫描间隔。
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval / 2)
	defer ticker.Stop()

	runOnce := func() {
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
