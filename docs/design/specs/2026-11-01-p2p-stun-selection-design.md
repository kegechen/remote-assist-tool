# P2P STUN 选择改进设计（relay 下发 STUN + 客户端竞速发现）

状态：已确认 · 2026-11-01

## 1. 背景与问题

Windows relay（华为云）安全组未放行 UDP 入站，`relay:3478` 对外不可达（本机 coturn 能应答，但包进不来），且我们没有控制台权限，改不了安全组。连这台 relay 的会话 P2P 基本全部失败。

读代码后确认的现状（以 `feat/relay-reconnect-grace` / master 为准）：

1. 客户端默认 STUN = relay 主机:3478（`cmd/remote/main.go:338/423`），`--stun` 可手动覆盖，但没有用户会去填。
2. 公网地址发现 `DiscoverPublicAddrVia`（`internal/p2p/stun.go:263`）对单个服务器按 3s/5s/8s 重试，**不可达时要等满 16s** 才放弃，然后再依次试 Google ×2、Cloudflare（`internal/p2p/manager.go:210`），每个不通的也是 16s。
3. NAT 类型探测 `DetectNATType` 取列表里前两个不同 IP 的服务器（`internal/p2p/nat.go:18`），列表第一个就是 `relay:3478`。relay 不通时第一次查询 3s 超时后直接返回「Cone」——**这台 relay 下 NAT 类型永远是 Cone**，对称型 NAT 走不到生日攻击分支。
4. 同一个 STUN 地址还兼作 UDP 中继（`manager.go:371`，并用于隧道来源判定 `manager.go:601`、`share.go:774`）。这项只能指向本 relay 自己（会话状态在本进程内）。

本机实测（国内办公网，2026-11-01）单次发现耗时：

| 服务器 | 结果 |
|---|---|
| 23.95.78.14:3478（Linux relay） | 616 ms 成功 |
| 113.44.139.100:3478（Windows relay） | 16008 ms 超时 |
| stun.chat.bilibili.com:3478 | 21 ms 成功 |
| stun.miwifi.com:3478 | 551 ms 成功 |
| stun.cloudflare.com:3478 | 152 ms 成功 |
| stun.l.google.com:19302 | 171 ms 成功（很多国内网络 DNS 被污染，不可依赖） |
| stun.qq.com / stun.hitv.com:3478 | 16 s 超时 |

结论：失败的主因不只是「拿不到公网地址」，还有**串行的 16s 超时**拖慢两端的地址交换、以及**NAT 类型探测失效**。只把 relay 换成能通的 STUN 能解决一部分；把发现改成竞速才是主要收益。

## 2. 目标 / 非目标

目标
- relay 运营方可以给客户端指定 STUN 服务器（满足用户诉求：Windows relay 让客户端用 Linux relay 的 3478）。
- 任何一台 STUN 不通都不再拖慢 P2P：发现阶段总耗时上限约 3s。
- NAT 类型探测只用「实际应答过」的服务器。
- 能在看板上看到每端用哪台 STUN、耗时多少，用来量化改进前后的成功率。
- 老客户端、老 relay 互相兼容，行为不变。

非目标
- 不改打洞算法本身（直连、生日攻击、超时与后台重试不动）。
- 不把 UDP 中继改成跨服务器（会话状态不在别的进程里）。
- 不解决对称型 NAT ↔ 对称型 NAT 这类本身就很难打通的组合。

## 3. 方案

### 3.1 relay：下发 STUN 列表（协议新增可选字段）

- 新参数 `--advertise-stun host:port[,host:port...]`，Windows 服务 `config.json` 对应 `advertise_stun`。最多 4 个，启动时校验格式（host 非空、端口 1–65535），非法则启动失败并报错。
- `RegisterResponse` 与 `JoinResponse` 新增 `stun_servers []string`（`omitempty`）。未配置时不下发，与现状完全一致。
- 老客户端忽略该字段。
- Windows relay 部署时配置 `advertise_stun: "23.95.78.14:3478,stun.chat.bilibili.com:3478,stun.cloudflare.com:3478"`；Linux relay 可不配（自身 3478 可达）。
- 信任边界：relay 本就能看到并转发全部流量，下发 STUN 不扩大信任面；恶意值最多导致拿到错误公网地址、P2P 失败、回退中继。

### 3.2 客户端：候选列表与优先级

候选按以下顺序合并去重（保留顺序，最多 8 个）：

1. 本端配置：`--stun` 显式值，未指定时为 relay 主机:3478（现有默认，两者在 Config 里是同一个字段）；
2. relay 下发的 `stun_servers`；
3. 内置兜底：`stun.chat.bilibili.com:3478`、`stun.miwifi.com:3478`、`stun.cloudflare.com:3478`、`stun.l.google.com:19302`。

