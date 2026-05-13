# Claude Code 远程调试通道设计

- **日期**：2026-05-13
- **状态**：Draft（待评审）
- **作者**：kechen12
- **范围**：在现有 remote-assist-tool（relay + P2P + 协助码）之上，新增"Claude Code 远程调试通道"，让本地 Claude Code 通过本地 `remote help` 进程对远端 `remote share` 进行结构化操作（exec / 文件 / 进程 / 日志），目标替代 SSH 在远程调试场景下的角色，尤其消除 Windows 启用 openssh-server 的负担。

## 1. 目标与约束

### 1.1 用户故事

开发者本地跑 Claude Code，想让 Claude 协助调试一台远端机器（可能是 Windows、Linux，可能在 NAT 后）。期望：

1. 远端执行一条命令 `remote share`，本地执行 `remote help`，二者通过协助码配对。
2. Claude Code 立即获得一套作用于远端的工具（exec / read_file / write_file / list_dir / grep / tail_log / ...），无需 SSH，无需在远端开启 openssh-server。
3. 工具可分发——任何人拿到二进制就能用自己的 Claude 调试自己的远端，**不绑定任何账号或身份提供方**。

### 1.2 设计约束

- 复用现有传输层（TLS 1.3 + 协助码 + relay + P2P 打洞 + relay 回落）。
- 不引入身份服务、不引入额外端口监听。
- 远端默认不需要管理员权限；Windows 提权是显式可选项。
- 远端默认沙箱（路径 root 限制 + 无 shell 展开的 exec）。
- 向后兼容现有 SSH 转发模式（旧用户不受影响）。

### 1.3 非目标（MVP 不做）

- ❌ 多 help 并发连同一 share。
- ❌ 远端 daemon 加载用户自定义 MCP 插件。
- ❌ 按命令粒度的 UAC 提权。
- ❌ 浏览器版 Claude 直连。
- ❌ 通用文件 watch / 通用端口转发（保留 `--legacy-ssh` 顶替端口转发场景）。

## 2. 架构与组件

```
                                  Relay (公网，TLS 1.3)
                                       │
                ┌──────────────────────┴──────────────────────┐
                │                                             │
   ┌────────────▼────────────┐                  ┌────────────▼────────────┐
   │ remote share (远端)     │                  │ remote help --mcp-stdio │
   │ ── 协助码 ABCD-EFGH     │                  │   (本地 MCP server)     │
   │ ── --root D:\src\proj   │                  │                          │
   │ ── 工具 daemon (新)     │ ◀── P2P 优先 ──▶ │ ── MCP 适配层 (新)      │
   │   - exec/fs/proc/log    │     回落 Relay   │ ── 隧道客户端 (现成)     │
   │ ── 审计 + 实时活动显示  │                  │                          │
   └─────────────────────────┘                  └─────┬───────────────────┘
                                                     │ MCP over stdio
                                                     ▼
                                            ┌──────────────────┐
                                            │   Claude Code    │
                                            └──────────────────┘
```

### 2.1 代码变更点

| 位置 | 类型 | 内容 |
|---|---|---|
| `internal/agent/` | 新建 | share 端工具 daemon，实现 9 个工具 |
| `internal/proto/` | 修改 | 新增 `MsgToolReq` / `MsgToolResp` / `MsgStreamChunk` / `MsgCancel` |
| `internal/mcp/` | 新建 | help 端 MCP server 适配层（MCP JSON-RPC ↔ ToolReq） |
| `cmd/remote` | 修改 | `share` 加 `--root` `--allow-exec` `--deny-exec` `--elevate` `--unsafe-full-system`；`help` 加 `--mcp-stdio` `--mcp-port` `--legacy-ssh` |
| `client/share.go` / `client/help.go` | 修改 | 在现有 SSH 转发旁加工具通道分发 |

### 2.2 不动的部分

`cmd/relay`、协助码生成与 TTL、TLS 配置、P2P 打洞与回落策略、审计日志框架。

## 3. 协议设计

### 3.1 握手（在协助码验证之后追加）

```
share→help:  HELLO { version, capabilities[], session_nonce_share }
help→share:  HELLO_ACK { version, session_nonce_help }
两端各自:    session_key = HKDF(协助码 || nonce_share || nonce_help, "rat-tool-v1")
```

握手失败（版本不兼容、capabilities 不匹配）→ 关闭工具通道，但保留 SSH 兼容通道。

### 3.2 工具帧（多路复用在现有 TLS 隧道之上）

```
ToolReq    { id: u64, tool: string, args: cbor, deadline_ms: u32 }
ToolResp   { id: u64, ok: bool, result: cbor, error?: string }
StreamChunk{ id: u64, seq: u32, fin: bool, data: bytes }
Cancel     { id: u64, reason?: string }
```

