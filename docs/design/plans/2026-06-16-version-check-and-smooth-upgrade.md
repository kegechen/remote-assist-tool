# 版本检测与平滑升级 设计

> 状态：草案（2026-06-16）。决策大部分已定，R1 分发源（3/4）待最终确认。

## 目标

给 remote-assist 加三层能力（由浅入深）：
1. **R0** 规范的语义版本号（替换 `dev (commit:xxx)`）。
2. **R2** 连接时检测客户端版本是否最新，过期主动提示。
3. **R3** 平滑升级：客户端自动拉新版本 + 自动重连，不丢会话（秒级闪断）。

## 已有基础（决定改造量，均经源码查证）

| 能力 | 位置 | 现状 |
|---|---|---|
| 版本注入 | `internal/version/version.go` | `var Version="dev"`，ldflags 注入；未注入回退 `dev (commit:xxx)` |
| CI 发布 | `.github/workflows/release.yml` | `release: created` 触发，矩阵 win/linux/darwin×amd64/arm64，**已用 `tag_name` 注入版本**，传 release assets |
| 本地构建 | `build.sh` / `build-linux.bat` | **未注入版本**（所以本地构建都是 dev） |
| 版本握手 | `client/help.go:48`、`share.go:154` | help 用 `JoinRequest{Version}`、share 用 `RegisterRequest{Version}` 上报 |
| relay 汇聚版本 | `relay/server.go:272/382/388/392`、`session.go:32` | 记录 `client.Version`，`SessionReady{PeerVersion}` 互传 |
| 客户端显示 | `help.go:91`、`share.go:250` | 已打印「对端版本: %s」 |
| **session 复用** | `relay/server.go:315`、`session.go:103/138` | share 带 ClientID → `GetSessionByClientID`+`ReuseSession`，复用同一 session |
| **help 断连去抖** | `session.go:239-251` | help 掉线 5s 内同 code 重 join 无缝接回 |

⇒ 「上报版本 → relay 汇聚 → 回传客户端 → 打印」链路已通；session 复用机制已在。本需求主要是**加一个 latest 权威参照 + 比对提示**，以及补齐 R3 的少量缺口。

## 决策记录

| # | 决策 | 选择 |
|---|---|---|
| 版本格式 | `0.0.3+<short-sha>`，**比较只看 semver core `0.0.3`**，commit 仅展示/溯源 | 已定 |
| 版本来源 | 自动，`git describe --tags`（CI 已用 tag_name，本地脚本待补 ldflags） | 已定 |
| 比对粒度 | semver core「大于」；relay 持 latest 完整串，普通连接 `!=` 即提示 | 已定 |
| 提示 vs 强制 | 提示为主 + 保留 `required`（协议不兼容时拒绝），暂不强制 | 已定 |
| 升级对象 | share 端 + help 端都要（help/MCP 较复杂，后置） | 已定 |
| 升级方式 | 自动重连（秒级闪断），非零断 handoff | 已定 |
| **分发源（R1）** | **待定**：GitHub release 直连 vs relay 镜像。推荐见下 | ⏳ |

### commit 为什么不能用来比新旧
- semver 2.0：`+commit` 是 build metadata，**不参与版本优先级比较**（`0.0.3+aaa` == `0.0.3+bbb`）。
- commit hash **无序**，不是单调量，比不出大小。
- ⇒ 「新旧」只能靠 **semver core（发布时递增 tag）**；commit 仅展示。relay 持 latest 完整串，`!=` 即提示。同 tag 内若要可比，用 `git describe --long` 的 commit 距离 `0.0.3-dev.N+gxxxx`（N 单调）。

### 分发源推荐：GitHub 当上游，relay 当面向客户端的镜像/权威
理由：①GitHub release CI 现成（零改动出带版本号的多平台二进制）；②**客户端直连 github 不可靠**（公司内网麒麟/工作机，remote 走裸 dial 不吃代理），但**客户端连 relay 是刚需必通**，relay 在海外 VPS 访问 github 畅通。

