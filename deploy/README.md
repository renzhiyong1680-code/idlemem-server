# 闲存云服务端部署（Linux / systemd）

把 `idlemem-server` 部署成云主机上的常驻服务：开机自启、崩溃自动重启、日志进 journald。

## 文件

| 文件 | 说明 |
|---|---|
| `xiancun-server.service` | systemd 单元（`/etc/systemd/system/xiancun-server.service`） |
| `install-server.sh` | 一键部署脚本（root/sudo 执行） |
| `config.json.example` | 生产配置模板（`/etc/xiancun/server-config.json`） |

## 快速开始

```bash
# 1. 构建（本机或 CI 产物）
go build -o bin/idlemem-server ./cmd/server

# 2. 上传到服务器后一键部署
sudo ./deploy/install-server.sh --binary ./bin/idlemem-server

# 3. 确认
systemctl status xiancun-server
journalctl -u xiancun-server -f
curl http://127.0.0.1:8080/healthz
```

也可以直接从 URL 拉取二进制（如 GitHub Release）：

```bash
sudo ./deploy/install-server.sh --binary https://example.com/idlemem-server-linux-amd64
```

使用自定义配置（跳过模板）：

```bash
sudo ./deploy/install-server.sh --binary ./bin/idlemem-server --config ./my-config.json
```

## 默认布局

| 路径 | 内容 |
|---|---|
| `/usr/local/bin/xiancun-server` | 服务端二进制 |
| `/etc/xiancun/server-config.json` | 配置文件 |
| `/var/lib/xiancun/` | 数据库与工作目录（`idlemem-server.db`） |
| 运行用户 | `xiancun`（脚本自动创建） |

## 常用运维命令

```bash
systemctl status xiancun-server        # 状态
journalctl -u xiancun-server -f        # 实时日志
systemctl restart xiancun-server       # 重启
systemctl disable --now xiancun-server # 停用并取消开机自启
```

## 生产注意事项

- **端口放行**：网关 `gateway_addr`（默认 `:6379`）需对客户端开放；REST API（默认 `:8080`）建议仅内网访问或经反向代理（Nginx/Caddy）加 TLS。
- **数据库备份**：`idlemem-server.db` 是唯一持久化文件，定期备份即可（缓存类数据允许丢失，冷备足够）。
- **多副本**：`default_replicas > 1` 可让同一 key 写入多台节点，容忍单节点掉线。
- **时长字段**：JSON 中为纳秒整数（`30s=30000000000`，`24h=86400000000000`）。

## 卸载

```bash
sudo systemctl disable --now xiancun-server
sudo rm /etc/systemd/system/xiancun-server.service /usr/local/bin/xiancun-server
sudo rm -rf /etc/xiancun /var/lib/xiancun
sudo systemctl daemon-reload
```
