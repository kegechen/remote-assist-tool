# Relay 安全整改代码评审（第二轮）

## 结论

**暂不通过，不建议提交。** 第一轮的两个 P0 已有实质进展：TCP 状态机已阻止重复 register/join，STUN 已改为固定 worker 池且公网入口默认关闭；审计文件中的协助码也已改为 HMAC 指纹。

本轮仍发现 **8 项 P1、1 项 P2**。主要集中在：普通日志仍泄露凭据、STUN 关闭竞态、会话计数泄漏、锁外解引用 session、ClientID 重连协议不完整、SNAT 旁路不完整，以及 UDP/注册后消息的容量保护尚未落实。

本轮只评审代码和测试，不修改实现。

## 评审发现

### P1-1：普通日志仍输出完整协助码和持久 ClientID

位置：`internal/relay/server.go:458-495`、`internal/relay/code.go:55-60`

审计日志已使用 `code_fp`，但两条普通运行日志仍调用 `FormatCode(code)`。`FormatCode` 只是插入连字符，输出仍是完整 bearer credential。复用日志还直接输出持久 `client.ClientID`；该值当前具有会话复用授权能力，泄露后可用于换绑 Share。

`MaskCode` 已实现但没有任何生产调用点。应把普通日志改为固定掩码或 HMAC 指纹，ClientID 同样只记录带服务端密钥的指纹。验收必须捕获 stdout/stderr 和审计文件，断言原始 code、持久 ClientID 均不存在。

### P1-2：STUN Close 可与接收循环并发触发 send-on-closed-channel

位置：`internal/p2p/stun_server.go:132-181`、`internal/p2p/stun_server.go:275-292`、`internal/p2p/stun_server.go:397-420`

`Close` 设置 `closed=true`、关闭 UDP conn 后立即 `close(taskQueue)`，但没有等待 `serve` goroutine 退出。若 `serve` 已成功读到一个数据报、正在构造 task 或执行 select，`Close` 可以先关闭 channel，随后 `serve` 对已关闭 channel 发送并 panic。

当前 WaitGroup 只覆盖 worker，不覆盖 `serve` 和 cleanup；cleanup 还可能在关闭后等待最多一分钟 ticker 才退出。

应使用 `done`/context 和覆盖 serve、cleanup、worker 的自有 WaitGroup：先停止接收并等待 producer 退出，再关闭 taskQueue，最后等待 workers。补并发 UDP flood + Close、多次 Close、队列满时 Close 和立即 Close 测试。

### P1-3：per-IP 会话计数会永久漏减，并在跨 IP 复用时记错所有者

位置：`internal/relay/session.go:100-137`、`internal/relay/session.go:337-375`、`internal/relay/session.go:398-473`

创建时只在 `sessionCountPerIP` 增加计数，却没有把计费 IP 保存到 `TunnelSession`。删除时临时从当前 `session.Share.Conn` 反推 IP：

- Share 先断开时 `DisconnectClient` 把 `session.Share=nil`；之后 TTL cleanup 删除会话时没有 IP 可减，旧 IP 配额永久泄漏。
- `CloseSession` 遇到 Share 已为空时同样漏减。
- ClientID 从新 IP 复用后，删除会按新 Share IP 递减，但创建时增加的是旧 IP；旧 IP 泄漏，新 IP 还可能误减其他会话的计数。

外部可重复触发“创建 -> Share 断开 -> 过期清理”，最终让该 IP 即使已经没有会话也永久达到 `maxActiveSessionsPerIP`。

应在 session 内保存不可丢失的计费 IP，并通过唯一、幂等的删除函数按该字段递减；若复用需要迁移所有者，则必须在同一锁内原子迁移两个 IP 的计数。补断开后清理、Close、跨 IP 复用、重复删除和容量恢复测试。

### P1-4：Join 和复用路径仍在锁外读取可变 Share/Help 指针

位置：`internal/relay/server.go:509-516`、`internal/relay/server.go:538-573`、`internal/relay/session.go:225-276`

`JoinSession` 只快照了 Share 的 Version/Host，调用方仍在锁外读取：

