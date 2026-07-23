# Relay 公网安全整改计划第五版评审

## 评审范围

- 计划源文件：`D:\claude-model\.claude-ifly\plans\dreamy-finding-parasol.md`
- 评审对象：第五版计划，重点复核第四轮 `3 P1 + 4 P2` 的落实情况。
- 评审边界：仅评审计划，不评审尚未完成的实现，不修改计划原文。

## 结论

第四轮 7 条意见均已在第五版闭环：SNAT 四类限制统一旁路、零值安全配置、活跃数据面谓词、UDP 字节桶顺序、报文尺寸契约、relay 删除 identity、48 bit 审计指纹及可靠验收方式均已明确。

本轮结论仍为 **暂不通过**，新增发现 **2 项 P1、1 项 P2**。两个 P1 分别涉及 UDP 快速重连时旧 peer 状态跨代残留，以及 Join 失败响应/计数没有统一安全出口。

## 评审发现

### P1-1：稳定 sessionID 会让旧 UDP peer 状态跨快速重连残留

第五版第 88、95 行用 `IsActiveDataSession(sessionID)` 判断当前两端控制连接是否在线，第 104 行要求 validator 失效时删除 `relaySession`。但 `relaySession` 仍以稳定的控制面 `sessionID` 为唯一键，没有数据面 generation：

- ClientID Share 重连会复用原 session 和 sessionID（计划第 87 行）。
- Help 去抖允许新 Help 替换旧 Help，JoinResponse 仍返回同一个 `session.ID`（当前 `internal/relay/server.go:392-403`）。
- MCP Help 重连会先关闭旧连接、随即建立新连接并 Join（`internal/client/help_bootstrap.go:238-258`），可能在 cleanup 周期内完成。
- UDP relay 状态当前保留 5 分钟（`internal/p2p/stun_server.go:11-15`），并持有两个自动发现的 peer 槽。

可重复触发的序列是：

1. Share A 与 Help B 建立 session S，UDP relay 记录旧地址 A/B。
2. B 断开，`IsActiveDataSession(S)` 短暂变 false，但该窗口内没有 UDP 包，周期 cleanup 也尚未运行，因此旧 relay entry 没有观察到 false。
3. 新 Help C 立即 Join 同一控制 session S，validator 再次变 true。
4. C 从不同公网 IP 发包时，旧 relay entry 已有 A/B 两个槽，现有“同 IP 更新端口”规则无法替换 B，C 会被当作未知第三 peer 拒绝，最长等待旧 entry TTL；旧 B 的地址则仍留在数据面状态中。

因此布尔 validator 只能校验“此刻 session 是否活跃”，不能证明现有 relay entry 属于当前这一代 Share/Help 配对。

计划需要增加明确的数据面换代机制，推荐二选一：

1. **数据面 generation/ID**：每次成功建立或替换 Share/Help 配对时生成新的 `dataSessionID`，通过 JoinResponse/SessionReady 下发，UDP header 和 relay map 使用该 ID；旧 generation 立即失效。
2. **控制面主动失效**：在 Share 复用、Help 进入 pending disconnect、Help 被替换及 session 删除时，Server 主动调用 STUNServer 的 `InvalidateRelaySession(sessionID)`，不能依赖“下一包恰好看到 validator=false”或一分钟 cleanup。跨 SessionManager/STUNServer 的调用必须在释放 `sm.mu` 后执行，明确锁顺序，避免 `sm.mu`/`relayMu` 反转。

如果选择主动失效，Join 新 Help 前必须完成旧 relay entry 删除，再向双方发送可开始 P2P 的响应。测试必须覆盖“失效窗口内没有任何 UDP 包、cleanup 尚未触发、不同公网 IP 快速重连”，并断言旧 peer 不会收到新 generation 数据、新 peer 可立即占槽。

### P1-2：Join 失败仍会泄露会话状态，限流拒绝是否计入连接失败次数未定义

风险评估 `docs/SECURITY_RISK_ASSESSMENT.md:198-210` 要求单连接失败达到阈值后关闭，且失败响应不泄露不必要的会话状态。第五版第 114-120 行定义了三层限制，但没有修改当前普通 Join 失败响应，也没有定义各失败分支如何统一累计 `joinFailures`。

