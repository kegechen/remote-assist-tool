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

终端页默认就是真终端（xterm.js）；只有真终端确实不可用时才回退到 exec 命令行模式，且界面会明确写出原因（日志首行红字 + 底部状态栏），不再笼统说“无 PTY”。真终端模式下不显示任何 exec 模式的提示或状态栏。

| 场景 | 被控端返回 | 界面提示 |
|------|-----------|---------|
| 对端是不含 pty 工具的旧版 | `unknown_tool` | 对端版本过旧，请升级被控端到最新版后重新连接 |
| 被控端设了 `--allow-exec` 白名单（未加 `--allow-pty`） | `exec_denied` | 被控端设置了命令白名单，已禁用真终端；如需真终端请被控端加 `--allow-pty` |
| Windows 版本过低，无 ConPTY | `pty_unsupported` | 被控端系统不支持 ConPTY（需 Windows 10 1809+） |
| 找不到 / 启动不了 shell | 对应错误 | 被控端无法启动 shell（附原始错误） |

- 设了 `--allow-exec` 白名单时 PTY 默认整体禁用：PTY 里的 shell 能执行任意命令，会绕过白名单。被控端用户可显式加 `--allow-pty` 开放真终端（默认仍禁用；开放即表示白名单不再约束协助端）。
- 回退到命令行模式后，重新连接会重新探测。

## 限制

- Windows 被控端需要 Windows 10 1809 及以上（ConPTY）；更老系统会回退到命令行模式并提示原因。
- 首次输出有一点延迟：要等被控端 shell 启动（PowerShell 带大量历史/预测时可能 1 秒左右）。
- 带宽：每个按键是一次 HTTP 往返 + 一次隧道工具调用（输入在浏览器端按 6ms 合并批量发送）；输出按块推送，海量输出（如 `cat` 大文件）受 relay 限流影响，若隧道丢帧，会话结束时会报 `stream_incomplete`。
- 单个被控端最多同时 16 个 PTY 会话；会话最长 24 小时。
- 空闲回收：会话“无输入且无输出”超过 2 小时（被控端环境变量 `REMOTE_ASSIST_PTY_IDLE` 可覆盖，如 `30m`、`6h`，`0`/`off` 关闭）后由被控端关闭，前端提示“终端因长时间空闲已关闭”。输入（pty_write/pty_resize）与输出都算活动，所以 `tail -f`、编译等持续有输出的长任务不会被误杀；24 小时绝对上限不变。
- 输入内容不会写入任何日志。
