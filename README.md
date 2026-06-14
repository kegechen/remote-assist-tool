# Remote Assist Tool

命令行远程协助工具：通过中转服务器（或 LAN 内直连 / NAT 穿透 P2P）建立安全隧道。既支持传统 SSH 远程协助，也支持让本地 **Claude Code 通过 MCP 工具通道直接调试远端机器**。

## 特性

- **两种协助通道**：经典 SSH 隧道；Claude Code MCP 工具通道（12 个远程工具，含文件上传/下载）。
- **三种连接方式**：公网 relay 中转；LAN 内 `--standalone` 进程内 relay 直连；NAT 穿透 P2P 直连（STUN 打洞，失败自动回落中转）。
- **分层加密**：relay 链路 TLS；MCP 工具通道额外以协助码派生密钥做 XChaCha20-Poly1305 AEAD —— 即使 relay 被攻破，也看不见、无法伪造工具内容。
- **沙箱**：MCP 工具受 `--root` 文件沙箱与 exec 命令黑/白名单约束。
- **relay 加固**：连接数限流（全局 + 每 IP）、读/写超时、单消息大小上限，抵御资源耗尽型 DoS / slowloris。

## 快速开始

### 构建

用仓库自带脚本，产物输出到 `bin/`（Windows 为 `.exe`）：

```bash
# Windows
build.bat
# Linux / macOS
./build.sh
# 在 Windows 上交叉编译 Linux 产物
build-linux.bat
```

生成 `bin/remote`（客户端，share/help 两种角色）和 `bin/relay`（中转服务器）。

> 提示：纯编译检查用 `go build ./...`（不产出文件）；要产物务必 `-o bin/` 或用上面的脚本，别让可执行文件散落到仓库根。

### 用法一：经典 SSH 远程协助（经 relay）

被协助端（分享本机 SSH）：

```bash
remote share --server relay.example.com:8443 --root /path/to/project
```

程序显示协助码，例如：

```
协助码已生成: ABCD-EFGHIJ
有效期至: 2026-02-28 18:30:00
等待协助端连接...
```

协助端（用协助码连接，默认走 MCP 模式；要传统 SSH 隧道加 `--legacy-ssh`）：

```bash
remote help --server relay.example.com:8443 --code ABCD-EFGHIJ --legacy-ssh
# 然后在另一个终端：
ssh -p 2222 user@127.0.0.1
```

> `--insecure` 默认 **true**：内置/standalone relay 使用自签证书，开箱即用。对接装有受信 CA 证书的 relay 时，请显式传 `--insecure=false` 启用证书校验。

### 用法二：LAN 直连（standalone，无需外部服务器）

被协助端进程内启动一个自签 TLS relay 并监听 LAN，help 端直接连这台机器，全程不依赖任何外部服务器：

```bash
remote share --standalone --standalone-listen :8443 --root /path/to/project
```

控制台会打印 help 端连接命令（自动探测 LAN IP）：

```
================ standalone (LAN) mode ================
Relay (TLS, self-signed) listening at: 192.168.1.23:8443 (LAN reachable)
Help side connects exactly like a normal relay (no --plain needed):
    remote help --server 192.168.1.23:8443 --code <code> --p2p disabled
=======================================================
```

> standalone 自签证书只覆盖 `localhost`，LAN 场景请保持默认 `--insecure=true`（勿用 `--insecure=false`，会因证书 SAN 不含 LAN IP 而失败）。

### 用法三：Claude Code MCP 远程调试

让本地 Claude Code 直接调试远端机器，无需在远端开 openssh-server。

远端启动 share（带沙箱）：

```bash
remote share --server relay.example.com:8443 --root /path/to/project
```

本地配置 Claude Code（项目根 `.mcp.json` 或 `~/.claude/mcp.json`）：

```jsonc
{
  "mcpServers": {
    "remote-debug": {
      "command": "remote",
      "args": ["help", "--server", "relay.example.com:8443", "--mcp-stdio"]
    }
  }
}
```

注意 **不带 `--code`** —— 这是 bootstrap 模式，MCP server 启动时还不知道协助码。