- `session.Share.ID` 用于 Activate；
- `session.Share` 用于发送 SessionReady；
- `session.Share.ID` 用于审计日志。

Share 可被并发断开或 ClientID 复用，因而这里仍有数据竞态，并可能在 `.ID` 解引用时 nil panic。复用路径的 `GetSessionByID` 也返回内部可变指针，释放 RLock 后再判断 `session.Help != nil` 和读取 `session.Help.ID`，存在同类竞态。

应让 SessionManager 返回包含 sessionID、expected Share/Help ID、目标连接及展示字段的不可变结果快照；调用方不得在解锁后继续解引用 `TunnelSession` 内部字段。race 测试需并发 Join、Share 断开和 ClientID 复用。

### P1-5：ClientID Share 重连没有完成客户端协议切换，旧连接也未关闭

位置：`internal/relay/session.go:196-222`、`internal/relay/server.go:458-517`；客户端行为：`internal/client/share.go:175-183`、`internal/client/share.go:241-280`

`ReuseSessionByClientID` 直接替换 Share，但没有返回或关闭旧 Share 连接。旧连接可以持续 heartbeat 并占用连接名额，重复复用会累积这类孤立连接。

当原 Help 仍在线时，Server 只在内部把 `dataPlaneReady` 设回 true，没有向新 Share 发送 `SessionReady`。`RegisterResponse` 只包含 code/expiry，不包含 sessionID；Share 客户端注册后会阻塞等待 `SessionReady` 才开始 P2P。因此所谓“重新激活”没有让新 Share 获得 sessionID，也没有通知现有 Help 重建配对，控制面和客户端状态不一致。

应让原子复用结果返回旧连接和当前 Help 快照，锁外关闭旧连接；随后明确协议：要么向两端发送重新配对通知并等待确认后激活，要么关闭现有 Help，要求其重新 Join。不能只修改服务端布尔值。

### P1-6：DisableSourceIPLimits 没有统一旁路四类 per-IP 限制

位置：`internal/relay/server.go:221-267`、`internal/relay/server.go:392-405`、`internal/relay/server.go:438-490`、`cmd/relay/main.go:19-65`

该配置目前只作用于部分桶：

- `acquireConnSlot` 仍无条件执行 `maxConnsPerIP`；
- `CreateSession` 仍无条件执行 `maxActiveSessionsPerIP`；
- create/join/heartbeat per-IP 桶可以旁路；
- Tunnel limiter 实际按 `client.ID`，属于 per-connection，却被该“禁用 source-IP 限制”开关错误旁路。

此外 `cmd/relay` 没有对应命令行参数，实际部署无法选择计划中的 ELB SNAT 模式。

应统一语义：开关只旁路连接数、活跃会话数、create 和 join 等真正按来源 IP 的限制，per-connection 与所有 global 限制始终保留；同时提供明确 CLI 映射和 SNAT 测试。

### P1-7：注册后的高频消息仍可触发 O(session) 扫描和逐请求日志

位置：`internal/relay/server.go:380-424`、`internal/relay/server.go:629-657`、`internal/relay/session.go:398-434`、`internal/relay/session.go:489-565`

SessionManager 仍没有 `byConnID`：`FindPeer`、`UpdatePeerAddr` 和 `DisconnectClient` 都遍历全部 session。攻击者注册一次后可以无限发送 Tool 消息或 PeerAddrAdvertise，每条触发 O(session) 扫描；PeerAddrAdvertise、P2PConnected 还逐条写普通日志。

Tunnel/Heartbeat 虽新增 token bucket，但每个被限流的请求都 `log.Printf`，攻击者在桶耗尽后仍可按输入速率制造日志。未知消息同样只逐条记录、不关闭连接、不限流。

应增加并维护 `byConnID`，让定位为 O(1)；为可重复控制消息定义幂等/频率约束；所有拒绝和异常日志采样。未知消息应按协议错误关闭。Tool 数据还需纳入与 Tunnel 等价的每连接/全局资源预算。

### P1-8：UDP worker 有界，但 PPS、状态数和字节出口仍未实现

位置：`internal/p2p/stun_server.go:67-94`、`internal/p2p/stun_server.go:132-228`、`internal/p2p/stun_server.go:300-375`

