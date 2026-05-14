# Remote Assist Tool

基于命令行的远程协助工具，通过公网服务器中转实现安全的SSH隧道连接。

## 快速开始

### 构建

```bash
go mod tidy
go build -o bin/relay ./cmd/relay
go build -o bin/remote ./cmd/remote
```

### 使用方式

#### 1. 启动中转服务器

```bash
# 生成自签名证书并启动
bin/relay --listen :8443 --gen-certs --certs-dir ./certs

# 启动服务器
bin/relay --listen :8443 --cert ./certs/server.crt --key ./certs/server.key
```

#### 2. 被协助端（分享SSH访问）

```bash
bin/remote share --server relay.example.com:8443 --insecure
```

程序会显示协助码，例如：
```
协助码已生成: ABCD-EFGHIJ
有效期至: 2026-02-28 18:30:00

等待协助端连接...
```

#### 3. 协助端（使用协助码连接）

```bash
bin/remote help --server relay.example.com:8443 --code ABCD-EFGHIJ --insecure
```

连接成功后，在另一个终端：

```bash
ssh -p 2222 user@127.0.0.1
```

## 架构

```
┌───────────────────────────────────────────────────────────────┐
│                        公网中转服务器 (Relay)                    │
│                        TLS 1.3 + AES-256-GCM                    │
└─────────────────────────┬───────────────────────────────────────┘
                          │
              ┌───────────┴───────────┐
              │                       │
    ┌─────────▼─────────┐   ┌───────▼─────────┐
    │  被协助端 (Share)  │   │  协助端 (Help)   │
    │  remote-cli share  │   │  remote-cli help │
    └─────────┬─────────┘   └───────┬─────────┘
              │                       │
    ┌─────────▼─────────┐   ┌───────▼─────────┐
    │  本地 SSH Server  │   │  本地监听 2222   │
    │    127.0.0.1:22   │   │   127.0.0.1:2222│
    └───────────────────┘   └───────────────────┘
```

## 命令行参考

### relay - 中转服务器

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--listen` | :8443 | 监听地址 |
| `--cert` | - | TLS证书文件 |
| `--key` | - | TLS私钥文件 |
| `--ttl` | 30m | 协助码有效期 |
| `--length` | 10 | 协助码长度 |
| `--audit` | audit.log | 审计日志文件 |
| `--plain` | false | 使用非TLS模式（仅开发测试） |
| `--gen-certs` | false | 生成自签名证书后退出 |
| `--certs-dir` | ./certs | 证书目录 |

### remote share - 被协助模式

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--server` | localhost:8443 | 中转服务器地址 |
| `--ssh` | 127.0.0.1:22 | 本地SSH地址 |
| `--insecure` | false | 跳过TLS验证 |
| `--ca` | - | CA证书文件 |

### remote help - 协助模式

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--server` | localhost:8443 | 中转服务器地址 |
| `--code` | - | 协助码（必填） |
| `--listen` | 127.0.0.1:2222 | 本地监听地址 |
| `--insecure` | false | 跳过TLS验证 |
| `--ca` | - | CA证书文件 |

## 安全特性

- TLS 1.3 加密传输
- AES-256-GCM 加密算法
- 协助码使用安全随机数生成
- 完整的审计日志

## 与类似工具对比

| 特性 | 本工具 | frp | ZeroTier | ngrok |
|------|--------|-----|----------|-------|
| **核心功能** | SSH 隧道远程协助 | 反向代理 | 虚拟局域网 | 内网穿透 |
| **使用场景** | 临时协助 | 长期服务暴露 | 组建虚拟网络 | 临时公网访问 |
| **配置复杂度** | 极低（只需要一个协助码） | 中等（需要配置文件） | 中等（需要加入网络） | 低 |
| **自建服务** | ✅ 支持 | ✅ 支持 | ❌ 依赖官方（有自托管版本） | ❌ 依赖官方 |
| **连接方式** | Relay / 可选 P2P | 仅 Relay | P2P 优先 | 仅 Relay |
| **单次使用** | ✅ 专为临时设计 | ❌ 偏长期运行 | ❌ 偏长期组网 | ✅ 临时 |
| **安全性** | TLS + 协助码 | TLS + 令牌 | 加密 | TLS |

### 典型使用场景

| 场景 | 推荐工具 |
|------|----------|
| 帮朋友/同事解决电脑问题 | **本工具** |
| 在家访问公司内网服务器 | frp / ZeroTier |
| 临时暴露本地 Web 服务给客户 | ngrok |
| 组建多人游戏内网 | ZeroTier |
| 长期提供 SSH 访问 | frp |

## 本工具的优势

1. **极简设计** - `share` 生成码，`help` 用码连接，两步搞定
2. **协助码过期** - 30分钟自动失效，安全
3. **专注单一功能** - 只做远程协助，不复杂
4. **完全自托管** - 所有数据自己控制
5. **计划支持 P2P** - 未来可以直连不耗服务器流量

## Claude Code 远程调试（新）

让本地 Claude Code 直接调试远端机器，无需在远端开 openssh-server。
工具通道（9 个 MCP 工具）与原 SSH 模式并存，向后兼容。

### 远端启动 share（带沙箱）

```bash
remote share --server relay.example.com:8443 --root /path/to/project --insecure
```

控制台显示协助码（如 `ABCD-EFGHIJ`）与沙箱配置摘要。

### 本地配置 Claude Code

项目根目录或 `~/.claude/mcp.json`：

```jsonc
{
  "mcpServers": {
    "remote-debug": {
      "command": "remote",
      "args": [
        "help", "--server", "relay.example.com:8443",
        "--mcp-stdio", "--insecure"
      ]
    }
  }
}
```

注意 **不带 `--code`** —— 这是 bootstrap 模式，MCP server 启动时还不知道协助码。

**每次调试会话**：
1. 远端跑 `remote share`，拿到协助码 `ABCD-EFGHIJ`
2. 在 Claude Code 里直接说："**协助码 ABCD-EFGHIJ，连上去**"
3. Claude 调用 `remote-debug:connect("ABCD-EFGHIJ")` 完成握手
4. 其后所有调用直接走 9 个真实工具，不需要重启 Claude

`/mcp` 看到的工具：`connect / exec / read_file / write_file / list_dir / stat / glob / grep / process_list / tail_log`。

**旧用法（写死协助码）** 仍然支持：在 args 里加 `"--code", "ABCD-EFGHIJ"`，跳过 bootstrap 步骤直接连。

### 工具一览

- `exec` —— 远端运行 argv（不过 shell）
- `read_file` / `write_file` —— 受 `--root` 沙箱
- `list_dir` / `stat` / `glob` / `grep` —— 远端文件系统探索
- `process_list` —— 远端进程
- `tail_log` —— 日志尾随（支持 follow）

### 沙箱与安全

- `--root <dir>` 是文件操作沙箱根；未设置时默认 CWD。
- `--allow-exec a,b,c` / `--deny-exec rm,shutdown,...` 控制可执行命令。默认 deny: `rm,shutdown,reboot,mkfs,dd`。
- `--elevate`（仅 Windows）：启动时通过 UAC 请求管理员权限。
- `--unsafe-full-system`：**关闭所有沙箱**，启动时强制 5 秒红色确认倒计时。
- 工具调用流量以协助码派生的 session_key 做 XChaCha20-Poly1305 AEAD 加密，relay 看不见也不能伪造。

### 旧 SSH 模式

`remote help --legacy-ssh`（或不带 `--mcp-stdio`）即为原 SSH 隧道行为，向后兼容。
