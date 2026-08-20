# `remote-debug` MCP Server 接入参考

> **先澄清一个常见误解**：`remote-debug` **不是 skill、也不是 plugin，而是一个 MCP Server**。
> 它由本地的 `remote help --mcp-stdio` 进程提供，通过 stdio 跟 Claude Code 通信。
> 你在 `.mcp.json` 里给它起的名字（`mcpServers.remote-debug`）就是它在 `/mcp` 里显示的名字。
>
> 想要 `/remote-debug <code>` 这种 slash 命令式的一键体验，可以**另外**写一个 skill 去封装它——
> 但底层能力来自这个 MCP server，本文档讲的就是这一层。

本文是接入速查表；面向「为什么用、怎么宣传」的整体介绍见 [`远程调试工具分享.md`](远程调试工具分享.md)。

---

## 1. 它是什么 / 怎么跑起来的

```
Claude Code
   │  MCP over stdio
   ▼
remote help --mcp-stdio   ← 这就是 "remote-debug" MCP server（本地进程）
   │  TLS 隧道 / P2P / standalone loopback
   ▼
remote share              ← 远端被调试机器（提供协助码 + 沙箱）
```

- MCP server = 本地 `remote help --mcp-stdio`。
- 它向 Claude 暴露 **10 个工具**：`connect` + 9 个作用于远端的真实工具。
- 启动时**不连任何远端**；直到 Claude 调用 `connect(code)` 才完成配对。

---

## 2. 配置 `.mcp.json`（bootstrap 模式，推荐）

项目根 `.mcp.json` 或 `~/.claude/mcp.json`：

```jsonc
{
  "mcpServers": {
    "remote-debug": {
      "command": "D:\\path\\to\\remote-windows-amd64.exe",
      "args": ["help", "--server", "relay.example.com:8443", "--mcp-stdio", "--insecure"]
    }
  }
}
```

| 要点 | 说明 |
|---|---|
| **不写 `--code`** | bootstrap 模式：协助码每次会话由 `connect` 工具临时提供，换机器/换会话**不用重启 Claude** |
| `--server` | 默认 relay 地址；可被 `connect(server=...)` 临时覆盖（standalone/LAN 场景） |
| `--insecure` | 自签名证书时跳过 TLS 校验；正式 CA 证书可去掉 |
| `--plain` | 仅当 relay 跑在明文模式时才用（如局域网）；公网请用 TLS |

> **旧写法（写死协助码）** 仍支持：args 里加 `"--code", "ABCD-EFGHIJ"`，跳过 bootstrap，启动即连。
> 缺点是换码必须改配置 + 重启 Claude，所以日常推荐 bootstrap。

---

## 3. 连接流程（一次会话）

1. **远端**跑 `remote share`，拿到协助码（如 `ABCD-EFGHIJ`，30 分钟有效）。
2. **本地**在 Claude 里说一句话：
   - 公网/默认 relay：`协助码 ABCD-EFGHIJ，连上去`
   - standalone/LAN：`协助码 ABCD-EFGHIJ 在 192.168.1.50:8443`
3. Claude 调 `connect(...)` 完成握手，之后 9 个工具直通远端。
4. 协助码过期/换机器：远端重跑 `share`，再说一句新码即可（无需重启）。

---

## 4. 工具清单（参数以实现 `internal/mcp/schema.go` 为准）

> ⚠️ **v1 现状**：`exec` **不支持流式**（执行完一次性返回）；`tail_log` **不支持 follow**（只读末尾 N 行）。设计稿里提过的 stream/follow 尚未落地，文档以实际实现为准。

| 工具 | 必填 | 可选 | 说明 |
|---|---|---|---|
| `connect` | `code` | `server` | 配对，必须**最先调用**；未连接时其他工具返回 `not_connected`。`server` 可临时覆盖 relay 地址（standalone/LAN） |
| `exec` | `argv` (string[]) | `cwd`, `timeout_ms` | 走 argv **不过 shell**；要 shell 展开得显式 `exec("bash",["-c","..."])`，且受 deny-exec 限制 |
| `read_file` | `path` | `offset`, `length` | 单次最多 **1 MiB**；用 `offset` 翻页直到 `eof=true` |
| `write_file` | `path`, `content` | `append` | `content` 为 **base64** 编码；`append=true` 追加 |
| `list_dir` | `path` | `recursive`, `glob` | 列目录 |
| `stat` | `path` | - | 查路径元信息（类型/大小/mtime/mode） |
| `glob` | `pattern` | `root` | 按 glob 找文件 |
| `grep` | `pattern` | `root`, `glob`, `ignore_case` | 远端正则搜索 |
| `process_list` | - | `filter` | 远端进程清单 |
| `tail_log` | `path` | `lines` | 读日志末尾 N 行（默认 **100**） |

