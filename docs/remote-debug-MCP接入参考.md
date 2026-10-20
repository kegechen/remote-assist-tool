# `remote-debug` MCP 接入参考

`remote-debug` 是一个 **stdio MCP Server**（不是 skill / plugin），由 `remote-assist-cli help --mcp-stdio`
提供。任何支持 MCP 的 AI agent（Claude Code、Codex、Cursor 等）接入后，就能在远端机器上
执行命令、读写文件、看进程、翻日志。

```
AI agent ──MCP stdio──▶ remote-assist-cli help --mcp-stdio ──TLS / P2P──▶ remote-assist-cli share（远端）
```

---

## 1. 安装

从 [GitHub Releases](https://github.com/kegechen/remote-assist-tool/releases/latest) 下载对应平台的客户端：

```
remote-assist-cli-<os>-<arch>[.exe]
                  │     └─ amd64 | arm64
                  └─ windows | linux | darwin
```

- `remote-assist-cli`：客户端，**两端用同一个文件**——远端跑 `share`，本地跑 `help`。
- 同一 Release 里的 `remote-assist-relay-*` 是中转服务器，只有自建 relay 才需要；`remote-assist-webui-*` 是浏览器控制台，它在后台拉起 `remote-assist-cli help --mcp-stdio`，用同一套 MCP 工具，只是改由人在网页上操作，接入 AI agent 不需要它。

也可以用一键脚本（Linux、macOS，或 Windows 的 Git Bash / MSYS2 / Cygwin）：

```bash
curl -fsSL https://raw.githubusercontent.com/kegechen/remote-assist-tool/master/install.sh | sh
```

脚本会自动识别系统和架构，下载对应文件并校验 `SHA256SUMS`，然后：

- 改名为 `remote`（Windows 为 `remote.exe`），Linux / macOS 自动加可执行权限；
- 默认装到 `~/.local/bin`（可用环境变量 `REMOTE_INSTALL_DIR` 指定目录）；**不会自动加进 PATH**，没加的话 MCP 配置里请写绝对路径。

---

## 2. 配置 MCP Server

最简配置只需要两个参数，**不要写 `--code`、`--server`**——协助码和 relay 地址都在每次会话里通过 `connect` 传入：

### Claude Code（项目根 `.mcp.json` 或 `~/.claude.json`）

```json
{
  "mcpServers": {
    "remote-debug": {
      "command": "D:\\tools\\remote-assist-cli-windows-amd64.exe",
      "args": ["help", "--mcp-stdio"]
    }
  }
}
```

或一条命令：`claude mcp add --scope user remote-debug -- <二进制路径> help --mcp-stdio`

### Codex（`~/.codex/config.toml`）

```toml
[mcp_servers.remote-debug]
command = "/usr/local/bin/remote-assist-cli-linux-amd64"
args = ["help", "--mcp-stdio"]
```

或一条命令：`codex mcp add remote-debug -- <二进制路径> help --mcp-stdio`

### 其他客户端

Cursor、Trae、Cline 等大多沿用与 Claude Code 相同的 `mcpServers` JSON 结构，只是文件位置不同；
核心永远是：**command = 二进制路径，args = `["help", "--mcp-stdio"]`**。

> 用 `install.sh` 装过且安装目录已在 PATH 中，`command` 直接写 `remote` 即可。改完配置需重启客户端（或其 MCP 连接）生效。

---

## 3. 连接流程

1. **远端**直接运行（或双击）`remote-assist-cli`，得到协助码（如 `ABCD-EFGHIJ`，默认 30 分钟有效），整段协助信息已复制到剪贴板。
   - 不带任何参数时默认就是 `share`，连默认公共 relay；
   - 要带参数（如 `--server`、`--standalone`、`--root`）就必须写出子命令：`remote-assist-cli share --server relay.example.com:8443`；
   - 想双击也连自建 relay，可设置环境变量 `REMOTE_RELAY_SERVER=host:8443`；
   - 同一台机器默认只能运行一个 share，再启动会提示「已有 share 正在运行」；确实要再开一个，用 `remote-assist-cli share --new-instance`，它会生成一个独立的新协助码。
2. **本地**把这段信息粘贴给 AI agent，或者说「协助码 ABCD-EFGHIJ，连上去」。
3. agent 调用 `connect`，成功后即可使用其余工具。确认返回的 `peer_host` 与分享信息中的「本机标识」一致。
4. 码过期、换机器、隧道断开：远端重跑 `share`，把新码再说一次即可，**不用重启 agent**。

`connect` 的几种写法：

| 场景 | 调用 |
|---|---|
| 默认公共 relay | `connect(code="ABCD-EFGHIJ")` |
| 自建 relay / LAN standalone | `connect(code="ABCD-EFGHIJ", server="192.168.1.50:8443")`（只写 `host:port`，不能带 `http://`） |
| 对端是 0.0.x 旧版且用户同意降级 | 加 `min_proto="1"`（关闭防重放等保护，仅限可信网络） |

---

## 4. 网络模式与安全取舍

| 模式 | 怎么用 | 适用场景 | 注意 |
|---|---|---|---|
| **默认公共 relay** | 什么都不配 | 临时试用、非敏感环境 | 地址 `23.95.78.14:8443`，部署在**海外**，部分网络会慢或连不上。relay 是可信边界：它生成协助码、能推导出工具通道的会话密钥 |
| **自建 relay** | 部署 `remote-assist-relay`，`connect(server="host:8443")` | 公司内网、敏感环境（推荐） | 信任边界在自己手里。用正式 CA 证书时，可在 args 加 `--insecure=false` 或 `--ca <文件>` 开启证书校验 |
| **LAN standalone** | 远端 `share --standalone`，`connect(server="远端IP:8443")` | 两端在同一局域网 | 不经任何第三方；share 进程内嵌 relay，使用自签证书，**必须保持默认的跳过校验**（证书只覆盖 localhost） |

几个开关的判断标准：

- **`--insecure`（默认 true）**：跳过证书的 CA 校验，是为了让内置 / standalone 的自签证书开箱即用。首次连接会记录证书指纹，之后指纹变化会被拒绝（relay 正当换证书时加 `--trust-new-cert`）。工具内容另有 AEAD 加密，所以默认值对公共 relay 和 standalone 都可接受；自建且有正式证书时建议关掉。
- **`--plain`**：relay 以明文模式运行时才需要。协助码会明文过网，路径上的旁观者拿到它就能解开工具通道——**只用于开发调试或物理隔离网络**。
- **`--no-auth`**：不需要协助码，网络里任何能连到 relay 的设备都能控制远端——**只用于完全可信的私网**。

---

## 5. 工具清单

> `exec` 不支持流式（执行完一次性返回）；`tail_log` 不支持 follow（只读末尾 N 行）。

| 工具 | 必填 | 常用可选 | 说明 |
|---|---|---|---|
| `connect` | `code` | `server`, `no_auth`, `min_proto` | 配对，必须**最先调用** |
| `exec` | `argv` | `cwd`, `timeout_ms`, `max_output_bytes` | **不经过 shell**：管道、重定向、`cd`/`dir` 等内建命令要显式包一层 `["sh","-c","..."]` 或 `["powershell","-NoProfile","-Command","..."]`；输出默认每流 32 KiB，保留首尾 |
| `read_file` | `path` | `offset`, `length`, `as_text` | 默认 64 KiB/次，最大 1 MiB；`offset += bytes_len` 循环到 `eof=true` |
| `write_file` | `path`, `content` | `create`, `append` | 写小段内容（base64）；新建文件需 `create=true`，传本地文件用 `upload_file` |
| `upload_file` | `local_path`, `remote_path` | `offset` | 本地 → 远端，512 KiB 分块，自动重试，可断点续传 |
| `download_file` | `remote_path`, `local_path` | `offset` | 远端 → 本地，适合 dump、日志包 |
| `list_dir` | `path` | `recursive`, `glob`, `max_entries` | 默认最多 200 项 |
| `stat` | `path` | - | 类型 / 大小 / mtime / mode |
| `glob` | `pattern` | `root` | 按通配符找文件 |
| `grep` | `pattern` | `root`, `glob`, `ignore_case`, `max_matches` | 正则搜索，默认最多 1000 条 |
| `process_list` | - | `filter`, `max_count` | 默认最多 50 个 |
| `tail_log` | `path` | `lines` | 默认 100 行，上限 64 KiB（保留最新） |

**远端 `share` 的护栏**（决定上述工具能做什么）：

| 参数 | 默认 | 作用 |
|---|---|---|
| `--deny-exec` | `rm,shutdown,reboot,mkfs,dd` | exec 黑名单（按 basename） |
| `--allow-exec` | 空 | exec 白名单，设置后只允许名单内命令 |
| `--root <dir>` | 空 = 不限制 | 把文件工具限制在该子树内；防手滑，不约束 exec，不是安全边界 |
| `--unsafe-exec` | false | 关闭黑/白名单，启动时有 5 秒确认倒计时 |
| `--elevate` | false | Windows 下以 UAC 提权启动 |

---

## 6. 错误码与排错

| 错误码 / 现象 | 含义与处理 |
|---|---|
| `not_connected` | 还没调 `connect`，或上次会话已断开；重新提供协助码 |
| `tunnel_lost` | 网络中断，隧道不会自动重连；重新 `connect`，码已过期就让远端重跑 `share` |
| `exec_denied` | 命中黑名单或不在白名单；让远端调整 `--allow-exec` / `--deny-exec` |
| `path_outside_root` | 远端设了 `--root` 且目标在子树外 |
| `file_not_found` / `permission_denied` | 远端路径不存在 / 无权限 |
| `deadline_exceeded` / `cancelled` | 工具执行超时 / 被取消；`exec` 可调大 `timeout_ms` |
| `server_busy` | 远端 share 同时收到的请求太多，稍后重试即可 |
| `unknown_tool` | 远端 share 版本较旧，没有这个工具；升级远端的 `remote-assist-cli` |
| `unauthenticated` / `decrypt_failed` / `replayed` | 工具通道的加密校验失败（会话密钥不一致、请求被篡改或重放）；重新 `connect`，仍失败就让远端重跑 `share` |
| `remote_panic` / `internal_error` | 远端工具异常，带上错误信息反馈 |
| `join failed` / `relay connect failed` | relay 地址或端口不对、relay 未运行；standalone 场景确认传了 `server=LAN_IP:8443` |
| `connect` 只报 `Transport closed`，无任何连接结果 | 请求没到达 CLI，是 MCP 客户端的 stdio 连接已失效，与协助码和 relay 无关。**重启 AI agent 客户端**后用同一码重试；详细排查见 [`MCP_SETUP.md`](../MCP_SETUP.md#4-排查-transport-closed) |

---

## 7. 局域网直连（不经任何 relay）

两台机器在同一局域网、连不上公共 relay 或不希望流量出网时使用：

1. **远端**运行：

   ```bash
   remote-assist-cli share --standalone
   ```

   它会在本机启动内嵌 relay（默认监听 `:8443`，可用 `--standalone-listen :9443` 换端口），并打印协助码和本机局域网地址，例如 `192.168.1.50:8443`。远端防火墙需放行该 TCP 端口。

2. **本地**对 AI agent 说「协助码 ABCD-EFGHIJ 在 192.168.1.50:8443」，agent 会调用：

   ```
   connect(code="ABCD-EFGHIJ", server="192.168.1.50:8443")
   ```

MCP 配置不用改，仍是 `["help", "--mcp-stdio"]`。内嵌 relay 使用自签证书，保持默认设置即可，不要加 `--insecure=false`。

---

更完整的产品介绍与使用场景见 [`远程调试工具指南.md`](远程调试工具指南.md)。
