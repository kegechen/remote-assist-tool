# Remote Assist Tool

命令行远程协助工具：通过中转服务器（或 LAN 内直连 / NAT 穿透 P2P）建立安全隧道。既支持传统 SSH 远程协助，也支持让本地 **Claude Code 通过 MCP 工具通道直接调试远端机器**。

## 特性

- **两种协助通道**：经典 SSH 隧道；Claude Code MCP 工具通道（12 个远程工具，含文件上传/下载）。
- **三种连接方式**：公网 relay 中转；LAN 内 `--standalone` 进程内 relay 直连；NAT 穿透 P2P 直连（STUN 打洞，失败自动回落中转）。
- **分层加密**：relay 链路 TLS；MCP 工具通道额外以协助码派生密钥做 XChaCha20-Poly1305 AEAD —— 即使 relay 被攻破，也看不见、无法伪造工具内容。
- **护栏**：exec 命令黑/白名单；可选 `--root` 把文件工具限制在某子树内（防手滑，非安全边界 —— exec 不受其约束）。
- **relay 加固**：连接数限流（全局 + 每 IP）、读/写超时、单消息大小上限，抵御资源耗尽型 DoS / slowloris。

## 快速开始

### 一键安装 remote

脚本会从 GitHub 最新正式 Release 下载当前系统与架构对应的客户端，并安装为
`~/.local/bin/remote`（Windows 的 Git Bash / MSYS2 / Cygwin 中为 `remote.exe`）：

```bash
curl -fsSL https://raw.githubusercontent.com/kegechen/remote-assist-tool/master/install.sh | sh
```

也可以使用 `wget`，或通过 `REMOTE_INSTALL_DIR` 指定安装目录：

```bash
wget -qO- https://raw.githubusercontent.com/kegechen/remote-assist-tool/master/install.sh | sh
REMOTE_INSTALL_DIR=/usr/local/bin sh install.sh
```

支持 Linux、macOS 和 Windows（Git Bash / MSYS2 / Cygwin）的 amd64、arm64 架构；
Linux/macOS 下载后会自动添加可执行权限；Windows 中 `curl` / `wget` 下载失败时会自动回退到
系统 PowerShell。

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

产物都带平台后缀（`<组件>-<os>-<arch>`），一眼看得出这个二进制给谁跑，也不会跟 PATH 里
别的东西撞名：

| 产物 | 说明 |
|---|---|
| `bin/remote-assist-cli-windows-amd64.exe` | 客户端，share / help 两种角色，也是 MCP 入口 |
| `bin/remote-assist-webui-windows-amd64.exe` | 浏览器控制台（会去同目录/`bin/` 找上面那个 cli） |
| `bin/remote-assist-relay-windows-amd64.exe` | 中转服务器 |
| `bin/remote-assist-{cli,relay}-linux-{amd64,arm64}` | `build-linux.bat` 交叉编译产出 |

Windows 产物内嵌产品信息（右键属性可见产品名/描述/版本），由 `goversioninfo` 按
`git describe` 生成。该工具已用 `tools.go` 钉进 `go.mod`：`go mod download` 一次之后
**离线也能构建**，不必每次联网。

> 提示：纯编译检查用 `go build ./...`（不产出文件）；要产物务必 `-o bin/` 或用上面的脚本，别让可执行文件散落到仓库根。

### Web UI 内升级 share

Web UI 连接后会比较 help 与 share 的版本；确认 share 较旧时，顶部显示升级提示。选择与
远端系统和架构匹配的 CLI 二进制后，后端按“先建后断”完成交接：

1. 经 old 通道探测 PID、原可执行文件路径、系统与架构；上传前检查候选文件的 ELF/PE
   machine，确认 Linux/Windows 与 amd64/arm64 均匹配后才分块上传，再在远端执行
   `--version` 验证。
2. 复用 old 的 share 参数，以隔离 `HOME`（独立 ClientID）和固定 `--code-file` 启动 new。
   升级默认实例时，隔离 `HOME` 不改变其单实例锁：old 持锁期间 new 排队等待接管，因此
   升级交接过程中再次启动默认 share 仍会直接报错。
3. 仍经 old 通道读取 new code，主动连接 new 并核对版本。
4. Linux 在 new 验证成功后原子替换原文件，再按 PID 终止 old。Windows 先把运行中的 old
   改成备份名、让候选文件占回原路径，再启动 new；new 验证成功后按 PID 终止 old 并删除
   备份，切换失败则经 old 通道恢复原文件。

切换前任一步失败都不会终止 old；连接 new 失败会尝试自动回连 old。当前仅支持使用外部
relay 的 Linux/Windows amd64/arm64 share，`--standalone` / `--no-auth` 会被拒绝。
如果配置了 `--allow-exec`，升级所需的 `sh`/`powershell.exe`、`mkdir`、`chmod`、`kill` 及候选二进制也必须
获准执行。原路径替换后，已有 systemd/开机启动配置无需改文件名；当前临时接管进程继续
使用隔离 ClientID，下一次由原路径正常启动时恢复标准 HOME/ClientID。

### 用法一：经典 SSH 远程协助（经 relay）

被协助端（分享本机 SSH）：

```bash
remote share --server relay.example.com:8443
```

程序显示协助码，例如：

```
协助码已生成: ABCD-EFGHIJ
有效期至: 2026-02-28 18:30:00
等待协助端连接...
```

同一用户下默认只允许一个 share。需要额外分享不同 SSH 服务或不同工具策略时，显式创建
独立实例：

```bash
remote share --ssh 127.0.0.1:22 --server relay.example.com:8443
remote share --new-instance --ssh 127.0.0.1:2222 --server relay.example.com:8443
```