**沙箱默认拒绝**（由远端 `share` 的参数控制，见下）：
- 远端传了 `--root` 且路径超出其子树 → `path_outside_root`（默认不传则无此限制）
- `exec` 的 `argv[0]` 命中 `--deny-exec`（默认 `rm,shutdown,reboot,mkfs,dd`），或设了 `--allow-exec` 白名单却不在其中 → `exec_denied`

---

## 5. 远端 `share` 侧的相关开关（决定工具能做什么）

| 参数 | 默认 | 作用 |
|---|---|---|
| `--root <dir>` | 空 = 不限制 | 可选：把文件工具限制在该子树，越界拒绝。不约束 exec，只防手滑 |
| `--allow-exec a,b` | 空 | exec 命令白名单（basename） |
| `--deny-exec ...` | `rm,shutdown,reboot,mkfs,dd` | exec 命令黑名单 |
| `--standalone` | false | 内嵌 relay，局域网零服务器（自动 `--plain`，工具通道仍 AEAD 加密） |
| `--elevate` | false | Windows 启动时 UAC 提权 |
| `--unsafe-exec` | false | **关闭 exec 黑/白名单**，启动强制 5 秒红色倒计时确认；不影响 `--root` |

---

## 6. 错误码（统一返回给 Claude）

```
path_outside_root   exec_denied        timeout         tunnel_lost
file_not_found      permission_denied  cancelled       remote_panic
deadline_exceeded   too_large          session_expired version_mismatch
not_connected       （未先调用 connect 时其他工具返回）
```

---

## 7. 排错速查

| 现象 | 多半原因 / 处理 |
|---|---|
| 工具都报 `not_connected` | 还没调 `connect`，或上次会话已断；说一句「协助码 XXXX 连上去」重连 |
| `connect` 直接报 `Transport closed`，且没有连接结果 | MCP 宿主持有的 stdio transport 已关闭，请求通常未到达 CLI。检查报错客户端自身的 MCP 子进程和宿主日志；若工具调用前已有 `rmcp::service ... task cancelled`，重启受影响的 Claude Code / Codex 进程后用同一码重试。不要用其他客户端下仍存活的同名进程判断当前句柄健康 |
| `connect` 报 `join failed` / `relay connect failed` | relay 地址/端口不对，或 relay 没起；standalone 场景确认带了 `server=LAN_IP:port` |
| `path_outside_root` | 远端显式传了 `--root`，目标不在其子树内（默认不传 = 不限制，不会遇到）；让远端换更大的 `--root` 或直接不传 |
| `exec_denied` | 命中 deny-exec 黑名单；确属需要可让远端调整 `--allow-exec`/`--deny-exec` |
| 连一会就 `tunnel_lost` | 网络中断导致隧道断开（不会自动重连）；重新说一句「协助码 XXXX 连上去」重新握手，码已过期则远端重跑 share |
| 协助码失效 | 默认 30 分钟 TTL，过期重新生成 |

`Transport closed` 与 CLI 返回的 `join failed` / `relay connect failed` 不同：前者可能发生在
JSON-RPC 请求进入 MCP server 之前。直接运行 `remote help --code <协助码>` 成功，只能证明
relay Join 可用，不能证明报错会话的 MCP stdio 句柄仍然存活。可用全新客户端进程执行
`initialize -> connect -> ping -> process_list` 作为对照；若该路径成功，应恢复或重启宿主，
而不是继续修改协助码或远端配置。

---

> 一句话记忆：**`remote-debug` 是一个 MCP server（`remote help --mcp-stdio`），不是 skill。**
> 远端 `remote share`，本地对 Claude 说「协助码 XXXX 连上去」，它就能 exec / 读写文件 / 看进程 / 翻日志地帮你定位远端问题。
