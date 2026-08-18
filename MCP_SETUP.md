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