- 外层用 `session_key` 做 XChaCha20-Poly1305 AEAD；relay 看不到工具内容、也无法伪造请求。
- `id` 由 help 端单调递增；share 端的 ToolResp / StreamChunk 必须回带原 id。
- 流式工具（`exec stream=true`、`tail_log follow=true`、大文件）通过多个 StreamChunk 推送，最后一个 `fin=true`；最终 ToolResp 给汇总结果（如 exit_code）。
- 反压：依赖底层 TCP 自带回压；如有必要 v2 加显式 `Ack` 窗口。

### 3.3 工具集 v1

| 工具 | 入参 | 出参 | 备注 |
|---|---|---|---|
| `exec` | argv:string[], cwd?, env?, timeout_ms?, stream? | exit_code, stdout, stderr (或流) | argv 列表，**不过 shell** |
| `read_file` | path, offset?, length? | bytes, eof | 大文件分块 |
| `write_file` | path, content, mode?, create? | bytes_written | 受 root 限制 |
| `list_dir` | path, recursive?, glob? | entries[name, kind, size, mtime] | |
| `stat` | path | kind, size, mtime, mode | |
| `glob` | pattern, root? | paths[] | |
| `grep` | pattern, paths/glob, flags | matches[file, line, text] | 远端文件系统执行 |
| `process_list` | filter? | proc[pid, name, cmdline, user] | |
| `tail_log` | path, lines?, follow? | StreamChunk | follow 用 fsnotify |

**默认拒绝**：
- 路径不在 `--root` 子树 → `path_outside_root`。
- exec 的 `argv[0]` 命中 `--deny-exec`，或设了 `--allow-exec` 白名单但不在其中 → `exec_denied`。

### 3.4 MCP 适配（help 端）

- `remote help --mcp-stdio` 在 stdio 上跑 MCP JSON-RPC（`initialize` / `tools/list` / `tools/call` / `notifications/cancelled`）。
- `tools/list` 返回固定 schema（与 share 端 daemon 同源生成）。
- `tools/call` → 翻译成 ToolReq 发隧道 → 等 ToolResp → 翻译回 JSON-RPC result。
- 流式工具用 MCP 的 progress notification 推送 chunk。
- 用户 Claude Code 接入示例：

```jsonc
// .mcp.json
{
  "mcpServers": {
    "remote-debug": {
      "command": "remote",
      "args": ["help", "--server", "relay.example.com:8443",
               "--code", "${REMOTE_CODE}", "--mcp-stdio"]
    }
  }
}
```

## 4. 安全模型

### 4.1 威胁与对策

| 威胁 | 对策 |
|---|---|
| 协助码泄露 | 30 分钟 TTL + 单次配对 + 显示对端指纹（IP + 客户端版本 hash）供人眼核对 |
| Relay 被劫持 / 中间人 | session_key 派生自协助码 + 双方 nonce，AEAD 保护工具内容；relay 看不到、改不了 |
| help 端被入侵 | share 端实时显示所有调用 + 审计日志 + `--root`/`--allow-exec` 沙箱兜底 |
| 恶意 share 配置分发 | 默认沙箱开；`--unsafe-full-system` / `--elevate` 启动时红色横幅 5 秒倒计时强制确认 |

### 4.2 沙箱实现

- **路径校验**：`filepath.Clean` + `filepath.Rel(root, path)` 检查无 `..`；对 symlink 先 `EvalSymlinks` 再校验（防 symlink 越狱）。
- **exec**：`os/exec.Command(argv[0], argv[1:]...)` 直走 argv，**永不**经 `cmd /c` 或 `bash -c`；用户若要 shell 展开，需显式 `exec("bash", ["-c", "..."])`，由 deny-exec 拦截。
- **超时**：每个 ToolReq 必带 deadline；exec 默认 5 分钟；`tail_log follow=true` 无超时但 help 断开即终止。
- v1 不做硬资源限制（CPU/内存配额），仅依靠超时。

### 4.3 Windows 提权

- `--elevate`：share 启动时通过 `ShellExecuteW` + `runas` 触发一次 UAC，被批准后 daemon 子进程全程提升权限。
- 不提权时仍能用：daemon 跑在当前用户上下文，能操作用户目录、跑 `tasklist`、读大部分日志。
- 不支持按需提权（每条命令弹 UAC 不现实）。
- share 终端显示当前权限级别（"running as: kechen12 (non-elevated)" / "(ELEVATED)"）。

### 4.4 审计