每次调试会话：

1. 远端跑 `remote share`，拿到协助码 `ABCD-EFGHIJ`；
2. 在 Claude Code 里直接说：“**协助码 ABCD-EFGHIJ，连上去**”；
3. Claude 调用 `remote-debug:connect("ABCD-EFGHIJ")` 完成握手；若是 standalone/LAN，可加地址：`connect("ABCD-EFGHIJ", server="192.168.1.23:8443")`；
4. 之后所有调用直接走真实工具，无需重启 Claude。

旧用法（写死协助码）仍支持：在 args 里加 `"--code", "ABCD-EFGHIJ"` 跳过 bootstrap。

## 架构

```
              ┌──────────────────────────────────────────┐
              │      公网 Relay（TLS）/ standalone 进程内   │
              │   仅转发；工具通道内容对 relay 不可见        │
              └───────────────┬──────────────┬────────────┘
                              │              │
              ┌───────────────┘              └───────────────┐
              │   （NAT 穿透成功则 P2P 直连，跳过 relay）       │
   ┌──────────▼──────────┐                      ┌────────────▼─────────┐
   │  被协助端 (Share)    │ ◀── 工具通道 AEAD ──▶ │   协助端 (Help)       │
   │  remote share        │      / SSH 隧道       │   remote help        │
   └──────────┬──────────┘                      └────────────┬─────────┘
              │                                               │
   ┌──────────▼──────────┐                      ┌────────────▼─────────┐
   │  沙箱内文件/exec      │                      │  本地 SSH :2222 /     │
   │  或本地 SSH :22       │                      │  Claude Code MCP      │
   └─────────────────────┘                      └──────────────────────┘
```

## 命令行参考

### relay — 中转服务器

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--listen` | `:8443` | 监听地址 |
| `--cert` / `--key` | - | TLS 证书 / 私钥文件 |
| `--ttl` | `30m` | 协助码有效期 |
| `--length` | `10` | 协助码长度 |
| `--audit` | `audit.log` | 审计日志文件 |
| `--stun` | `:3478` | STUN 服务监听地址（空则禁用） |
| `--plain` | `false` | 非 TLS 模式（仅开发测试） |
| `--gen-certs` | `false` | 生成自签证书后退出 |
| `--certs-dir` | `./certs` | 证书目录（未指定 cert/key 时自动在此生成自签证书） |
| `--version` | `false` | 显示版本 |

### remote share — 被协助模式

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--server` | `localhost:8443` | 中转服务器地址（也可用环境变量 `REMOTE_RELAY_SERVER` 覆盖） |
| `--insecure` | `true` | 跳过 TLS 校验（自签 relay 用；对接受信 CA relay 改 `false`） |
| `--ca` | - | CA 证书文件 |
| `--ssh` | `127.0.0.1:22` | 本地 SSH 地址（SSH 隧道模式） |
| `--p2p` | `auto` | P2P 模式：`disabled` / `auto` / `required` |
| `--stun` | - | STUN 服务地址（默认同 relay 的 :3478） |
| `--bind-ip` | - | 指定 UDP 绑定 IP（绕过 TUN 代理自动探测） |
| `--standalone` | `false` | 进程内启动 relay 并监听 `--standalone-listen`，LAN 直连场景 |
| `--standalone-listen` | `:8443` | standalone relay 监听地址 |
| `--code-file` | - | 注册后把协助码+有效期以 JSON 原子写到该文件（供宿主程序读取） |
| `--root` | - | 文件操作沙箱根（未设默认 CWD；`--unsafe-full-system` 除外） |
| `--allow-exec` | - | exec 命令白名单（逗号分隔，空=仅受黑名单约束） |
| `--deny-exec` | `rm,shutdown,reboot,mkfs,dd` | exec 命令黑名单 |
| `--elevate` | `false` | Windows：启动时经 UAC 请求管理员权限 |
| `--unsafe-full-system` | `false` | **危险**：完全关闭沙箱（启动有 5 秒红色确认倒计时） |
| `--plain` | `false` | 非 TLS（仅开发） |

