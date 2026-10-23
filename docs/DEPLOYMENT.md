# 部署与测试说明

本文使用占位符描述 relay 和客户端的部署流程。把 `<relay-host>`、`<relay-port>`、`<code>` 和文件路径替换成实际值；示例不会假设某台服务器已经在线。

## 构建产物

运行 `scripts\build.bat`（Windows）或 `scripts/build.sh`（Linux/macOS），产物位于 `bin/`：

- `remote-assist-relay-<os>-<arch>`：relay 服务端；
- `remote-assist-cli-<os>-<arch>`：share/help 客户端；
- Windows 文件名带 `.exe` 后缀。

也可以按 [构建和测试指南](BUILD_GUIDE.md) 手动构建。

## 部署 Linux relay

relay 默认使用 TLS；未指定证书时会在 `--certs-dir` 下自动生成自签证书。生产环境应使用受信证书，并让客户端显式启用证书校验。

```bash
mkdir -p /opt/remote-assist/{bin,certs,logs}

# 前台验证
/opt/remote-assist/bin/remote-assist-relay-linux-amd64 run \
  --listen :8443 \
  --certs-dir /opt/remote-assist/certs \
  --audit /opt/remote-assist/logs/audit.jsonl

# 后台运行（证书已准备好时也可加 --cert 和 --key）
nohup /opt/remote-assist/bin/remote-assist-relay-linux-amd64 run \
  --listen :8443 \
  --certs-dir /opt/remote-assist/certs \
  --audit /opt/remote-assist/logs/audit.jsonl \
  > /opt/remote-assist/logs/relay.log 2>&1 &
echo $! > /opt/remote-assist/relay.pid
```

公网部署至少放行 TCP 8443；只有确实启用自建 STUN/UDP relay 时才放行 UDP 3478，并配置 `--stun :3478`。限流和来源 IP 设置见 [Relay 来源 IP、限流与监控指南](RELAY_LIMITS.md)。

## 连接测试

在被协助端运行 share：

```bash
remote-assist-cli-linux-amd64 share \
  --server <relay-host>:8443 \
  --insecure=false
```

程序显示协助码后，在协助端运行 help。默认 help 是 MCP 工具模式；要测试传统 SSH 隧道，显式加 `--legacy-ssh`：

```bash
remote-assist-cli-linux-amd64 help \
  --server <relay-host>:8443 \
  --code <code> \
  --legacy-ssh \
  --insecure=false
```

> `--server` 不是必填：省略它会使用内置公共 relay `23.95.78.14:8443`。上面的写法用于连接自建 relay；使用内置 relay 时可删掉 `--server <relay-host>:8443`，并保留默认的 `--insecure`。

如果 relay 使用自签证书，开发测试可把两端的 `--insecure=false` 改为 `--insecure`。这会跳过 TLS 证书校验；公网生产环境应改用受信 CA 或 `--ca`，不要使用 `--plain`。

同一局域网没有外部 relay 时，可以让 share 内嵌 TLS relay：

```bash
remote-assist-cli-linux-amd64 share --standalone --standalone-listen :8443
remote-assist-cli-linux-amd64 help --server <share-lan-ip>:8443 --code <code> --p2p disabled
```

## Windows relay 服务

Windows 推荐安装原生服务：

```powershell
.\remote-assist-relay-windows-amd64.exe service install
.\remote-assist-relay-windows-amd64.exe service start
.\remote-assist-relay-windows-amd64.exe service status
```

服务配置、证书目录、Event Log、升级和卸载行为见 [Windows Relay 服务部署](WINDOWS_RELAY_SERVICE.md)。

## 文件和日志

Linux 示例目录结构：

```text
/opt/remote-assist/
├── bin/
│   ├── remote-assist-relay-linux-amd64
│   └── remote-assist-cli-linux-amd64
├── certs/
│   ├── server.crt
│   └── server.key
├── logs/
│   ├── relay.log
│   └── audit.jsonl
└── relay.pid
```

Windows 服务默认使用：

- 程序：`C:\Program Files\RemoteAssistRelay\relay.exe`；
- 配置、证书和审计日志：`C:\ProgramData\RemoteAssistRelay\`；
- 普通运行日志：Windows Application Event Log。

## 运维操作

查看 Linux relay：

```bash
ps -ef | grep remote-assist-relay
ss -ltnp | grep ':8443'
tail -f /opt/remote-assist/logs/relay.log
```

停止并重启：

```bash
kill "$(cat /opt/remote-assist/relay.pid)"
nohup /opt/remote-assist/bin/remote-assist-relay-linux-amd64 run \
  --listen :8443 \
  --certs-dir /opt/remote-assist/certs \
  --audit /opt/remote-assist/logs/audit.jsonl \
  > /opt/remote-assist/logs/relay.log 2>&1 &
echo $! > /opt/remote-assist/relay.pid
```

Windows 服务使用 `service stop`、`service start` 和 `service status`，不要在服务已监听同一端口时再启动前台 relay。

## 建议的验收流程

1. 检查 relay 监听 TCP 8443，确认启动日志没有证书或配置错误。
2. share 创建协助码，help 使用该码连接；传统 SSH 测试加 `--legacy-ssh`。
3. 测试一个过期协助码（默认 TTL 为 30 分钟）和一个无效协助码。
4. 断开网络后确认客户端退出或按预期重连，并检查 relay 审计日志。
5. 公网环境确认客户端使用 `--insecure=false` 或 `--ca`，没有使用 `--plain` 或 `--no-auth`。

## 故障排除

- **无法连接**：确认 DNS、TCP 8443、防火墙和 relay 进程；再检查客户端与 relay 的地址、端口是否一致。
- **证书错误**：开发环境可使用 `--insecure`；生产环境配置受信 CA 并使用 `--insecure=false` 或 `--ca <file>`。
- **协助码无效或过期**：重新运行 share 获取新码；默认码有效期为 30 分钟。
- **P2P 失败**：将 `--p2p auto` 保持为默认值，客户端会回落到 TCP relay；需要诊断时可暂时使用 `--p2p disabled`。
- **服务端限流**：查看 relay 的采样日志和 [RELAY_LIMITS.md](RELAY_LIMITS.md)，只在确认合法流量被误限后调整 rate/burst。