- 默认 `~/.remote-assist/audit.log`，复用现有 `logger/audit.go` 框架。
- 每条 ToolReq 一行：`ts | session_id | tool | args_summary | duration_ms | ok|err`。
- 大文件 read/write 只记录 path + bytes，不记录内容。

## 5. 生命周期与错误处理

### 5.1 Share 端

```
启动 → 解析 flags → 注册 relay 拿协助码 → 打印码 + 沙箱配置摘要
  → 等 help 接入 → 握手 OK → 启动工具 daemon 接收 ToolReq
  → 持续显示活动 + 写审计 → help 断开 → daemon 关闭未完成流
  → 等下一个 help（v2 多会话）或退出
```

- `Ctrl+C`：发 `Goodbye` 帧给 help 后退出。

### 5.2 Help 端 / MCP 适配

- Claude Code 拉起 `remote help --mcp-stdio` → 初始化时和 share 完成握手；握手失败用 JSON-RPC 错误回执后退出。
- 隧道断开：先尝试 P2P→relay 自动重连（复用现有逻辑），重连窗口内 ToolReq 排队；超过 30 秒未恢复，所有 in-flight 返回 `tunnel_lost`，MCP server 退出。
- Claude Code 关闭 → stdin EOF → 优雅退出 → 给 share 发 Goodbye。

### 5.3 错误码（统一返回给 Claude）

```
path_outside_root   exec_denied        timeout         tunnel_lost
file_not_found      permission_denied  cancelled       remote_panic
deadline_exceeded   too_large          session_expired version_mismatch
```

- `remote_panic`：share 侧某工具实现挂了；捕获 goroutine panic 不污染整个 daemon，返回 stack 摘要。

### 5.4 取消语义

- Claude 调用 MCP `notifications/cancelled` → MCP 适配层发 `Cancel{id}` 给 share。
- share 端：
  - exec：给子进程发 SIGTERM，5 秒后 SIGKILL（Windows 用 `TerminateProcess`）。
  - 流式工具：立即关闭 chunk 输出，返回 `cancelled` ToolResp。
- 已完成的请求收到 Cancel：忽略，不报错。

## 6. 测试策略

### 6.1 单元测试

- `internal/proto`：4 个新消息类型 marshal/unmarshal roundtrip + AEAD 加解密。
- `internal/agent`：每个工具一组——正常路径、超出 root、超时、deny-exec、不存在文件、并发取消。
- `internal/mcp`：MCP JSON-RPC ↔ ToolReq 双向翻译；流式 progress notification 顺序与 fin 语义。

### 6.2 集成测试（扩展现有 `tests/`）

- 端到端：本地 relay + share + help-mcp-stdio，从 stdin 喂 MCP 调用，断言 stdout 输出。
- 故障注入：relay 关、share 进程 kill、help stdin EOF——验证错误码。
- Linux + Windows 各跑一遍（GitHub Actions 现有矩阵）。

### 6.3 手工验收清单

1. share 端跑 `remote share --root .`，把协助码塞 Claude Code 的 `.mcp.json` 环境变量。
2. Claude 里说"读 README 第 1 行" → 触发 `remote-debug:read_file` 并返回正确内容。
3. Claude 里说"在远端跑 `go vet ./...`" → 触发 `remote-debug:exec` 流式返回。
4. share 终端能看到这两条活动行。
5. 把 share 端 `--root` 改成空目录后重启，让 Claude 读项目文件应失败 `path_outside_root`。
6. `--legacy-ssh` 模式下 `ssh -p 2222` 仍然可用（向后兼容回归）。

### 6.4 性能基线（参考，非硬指标）

- exec 小命令 e2e：P2P 直连 < 50ms；Relay < 200ms。
- read_file 1MB：P2P < 500ms。
- 串行 100 个 `stat`：< 10s（验证握手摊销 + 多路复用无明显瓶颈）。

## 7. 开放问题 / 风险

1. **CBOR 还是 protobuf 作为 args/result 编码**？项目已有 proto/message.go，需要看现有编码风格再定，倾向沿用既有方案，避免新增依赖。
2. **MCP 协议版本**：Claude Code 当前支持的 MCP 版本可能演进，需在 `tools/list` 实现时锁定一个 SDK 版本。
3. **Windows fsnotify**：`tail_log follow=true` 在 Windows 上的 fsnotify 兼容性需在实现期实测。
4. **审计日志位置**：`~/.remote-assist/audit.log` 在多用户 Windows 上的具体路径需要确认；用 `os.UserConfigDir()` 比较安全。

## 8. 下一步

- 用户评审本设计文档。
- 通过后由 `superpowers:writing-plans` 生成实施计划（拆任务、时序、关键里程碑）。