固定 worker 和有界 channel 只限制同时处理任务数。接收循环仍会对每个数据报先分配并复制 `data`，然后才发现队列满；没有 ingress per-IP/global PPS，UDP flood 的分配和 GC 压力仍与包速率线性增长。

同时仍缺：relay session 总量/per-IP 上限及幂等计数释放、per-session 后 global 的字节桶、STUN/relay 出口带宽限制。`handlePacket` 对 invalid、non-binding、binding 仍逐包写日志。导出的 `NewSTUNServer` 还允许 nil validator，启用 relay 后任意 sessionID 可建状态。

默认关闭 UDP 是正确的临时控制，但显式 `--stun` 仍会启用未完成的 SR-002 路径。应完成上述容量保护，或在本阶段完全禁用 relay 能力并明确阻止生产开启。

### P2-1：KeyedLimiter 仍不是计划中的 O(1) LRU，关键测试和 benchmark 仍缺失

位置：`internal/ratelimit/ratelimit.go:61-145`、`.github/workflows/test.yml:30-36`

本轮把满表扫描限制为最多 100 个 entry，关闭了最坏 O(maxKeys) CPU 放大，但实现仍不是注释所称的 LRU：没有双向链表，也不能保证存在空闲 entry 时一定找到并淘汰，合法新 key 可能被概率性拒绝。

CI 继续运行 `-bench=.`，但 `internal/ratelimit` 没有任何 `Benchmark*`。新增测试也未覆盖：STUN Close 竞态、session per-IP 计数释放、SNAT 旁路、真实 UDP 快速重连、旧 Share 关闭以及重连双方协议消息。

应实现确定性的 O(1) LRU/空闲淘汰与原子 Allow 语义，增加操作计数测试和真实 benchmark，并补齐上述跨组件验收。

## 第一轮问题状态

- P0-1 TCP 状态机：**已关闭主路径**。重复 register/join、跨角色和注册前业务消息已有守卫及测试。
- P0-2 每包 goroutine/共享缓冲：**部分关闭**。已使用有界 worker 和复制任务，但 Close 生命周期与 UDP 入口容量仍未闭环。
- P1-1 UDP 失效 TOCTOU：**部分关闭**。新 entry 创建前增加复检，但缺受控交错测试。
- P1-2 协助码日志：**部分关闭**。审计文件已改指纹，普通日志仍泄露。
- P1-3 Join 回滚/竞态：**部分关闭**。已有 identity 回滚，Share/Help 快照仍不完整。
- P1-4 ClientID 原子复用：**部分关闭**。查找和替换已合锁，但旧连接、不可变返回值和重连协议未闭环。
- P1-5 限流矩阵：**部分关闭**。TCP create/join/heartbeat/tunnel 已增加部分桶，UDP和控制消息仍缺。
- P1-6 KeyedLimiter：**部分关闭**。扫描已定长，但并非定稿要求的 O(1) LRU。
- P2-1 测试/benchmark：**未关闭**。

## 验证结果

- `git diff --check`：通过。
- `go vet ./internal/relay ./internal/p2p ./internal/logger ./internal/ratelimit`：通过。
- `go test ./internal/relay ./internal/p2p ./internal/logger ./internal/ratelimit -count=1 -timeout 60s`：通过。
- 目标状态机/重用测试重复 20 次：通过。
- UDP 上限测试重复 100 次：通过。
- 第一次 `go test ./...` 在系统剪贴板往返测试失败，实际读到另一进程写入值；该测试单独复跑通过，第二次全量 `go test ./...` 通过，判定为环境共享剪贴板干扰。
- `go test -race`：当前 Windows 环境仍为 `CGO_ENABLED=0` 且没有 C 编译器，未执行；必须以 Ubuntu CI 的真实结果为准。

## 下一轮最低通过条件

关闭全部 P1，尤其先修复 STUN Close 生命周期、session 计数所有权和所有锁外 session 指针读取；然后补跨组件 race/状态转换测试。UDP 在 PPS、状态与字节限制完成前继续保持默认关闭，且不得作为已完成能力上线。