默认实例重复启动会直接报错，不会断开已运行的实例。每次使用 `--new-instance` 都会创建
独立协助码；在协助码有效期内，该进程网络断线重连时保持原码，但进程退出后再次启动会
生成新码。连接外部 relay 时 share 只建立出站连接，无需为每个实例分配本地监听端口；
`--ssh` 端口只是其代理的本地 SSH 服务。standalone 模式下每个实例内嵌一个 relay，因此
还必须分别设置不同的 `--standalone-listen` 端口。

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
remote share --standalone --standalone-listen :8443
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
首次接入可直接把 [`MCP_SETUP.md`](MCP_SETUP.md) 交给 Claude Code 或 Codex，自动完成
`remote` 下载与 `remote-debug` MCP 配置。

远端启动 share（带沙箱）：

```bash
remote share --server relay.example.com:8443
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

> 若 `connect` 直接报 `Transport closed`，且没有返回 `session_id` / `peer_host`，请求通常
> 尚未到达 `remote` CLI。不要据此判断协助码失效或 relay 故障；先重启受影响的 Claude
> Code / Codex 进程，再用同一码重试。旧 stdio 句柄一旦被宿主取消，反复调用 `connect`
> 无法恢复。详细证据与排查步骤见 [`MCP_SETUP.md`](MCP_SETUP.md#4-排查-transport-closed)
> 和 [`remote-debug` MCP 接入参考](docs/remote-debug-MCP接入参考.md#7-排错速查)。

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
| `--stun` | 空 | STUN/UDP relay 监听地址；例如 `:3478`，空则禁用 |
| `--trust-source-ip` | `true` | Relay 是否能从 TCP 连接看到真实来源 IP；SNAT 后端设为 `false` |
| `--limits-file` | `$REMOTE_RELAY_LIMITS_FILE` | JSON 限流配置文件；未设置则使用安全默认值 |
| `--print-default-limits` | `false` | 输出完整默认限流 JSON 后退出 |
| `--no-auth` | `false` | 固定码无鉴权模式，仅限完全可信私网 |
| `--plain` | `false` | 非 TLS 模式（仅开发测试） |
| `--gen-certs` | `false` | 生成自签证书后退出 |
| `--certs-dir` | `./certs` | 证书目录（未指定 cert/key 时自动在此生成自签证书） |
| `--version` | `false` | 显示版本 |

Relay 的来源 IP 判断、完整限流参数、默认值依据、公共 STUN 行为和部署监控方法见 [Relay 来源 IP、限流与监控指南](docs/RELAY_LIMITS.md)。

Windows 下直接双击无参数的 relay 会打开服务管理菜单。也可以使用非交互命令安装和管理原生 Windows 服务：

```powershell
remote-assist-relay-windows-amd64.exe service install
remote-assist-relay-windows-amd64.exe service start
remote-assist-relay-windows-amd64.exe service status
remote-assist-relay-windows-amd64.exe service stop
remote-assist-relay-windows-amd64.exe service uninstall
```

服务程序安装到 `C:\Program Files\RemoteAssistRelay`，配置、证书和审计文件位于 `C:\ProgramData\RemoteAssistRelay`，运行日志写入 Windows Application Event Log。服务模式、配置格式、权限和卸载行为见 [Windows Relay 服务部署](docs/WINDOWS_RELAY_SERVICE.md)。原有前台参数保持兼容；显式使用 `run` 可避免与服务管理命令混淆：

```powershell
remote-assist-relay-windows-amd64.exe run --listen :8443 --ttl 1h
```

### remote share — 被协助模式

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `--server` | `localhost:8443` | 中转服务器地址（也可用环境变量 `REMOTE_RELAY_SERVER` 覆盖） |
| `--insecure` | `true` | 跳过 TLS 校验（自签 relay 用；对接受信 CA relay 改 `false`） |
| `--ca` | - | CA 证书文件 |
| `--ssh` | `127.0.0.1:22` | 本地 SSH 地址（SSH 隧道模式） |
| `--new-instance` | `false` | 额外启动独立 share；生成新协助码且不影响默认实例 |
| `--p2p` | `auto` | P2P 模式：`disabled` / `auto` / `required` |
| `--stun` | - | STUN 服务地址（默认同 relay 的 :3478） |
| `--bind-ip` | - | 指定 UDP 绑定 IP（绕过 TUN 代理自动探测） |
| `--standalone` | `false` | 进程内启动 relay 并监听 `--standalone-listen`，LAN 直连场景 |
| `--standalone-listen` | `:8443` | standalone relay 监听地址 |
| `--code-file` | - | 注册后把协助码+有效期以 JSON 原子写到该文件（供宿主程序读取） |
| `--root` | - | 可选：把文件工具限制在该子树（未设 = 不限制）。防手滑，非安全边界 |
| `--allow-exec` | - | exec 命令白名单（逗号分隔，空=仅受黑名单约束） |
| `--deny-exec` | `rm,shutdown,reboot,mkfs,dd` | exec 命令黑名单 |
| `--elevate` | `false` | Windows：启动时经 UAC 请求管理员权限 |
| `--unsafe-exec` | `false` | **危险**：关闭 exec 黑/白名单，任意命令可跑（启动有 5 秒红色确认倒计时）。不影响 `--root` |
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
| `read_file` / `write_file` | 读 / 写远端文件（设了 `--root` 则限于该子树；单次最多 1 MiB） |
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
- **信任边界是协助码**：share 由本机用户主动发起，码交给谁，就等于把这台机器交给谁。`--root` / exec 名单是防手滑的护栏，不是对抗恶意方的边界 —— exec 可跑任意程序，一句 `sh -c 'cp /etc/passwd <root>/'` 即可绕过 `--root`。需要真隔离请在进程外面套（容器 / 专用低权限账号）。
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