### remote help — 协助模式

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--server` | `localhost:8443` | 中转服务器地址（可用 `REMOTE_RELAY_SERVER` 覆盖） |
| `--code` | - | 协助码（SSH/直连模式必填；MCP bootstrap 模式留空，由 `connect` 工具提供） |
| `--insecure` | `true` | 跳过 TLS 校验（同 share） |
| `--ca` | - | CA 证书文件 |
| `--listen` | `127.0.0.1:2222` | 本地监听地址（SSH 隧道模式） |
| `--p2p` | `auto` | P2P 模式：`disabled` / `auto` / `required` |
| `--stun` | - | STUN 服务地址 |
| `--bind-ip` | - | 指定 UDP 绑定 IP |
| `--mcp-stdio` | `false` | 作为 MCP stdio server 运行（供 Claude Code） |
| `--legacy-ssh` | `false` | 强制传统 SSH 隧道模式（不加 `--mcp-stdio` 时即此模式） |
| `--plain` | `false` | 非 TLS（仅开发） |

## MCP 工具一览（12 个）

| 工具 | 说明 |
|------|------|
| `connect` | 用协助码与远端配对，必须先调用；可选 `server=` 覆盖 relay 地址（用于 standalone/LAN） |
| `exec` | 远端按 argv 运行命令（不过 shell） |
| `read_file` / `write_file` | 读 / 写远端文件（受 `--root` 沙箱；单次最多 1 MiB） |
| `list_dir` / `stat` / `glob` / `grep` | 远端文件系统探索 |
| `process_list` | 列远端进程 |
| `tail_log` | 读远端日志末尾 N 行 |
| `upload_file` | 把本地文件分块（512 KiB）推送到远端 —— DLL/EXE/zip 等二进制 |
| `download_file` | 把远端文件分块拉到本地 —— crash dump / 日志归档等 |

`upload_file` / `download_file` 为 host 端复合工具，内部循环调用 `write_file` / `read_file`，share 端零改动。

## 安全特性

- relay 链路 TLS（自签或受信 CA）；`--insecure` 控制是否校验。
- MCP 工具通道以协助码派生 session key（HKDF-SHA256）做 XChaCha20-Poly1305 AEAD：relay 仅转发密文，看不见也无法伪造工具内容。
- 协助码：安全随机生成（54 字符集 × 10 位，去除易混淆字符），默认 30 分钟过期。
- 文件沙箱 `--root` + exec 黑/白名单；`--unsafe-full-system` 才解除，且有显式确认。
- relay 服务端加固：全局 + 每 IP 连接数上限、读/写超时、单消息大小上限。
- 完整审计日志。

## 与类似工具对比

| 特性 | 本工具 | frp | ZeroTier | ngrok |
|------|--------|-----|----------|-------|
| **核心功能** | SSH/MCP 远程协助 | 反向代理 | 虚拟局域网 | 内网穿透 |
| **使用场景** | 临时协助 / 远程调试 | 长期服务暴露 | 组建虚拟网络 | 临时公网访问 |
| **配置复杂度** | 极低（一个协助码） | 中等 | 中等 | 低 |
| **自建服务** | ✅ 支持 | ✅ 支持 | ❌（有自托管版） | ❌ 依赖官方 |
| **连接方式** | Relay / P2P / LAN 直连 | 仅 Relay | P2P 优先 | 仅 Relay |
| **单次使用** | ✅ 专为临时设计 | ❌ 偏长期 | ❌ 偏长期 | ✅ 临时 |
| **AI 集成** | ✅ Claude Code MCP | ❌ | ❌ | ❌ |

## 本工具的优势

1. **极简** —— `share` 生成码，`help`/Claude 用码连接，两步搞定。
2. **协助码过期** —— 默认 30 分钟自动失效。
3. **三种连接** —— 公网中转、LAN 直连、P2P 直连按场景自动选择。
4. **完全自托管** —— 数据自己掌控。
5. **AI 原生** —— 直接挂到 Claude Code 当远程调试 MCP。