```
打 tag → GitHub Actions 构建+发 release assets（已实现）
   └─ relay 从 release 拉取/缓存最新二进制（VPS 海外畅通）
        └─ 客户端只跟 relay 交互：连接拿 latest 提示 / 自更新从 relay 下载
```
latest 权威 = relay（同步自 github）；下载源 = relay；发布流程仍在 GitHub。

## R3 平滑升级可行性（自动重连）

- **已有**：share 用 ClientID 复用 session（`ReuseSession`）；help 5s 断连去抖。
- **缺口**：
  1. share 断连立即给 help 发 `PEER_DISCONNECTED`（`session.go:253`，无去抖）→ 需给 share 加去抖，或 help 端收到后进入「等 share 重连」而非退出。
  2. 客户端需加自动重连循环（`client.go` 支持重连，但 `RunMCPMode` 现在断了不重连）。
  3. MCP 模式 help 自更新涉及 stdio 子进程 + Claude Code 侧重连，复杂，后置。
- 结论：**share 端平滑升级改造量小-中，地基已在**。

## 关键改动点（实现时锚点）

- 版本注入：`build.sh` / `build-linux.bat` 加 `-ldflags "-X .../version.Version=$(git describe --tags)+$(git rev-parse --short HEAD)"`；`release.yml` 的 ldflags 拼上 short-sha。
- 协议：`proto/message.go` 给 `SessionReady` 加 `LatestVersion` / `UpdateHint` 字段。
- relay：`server.go` 的 `handleRegister`/`handleJoin` 比对 latest 回提示；新增 latest 版本配置 + 二进制下载端点 + 从 github release 同步。
- 客户端：`help.go`/`share.go` 收提示打印；加自动重连循环。
- R3：`session.go` 给 share 断连加去抖。

## 分期建议

- **MVP**：R0 + R2（版本注入 + relay 持 latest + 连接比对提示）。复用现有握手字段，改动集中在 version/build 脚本、`proto/message.go`、`relay/server.go`、客户端打印。
- **二期**：R1 relay 下载端点（托管二进制 + 从 github 同步）。
- **三期**：R3 平滑升级（share 去抖 + 客户端自动重连）。

## 待办/待确认
- 最终敲定分发源（github 直连 vs relay 镜像）。
- tag 前缀：用 `v0.0.3` 还是 `0.0.3`（影响 `git describe` 解析与 semver 比较的剥离逻辑）。
- `required` 的触发条件（仅协议不兼容？还是 core 低于某门槛）。

---

## 实测：make-before-break 交接升级（2026-06-17 已端到端跑通）

> 在 ARM64 UOS（`uos@192.168.137.27`）实测：share `0.0.3` → `0.0.4-test` 无缝、MCP 全程不失联。这比"杀旧起新 + 修 relay"更优，**建议作为 `--upgrade-peer` 的核心机制**（R3 可改用此法，绕开自动重连/relay 去抖的复杂度）。

### 机制（先建后断）
趁 old 通道还活着，把 new 的接入凭证从 old 通道读出来，确认能连 new，再撤 old。old 当安全网，任何一步失败可回退、不丢通道。
1. 起 new share（**独立 ClientID** + `--code-file <固定路径>`），`setsid` 后台。
2. **经 old 通道 `exec` 读 new 的 code-file** → help 拿到 new code（关键，保证不失联）。
3. help `connect(new_code)` 主动切到 new。
4. 经 new 通道 `kill` old。
5. 收尾：固化 new（标准 ClientID/版本、开机自启）、清理。

### 为什么比之前方案优
- **绕开 relay session 残留 bug**：new 用独立 ClientID，不和 old 抢同一 session → 无抽风。relay 那个 bug 可先不修。
- **不丢通道、不需带外救援**：new code 经还活着的 old 通道读出。
- **不强依赖 help 自动重连**：help 主动 `connect` 切换。
- **天然可回滚**：任何一步失败，old 还在、help 还连着 old。

