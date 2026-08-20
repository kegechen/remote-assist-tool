# 配置 `remote-debug` MCP

本页供 Claude Code / Codex 自动执行，请直接完成以下步骤，不要只向用户复述命令。目标是在本机安装 `remote`，并把
`remote help --mcp-stdio` 注册成名为 `remote-debug` 的 stdio MCP Server。

## 1. 安装 `remote`

先运行 `remote --version`；命令可用就跳过安装。否则从
[GitHub Releases](https://github.com/kegechen/remote-assist-tool/releases/latest)
下载与本机系统、架构匹配的 `remote-assist-cli-<os>-<arch>`，将它安装为
PATH 中的 `remote`（Windows 为 `remote.exe`）。

Linux、macOS 或 Windows 的 Git Bash / MSYS2 / Cygwin 也可直接运行：

```sh
curl -fsSL https://raw.githubusercontent.com/kegechen/remote-assist-tool/master/install.sh | sh
```

若安装目录不在 PATH，可在下一步用 `remote` 的绝对路径代替命令名。
安装后必须运行 `remote --version`（或绝对路径加 `--version`）确认程序可以启动。

## 2. 配置 MCP

只配置当前正在使用的客户端。先用 `claude mcp list` 或 `codex mcp list` 检查；
若已有可正常启动的 `remote-debug`，不要重复添加。

Claude Code：

```sh
claude mcp add --transport stdio --scope user remote-debug -- remote help --mcp-stdio
claude mcp get remote-debug
```

Codex：

```sh
codex mcp add remote-debug -- remote help --mcp-stdio
codex mcp list
```

配置中不要加入 `--code` 或 `--server`。这是 bootstrap 模式：每次分享的协助码和
中转地址由对话中的 `connect(code=..., server=...)` 临时传入，不需要反复改配置。

## 3. 生效并连接

重启 Claude Code / Codex 会话，使新 MCP 配置生效，然后重新粘贴 `remote share`
输出的整段协助信息。调用 `remote-debug` 的 `connect` 成功，并确认返回的
`peer_host` 与分享信息中的“本机标识”一致，即完成连接。

默认配置使用 TLS 并跳过证书校验，适配工具内置的自签名 relay。若分享信息明确显示
`(明文)`，需要在 MCP 命令末尾临时增加 `--plain` 后重启客户端。

## 4. 排查 `Transport closed`

若 `connect` 只返回以下错误，且没有 `session_id`、`peer_host` 或其他连接结果：

```text
tool call failed for `remote-debug/connect`
Caused by:
    Transport closed
```

这表示 MCP 客户端持有的 stdio transport 已关闭，请求通常还没有到达 `remote` CLI；
它本身不能证明协助码无效、relay 异常或 ToolHello 握手失败。尤其在 review、子代理或临时
任务结束后，宿主可能已经取消对应 MCP 子进程，但旧工具句柄仍留在原会话中。

按以下顺序验证：

1. 检查**报错的客户端进程自身**是否仍有 `remote help --mcp-stdio` 子进程；其他 Codex /
   Claude 进程下的同名子进程不能证明当前句柄可用。
2. 检查宿主日志中是否先出现 `rmcp::service ... task cancelled`，之后工具调用才报
   `Transport closed`。若顺序如此，属于宿主侧 stale transport。
3. 重启受影响的 Claude Code / Codex 进程或新开客户端进程，再用同一二进制和同一协助码
   调用 `connect`。新进程成功即可排除 CLI、relay 和协助码。
4. 不要在已关闭的旧句柄上反复调用 `connect`；stdio 管道关闭后只能由宿主重新启动 MCP
   server。若新进程仍失败，再用独立 stdio 测试执行
   `initialize -> connect -> ping -> process_list`，继续定位 CLI 或网络链路。

直接运行 `remote help --code <协助码>` 只能验证 relay Join 和传统 help 路径，不能覆盖
MCP initialize、健康检查及宿主 stdio 生命周期。