当前 `internal/relay/server.go:366-374` 直接把 `JoinSession` 的 `err.Error()` 返回客户端，攻击者可以区分：

- code 不存在；
- code 已过期；
- session 已有 Help；
- Share 已断开或 session 已关闭。

其中“already has helper”等响应会确认某个协助码曾经或当前有效，不满足统一失败面的要求。

计划应增加单一 `rejectJoin` 路径并明确以下语义：

- `ErrCodeInvalid`、`ErrCodeExpired`、`ErrSessionHasHelper`、`ErrSessionNotFound` 对外返回同一稳定错误码/消息；详细原因只进入受采样保护的服务端审计日志。
- per-IP/global bucket 拒绝也属于一次失败；必须累计该连接的 `joinFailures`，不能因未调用 `JoinSession` 而绕过 5 次关闭阈值。
- 第 5 次失败发送至多一个通用错误后立即关闭；畸形 payload 仍按状态机立即关闭。
- 只有真正 Join 成功才进入 help 状态；不能在处理第 5 次尝试前预先关闭，否则“前 4 次失败、第 5 次合法”会被误拒。

测试需断言不同内部错误得到完全相同的公开响应、连续限流拒绝同样会在阈值处关闭，以及前 4 次失败后合法 Join 仍可成功。

### P2-1：精确大小的 UDP 读缓冲无法实现“n 超限拒绝”

第五版第 96 行计划使用 `make([]byte, maxRelayDatagramSize)`，然后通过 `n` 超限识别过大数据报。但 Go `net.PacketConn.ReadFrom` 的契约明确为 `0 <= n <= len(p)`（本机 Go 源码 `C:\Program Files\Go\src\net\net.go:326-332`）。缓冲区长度正好等于上限时，`n > maxRelayDatagramSize` 永远不可能成立；更大的 UDP 数据报会被截断到缓冲区长度，截断后的前缀仍可能形成可通过 `parseRelayHeader` 的合法头部。

计划应明确采用下列方案之一：

- 分配 `maxRelayDatagramSize + 1` 字节缓冲；`n > maxRelayDatagramSize` 时拒绝。这样无需接收完整 64 KiB 包，也能识别至少超出一个字节的报文。
- 使用能返回截断标志的 `ReadMsgUDP`，检测平台对应的 `MSG_TRUNC` 后拒绝。

同时应显式校验 `sidLen <= maxSessionIDLen`，而不只校验 `len(data) >= 2+sidLen`。验收必须通过真实 UDP socket 发送“恰好上限”和“上限+1”两个数据报，不能只直接调用 parser；前者正常，后者不得入队或创建/更新 relay 状态。

## 已验证通过的第四轮整改

- `DisableSourceIPLimits` 使用零值安全的负向配置，并覆盖连接数、活跃会话数、create、join 四类 per-IP 限制。
- `IsActiveDataSession` 区分控制 session 存在、协助码有效和数据面活跃，活跃会话跨 TTL 仍有效，断开/pending 会话无效。
- UDP 字节桶明确采用 per-session 后 global，并补充双 session 公平性测试。
- UDP 最大报文回到约 1400 字节 payload 加 relay header，不再扩大到 64 KiB。
- `deleteRelaySessionLocked(id, expected)` 的 identity 和计数语义已经明确。
- 审计 HMAC 先截取原始摘要 6 字节再 hex，输出固定 12 hex 字符。
- LRU O(1) 使用确定性操作计数测试；benchmark 仅观察；worker 生命周期由自有 WaitGroup 保证。

## 下一版最低通过条件

1. 为 UDP relay 增加数据面 generation，或在所有参与方更换/断开路径主动、同步失效旧 relay entry，并补快速重连测试。
2. 所有 Join 失败走统一公开响应和统一连接失败计数路径，覆盖 limiter 拒绝。
3. UDP 接收使用 `max+1` 缓冲或截断标志，真实 socket 验证上限与上限加一，并限制 sessionID 长度。
