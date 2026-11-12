# webui 真终端（PTY）

webui 的「终端」页是一个真正的交互式终端（xterm.js + 被控端 PTY），不再是“一次性 exec 命令行”。

## 用法

- 连接成功后，终端页自动打开远端的默认 shell 并聚焦，直接敲键盘即可。
- 支持 vim / top / less / htop 等全屏程序、方向键历史、Tab 补全、颜色、中文、Ctrl+C 中断、Ctrl+D 退出。
- Ctrl+C 默认就是发给远端的真实中断；只有在终端里选中了文字时，Ctrl+C 才用作复制。
  Ctrl+Shift+C / Ctrl+Shift+V 始终是复制 / 粘贴。
- 浏览器窗口或面板大小变化会同步到远端 PTY 的行列数。
- shell 退出（exit / Ctrl+D）后会提示“进程已退出，按回车重新开始”，回车即可开新会话。
- 默认 shell：Windows 依次找 pwsh、powershell.exe、cmd.exe；Unix 用被控端用户的 `$SHELL`，否则 bash / sh（登录方式）。
- 关闭页面 / 断开连接 / 会话结束时，被控端会杀掉该会话的整棵进程树（Windows 用 Job Object，Unix 用 setsid + 进程组/会话清理），不留孤儿进程。
- 主题色读取 CSS 变量 `--term-bg`、`--term-fg`、`--term-cursor`、`--term-selection`，缺省为深色。

## 协议（供二次开发 / MCP）

新增 4 个工具（能力集 capabilities 已包含）：

| 工具 | 类型 | 说明 |
| --- | --- | --- |
| `pty_open` | 流式长调用 | 参数 `session`（调用方指定 id）、`shell`、`cwd`、`cols`、`rows`、`env`、`stream=true`；输出以 StreamChunk 字节原样推送，会话结束时返回 `{exit_code, reason}` |
| `pty_write` | 快工具 | `session`、`data`（base64 字节，Ctrl+C 即 0x03），单次最多 256 KiB |
| `pty_resize` | 快工具 | `session`、`cols`、`rows`（1~1000） |
| `pty_close` | 快工具 | 幂等；杀进程树 |

GUI 后端对应 `/api/pty/open`（NDJSON 流）、`/api/pty/write`、`/api/pty/resize`、`/api/pty/close`，沿用 token / Host / Origin 防护。
xterm.js 5.5.0 与 addon-fit 0.10.0 离线内嵌（`internal/gui/assets/xterm/`，MIT，许可证文件随包），由 `/assets/` 本地提供，不依赖外网。

## 回退与禁用

- 对端是不含 pty 工具的旧版（返回 `unknown_tool`），或对端系统不支持 ConPTY，终端页自动退回原来的 exec 命令行模式（原代码路径保留）。
- 被控端设置了 `--allow-exec` 白名单时 PTY 整体禁用（返回 `exec_denied`，GUI 退回 exec 模式，由白名单约束），避免经 shell 绕过白名单。

## 限制

- Windows 被控端需要 Windows 10 1809 及以上（ConPTY）；更老系统会自动回退到命令行模式。
- 首次输出有一点延迟：要等被控端 shell 启动（PowerShell 带大量历史/预测时可能 1 秒左右）。
- 带宽：每个按键是一次 HTTP 往返 + 一次隧道工具调用（输入在浏览器端按 6ms 合并批量发送）；输出按块推送，海量输出（如 `cat` 大文件）受 relay 限流影响，若隧道丢帧，会话结束时会报 `stream_incomplete`。
- 单个被控端最多同时 16 个 PTY 会话；会话最长 24 小时。
- 输入内容不会写入任何日志。
