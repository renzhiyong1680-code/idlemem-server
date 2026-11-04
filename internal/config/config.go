// Package config 提供服务端配置。
package config

import (
	"encoding/json"
	"os"
	"time"
)

// Config 服务端全部配置项。
type Config struct {
	// APIAddr REST 管理 API 监听地址。
	APIAddr string `json:"api_addr"`
	// GatewayAddr Redis 协议网关监听地址。
	GatewayAddr string `json:"gateway_addr"`
	// DBPath 持久化数据库文件路径。
	DBPath string `json:"db_path"`

	// HeartbeatTimeout 心跳超时（超过未上报判定离线）。
	HeartbeatTimeout time.Duration `json:"heartbeat_timeout"`
	// HeartbeatEvictAfter 离线节点从调度池剔除的时间（清理）。
	HeartbeatEvictAfter time.Duration `json:"heartbeat_evict_after"`
	// DefaultReplicas 默认分片副本数。
	DefaultReplicas int `json:"default_replicas"`
	// DefaultTTL 网关 KV 默认过期时间（0 表示不过期）。
	DefaultTTL time.Duration `json:"default_ttl"`
	// MeterInterval 计量采样周期（0 表示关闭计量）。
	MeterInterval time.Duration `json:"meter_interval"`
}

// Default 返回适用于本地开发/小规模内测的默认配置。
func Default() Config {
	return Config{
		APIAddr:            ":8080",
		GatewayAddr:        ":6379",
		DBPath:             "idlemem-server.db",
		HeartbeatTimeout:   30 * time.Second,
		HeartbeatEvictAfter: 24 * time.Hour,
		DefaultReplicas:    1,
		DefaultTTL:         0,
		MeterInterval:      60 * time.Second,
	}
}

// Load 从 JSON 文件加载配置；文件不存在或为空时返回默认配置。
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