### 实测确认的工程点（务必设计进去）
1. **独立 ClientID 是硬要求**：同 ClientID 多 share 撞 relay 残留 → 反复 `EOF` 抽风。隔离用 `HOME=<临时目录>`（ClientID 存 `$HOME/.remote_assist_client_id`）。
2. **`--code-file` 必备**：让 help 经 old 通道拿到 new code。
3. **进程名 >15 字符被 `comm` 截断**（`remote-linux-arm64`→`remote-linux-ar`），kill 用 PID/`pgrep -f`，别用 `pgrep -x 全名`。
4. **relay 跨境传大二进制不可靠**：`upload_file` 9.5MB 经海外 relay 断过。二进制分发走 SSH/LAN/p2p 或分块+重试；通道控制（小消息）走 relay 没问题。
5. **`--root` 文案 bug**：`share -h` 写 "required"，`cmd/remote/main.go:130` 实际默认回退 CWD。

### `--upgrade-peer [version]` 实现轮廓
1. help 检测 share 版本旧（握手 `PeerVersion`）。
2. 选对应平台（GOOS/GOARCH）× 目标版本二进制（本地存档 / relay 取）。
3. 传二进制到 share（优先 SSH/LAN；走 relay 要分块 + 重试）。
4. 起 new（独立 ClientID via `HOME` 隔离 + `--code-file`）。
5. 经 old 通道读 new code → `connect(new)` → 经 new 通道 `kill` old。
6. 固化 new（ClientID/开机自启）+ 清理临时文件。
- **唯一硬约束**：help 要能提供 "share 平台 × 目标版本" 的二进制。

---

## 传输可靠性改进（2026-06-17 确定要做）

### 背景（实测 + 代码确证）
`upload_file` 9.5MB 经跨境 relay 传到 chunk 12（~6MB）断（`tunnel_lost`）。根因不是带宽：
- `doUploadFile`（`internal/client/help_bootstrap.go`）**同步串行分块**（512 KiB/chunk，每块阻塞等跨境往返 ACK），**无重试、无续传**（任一块 `CallTool` 失败整个 `return error`，line 218-219；半截文件不回滚，line 179）。
- help 后台读循环 **2min read deadline**（`help_bootstrap.go:125`），靠 30s 心跳维持；跨境链路 ≥2min 静默（连心跳 echo 都收不到）→ 判 `tunnel_lost`（line 126-139）。
- ⇒ "传输耗时长 × 链路抖动窗口 × 一断全废"，9.5MB 只是拉长暴露时间、提高命中概率。

### 改进 1：upload/download 重试 + 断点续传（先做，低风险）
- **chunk 级重试**：每个 chunk 的 `CallTool` 失败重试 N 次（递增间隔），抗瞬时抖动。
- **断点续传**：`fileTransferArgs` 加 `Offset int64`：
  - upload：`f.Seek(offset)` 后读；远端首块按 `offset>0 ? Append : Create`（offset>0 不 truncate）。
  - download：从 `offset` 起 `read_file`，本地以 offset 定位写。
  - 失败 error 带"已传字节数"，调用方 reconnect 后用 `offset` 续传。
- 改动点：`internal/client/help_bootstrap.go` 的 `doUploadFile`/`doDownloadFile` + `fileTransferArgs`；schema（`help_mcp.go`/工具定义）暴露可选 `offset`。

### 改进 2：MCP 接 p2p（根治跨境绕路，后做）
- 现状（grep 确证）：`NewP2PManager` 仅 `help.go:129`/`share.go:274`（SSH 隧道 `negotiateP2P`）；`HandlePeerAddrReady` 仅 `help.go:171`/`share.go:316`。**MCP 两路径（`help_bootstrap.go` connect、`help_mcp.go` RunMCPMode）都没接**，工具流量全走 relay TCP；`help_mcp.go:53` 收到 `MsgPeerAddrReady` 直接跳过。
- 改：在 `connect` 工具路径（`help_bootstrap.go`）建 bridge 后，照 `help.go` 模式起 `negotiateP2P`，成功则工具消息走 P2P UDP tunnel、失败回退 relay；优先 LAN 快速通道（同网络直打私网，秒成）。share 端 p2p 已就绪。
- 收益：同 LAN 直连（升级传二进制飞快 + 不断），跨境也降延迟。复杂度中-高（要把工具通道 `SendMessage`/`ReadMessage` 切到 p2p tunnel）。

### 优先级
改进 1 独立、低风险、**先做并实施**；改进 2 根治、改动大、第二步。