顺序只影响 NAT 判定时的先后，谁先应答由竞速决定。`--stun` 为空（standalone / 回环 relay）且未下发时保持现状：不做 STUN。

UDP 中继地址（`stunAddr`）**仍然只取 relay 主机:3478**（或 `--stun` 显式值，保持现有语义），与发现候选分开，不受下发列表影响。

### 3.3 客户端：竞速发现（替换串行重试）

- 同一个主 socket 向所有候选并发发 Binding Request（各自独立 transaction ID），按 transaction ID 匹配响应，**第一个合法响应即为公网地址**。
- 轮次：0s 全发，1s 后对未应答的重发，2.5s 再重发一次，总时限 3s；全部失败则与现状一样不带公网地址继续（回退中继）。
- 只接受来源 IP:port 等于某个已发请求目标、且 transaction ID 匹配的响应（防注入；与现有 `UnpackSTUN` 校验叠加）。
- 记录每个应答服务器的映射地址与 RTT，供 3.4 使用。
- 主 socket 在发现结束后才进入 `receiveLoop`，与现有时序相同，不会和打洞包混读。

### 3.4 NAT 类型探测

- 只从「发现阶段已应答」的服务器中挑两个不同 IP 的做端口比较（仍用临时 socket，逻辑不变）。
- 不足两个时保持现状返回 Cone，但日志写明原因。
- 发现阶段已拿到多个映射：若主 socket 对两个不同 IP 服务器的映射端口已经不同，可直接判定 symmetric，省掉临时 socket 的一轮查询（可选优化，实现时视复杂度决定）。

### 3.5 可观测性

- `PeerAddrAdvertise` 新增可选字段 `stun_server`（实际采用的那台）与 `stun_ms`（发现耗时）。老 relay 忽略。
- relay 记入会话（实时会话详情显示「STUN：stun.chat.bilibili.com:3478，21 ms」），analytics 会话表加 `share_stun`/`help_stun` 两列（沿用已有的原地迁移机制），看板「连接方式」区块可按 STUN 来源拆 P2P 直连率。
- 用于上线后对比：Windows relay 的 P2P 直连率改进前后。

## 4. 兼容性

| 客户端 \ relay | 老 relay | 新 relay（未配置） | 新 relay（配置 advertise_stun） |
|---|---|---|---|
| 老客户端 | 现状 | 现状 | 现状（忽略字段） |
| 新客户端 | 竞速（relay:3478 + 兜底） | 同左 | 竞速（下发优先） |

即使 Windows relay 不升级，新客户端靠竞速 + 国内兜底也能拿到地址、不再等 16s；relay 下发是运营方的额外控制手段。

## 5. 改动范围

- `internal/proto/message.go`：三个可选字段。
- `internal/relay`：参数、下发、记录 STUN 来源；`cmd/relay` 参数与 Windows `config.json`。
- `internal/p2p`：候选合并、竞速发现、NAT 探测选服务器。
- `internal/client`、`cmd/remote`：把下发列表传给 P2P manager。
- web 控制台 / analytics（仅 web-console 分支）：展示与两列迁移。
- 分支：协议 + relay + 客户端在 **master** 实现并发版（客户端要靠正式 release 分发）；随后同步到 `feat/relay-reconnect-grace` 并补看板展示。

## 6. 测试

- 单测：候选合并顺序与去重；竞速在「第一个不通、第二个通」时 ≤1.2s 返回；全部不通 3s 返回；伪造来源/错误 transaction ID 的响应被丢弃；NAT 探测只用已应答服务器。
- relay：参数校验、未配置时响应 JSON 与现状逐字节一致、配置后下发。
- 端到端：本机起两个 relay（一个 3478 不监听，模拟 Windows），新客户端 share/help 打洞，确认发现耗时与 P2P 结果；老客户端连新 relay 回归。
- 上线后：看板对比 Windows relay 的 P2P 直连率。

## 7. 发布与部署

1. master 合入、发 release（客户端新版本）。
2. relay：Linux、Windows 都升级到含 3.1 的版本；Windows 配置 `advertise_stun`。按回滚式流程部署（备份、哈希校验、rollback 脚本），Windows 选无人连接时段。
3. 用户升级客户端后生效；relay 的升级提示可顺带推新版本号。

## 8. 已确认的决定（2026-11-01）

1. 内置兜底列表包含第三方公共 STUN（bilibili、小米、Cloudflare、Google）。
2. Windows relay 下发「Linux relay + bilibili + Cloudflare」三个。
3. 3.5 可观测性本期实现。
