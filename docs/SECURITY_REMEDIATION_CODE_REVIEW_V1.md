# Relay 安全整改代码评审（第一轮）

## 结论

**暂不通过，不建议提交。** 当前待提交改动没有完整落实已定稿计划，且存在可直接触发的状态机绕过、UDP 无界并发、数据面失效竞态和敏感凭据日志泄露。全量普通测试通过不能覆盖这些并发与攻击路径。

本轮只评审代码和测试，不修改实现。

## 评审发现

### P0-1：连接状态机仍未生效，单连接可以无限创建会话

位置：`internal/relay/server.go:302-342`、`internal/relay/server.go:353-403`、`internal/relay/session.go:89-109`、`internal/relay/session.go:291-327`

`handleMessage` 对 `MsgRegisterRequest` 无论当前 `client.Type` 是什么都会调用 `handleRegister`；payload 解码失败时也会继续注册。`handleRegister` 随后无条件把角色设为 share，并在空 ClientID 场景创建新会话。代码没有全局会话上限、per-IP 会话上限或 create 速率桶。

因此一个 TCP 连接可以反复发送 register，每次向 `sessions` 增加一条记录。连接关闭时 `DisconnectClient` 只找到并处理第一条匹配会话就返回，其余会话继续保留到 TTL。这正是 SR-001 的 P0 路径，当前改动没有关闭。

修复要求：

- 在 dispatch 前实施完整状态机；`new` 只接受一次合法 register 或 join。
- 第二次 register、register 后 join、join 后 register、业务消息早于注册都应返回协议错误并立即关闭。
- register payload 解码失败立即关闭，不能进入 `handleRegister`。
- 增加 create 的 per-IP/global 桶以及活跃会话 per-IP/global 上限；创建失败不得写部分状态。
- 增加上述状态转换和重复注册不增加会话数的测试。

### P0-2：STUN 仍是每包一个 goroutine，并把复用缓冲区交给异步任务

位置：`internal/p2p/stun_server.go:110-138`；默认入口：`cmd/relay/main.go:29`

非 relay UDP 数据报仍执行：

```go
go s.handlePacket(buf[:n], remoteAddr)
```

这里同时有两个问题：

1. 每个公网 UDP 包都能创建一个 goroutine，没有 worker 数量和队列上限。
2. `buf` 在下一次 `ReadFromUDP` 立即复用，异步 `handlePacket` 与读循环并发读写同一底层数组，既有数据竞态，也可能把 A 包解析成被 B 包覆盖后的内容。

此外 `cmd/relay` 的 `--stun` 默认仍为 `:3478`，并非“UDP 默认关闭”。在 UDP PPS、worker、状态容量和带宽限制尚未实现时，这会把未完成的 P0 路径默认暴露出来。

修复要求：改为固定 worker pool 和有界队列，任务拥有独立数据；补 per-IP/global PPS、队列满丢弃和采样日志；在 SR-002b 完成前把默认 `--stun` 改为空。

### P1-1：validator 与 relay 状态写入之间存在 TOCTOU，失效后仍可被旧包重建

位置：`internal/p2p/stun_server.go:251-264`、`internal/p2p/stun_server.go:318-327`

当前先在 `relayMu` 外调用 validator，之后才取得 `relayMu` 并创建 entry。以下顺序可以稳定违反“断连后同步失效”的不变量：

1. 旧 UDP 包调用 validator，得到 true，尚未取得 `relayMu`。
2. 控制面断连，将 `dataPlaneReady=false`，并完成 `InvalidateRelaySession`。
3. 旧 UDP 包随后取得 `relayMu`，用之前的 true 结果重新创建 relay entry。
4. 新 Help Join 后 validator 又变 true，旧 peer 状态继续跨代残留。

修复要求：把最终 validator 检查和 relay map 的创建/刷新放到同一个 `relayMu` 临界区内。当前控制面调用 STUN 前已释放 `sm.mu`，因此可以统一规定 `relayMu -> sm.RLock` 的锁顺序；或者使用不可复用的数据面 generation。必须补一条用屏障精确控制上述交错顺序的测试。

### P1-2：新增审计指纹没有接入生产日志，完整协助码仍被写出

位置：`internal/logger/audit.go:158-174`、`internal/relay/server.go:373`、`internal/relay/server.go:393`

`CodeFingerprint` 和 `MaskCode` 虽已新增，但生产调用仅在 Join 拒绝路径使用指纹。`LogCodeGenerated` 与 `LogSessionEstablished` 仍把原始 `code` 放进审计 Details，普通日志仍调用 `FormatCode(code)`；`FormatCode` 只是插入连字符，不是脱敏。

这意味着 SR-004 的凭据泄露路径完全保留。应让所有审计事件只记录 `code_fp`，普通日志只使用固定掩码，并增加端到端日志捕获测试，断言原始码不出现在审计文件、stdout/stderr 或错误输出中。

### P1-3：Join 激活失败不回滚已写入的 Help，并且读取 Share 存在数据竞态

位置：`internal/relay/server.go:421-439`、`internal/relay/session.go:168-202`

`JoinSession` 在锁内先执行 `session.Help = help`，返回后 `handleJoin` 在没有 `sm.mu` 的情况下读取 `session.Share`。它可与 `DisconnectClient`/`ReuseSession` 对 Share 的写入并发，构成真实数据竞态。

若 Share 在 Join 与 `ActivateDataPlane` 之间断开或换代，激活返回 false，但代码只调用 `rejectJoin`，没有清除刚写入的 `session.Help`。会话随后保留一个未成功 Join 的 Help，其他合法 Help 会得到 `ErrSessionHasHelper`；失败连接自身也无法再次 Join 该会话。

修复要求：让 Join 返回不可变快照或 expected IDs，禁止锁外读可变 session 字段；为激活失败提供按 expected Help identity 的幂等回滚，或将绑定与激活改为 SessionManager 内的事务状态转换。

### P1-4：ClientID 查找与复用仍非原子，复用后数据面可能永久不再激活

位置：`internal/relay/server.go:361-372`、`internal/relay/session.go:113-128`、`internal/relay/session.go:149-165`

`GetSessionByClientID` 和 `ReuseSession` 分两次加锁。两次调用之间，另一个注册或 cleanup 可以替换/删除映射；之后 `ReuseSession` 仍会修改已经脱离 map 的旧指针并向客户端返回不可 Join 的旧 code。两个相同 ClientID 并发注册时，后一个还会关闭前一个刚换入的连接，但前一个路径仍按成功继续执行。

另外，复用会把 `dataPlaneReady=false` 并清理 UDP entry，但当原 Help 仍在线时没有任何路径重新调用 `ActivateDataPlane` 或通知 Help 重建；该会话的数据面会一直保持 false，直到 Help 断开并重新 Join。

修复要求：实现单锁的 `ReuseSessionByClientID`，在锁内校验 map identity 并返回旧连接、sessionID、expected Share/Help IDs 等不可变结果；锁外关闭旧连接和失效 UDP。若 Help 仍在线，需要定义并实现重新激活/强制重连协议，不能留下永久 false 状态。

### P1-5：限流矩阵和容量保护仅实现了 Join 的两个桶

位置：`internal/relay/server.go:41-54`、`internal/relay/server.go:180-226`、`internal/relay/server.go:408-419`、`internal/p2p/stun_server.go:240-307`

当前可找到的新增限制只有 Join per-IP/global。以下定稿项没有生产实现：

- 每连接 Join token bucket（目前只有失败 5 次关闭）；
- create per-IP/global 速率桶；
- 活跃 session per-IP/global 数量上限；
- UDP per-IP/global PPS；
- UDP relay session 总量/per-IP 上限及计数释放；
- UDP per-session/global 字节桶，且应按 per-session 后 global 消费；
- 注册后控制消息的频率/幂等约束。

`DisableSourceIPLimits` 也只旁路 Join per-IP 桶；连接数限制仍在 `acquireConnSlot` 无条件执行，且 `cmd/relay` 没有把 SNAT/可信源 IP 配置映射到该字段。ELB SNAT 场景仍会把所有用户聚合到同一 per-IP 限制主体。

这不是测试缺口，而是生产调用点缺失。应按限流矩阵逐项实现并做容量恢复测试后再提交。

### P1-6：KeyedLimiter 声称 O(1) LRU，实际满表时每次新 key 都扫描全表

位置：`internal/ratelimit/ratelimit.go:95-117`、`internal/ratelimit/ratelimit.go:127-134`

实现没有 LRU 链表。表满时每个新 key 都调用 `evictIdleLocked` 遍历整个 map；若 8192 个 entry 尚未到 idleExpiry，攻击者每个新来源请求都会先做 8192 次检查再被拒绝。per-IP Join 检查又位于 global bucket 之前，因此该 CPU 成本不会先被全局桶封顶。

修复要求：按计划使用 map + 双向链表实现 O(1) 触碰和淘汰，并让“查找/触碰/本次 Allow/淘汰”具有一致原子语义。增加确定性操作计数测试，而不是只测试结果和容量。

### P2-1：新增测试没有覆盖本轮最关键的跨组件序列，CI benchmark 为空跑

位置：`internal/p2p/stun_server_relay_test.go:62-115`、`internal/relay/session_dataplane_test.go:28-111`、`.github/workflows/test.yml:30-36`

现有 UDP socket 测试只覆盖“恰好上限”和“上限+1”，没有覆盖更大数据报；数据面测试只分别调用 SessionManager 方法，没有启动带 validator 的 STUNServer，也没有覆盖 Invalidate 与在途 UDP 包交错、不同 IP 快速重连、旧 peer 不再收包、计数只减一次等验收序列。

CI 增加了 `-bench=.`，但 `internal/ratelimit` 当前没有任何 `Benchmark*` 函数。该命令会显示 PASS，却没有执行 benchmark，无法提供所称的观察数据。

建议补齐跨组件测试、真实 socket 快速重连测试以及实际 benchmark；CI 输出还应能证明 benchmark 确实被发现和运行。

## 验证结果

- `git diff --check`：通过。
- `go test ./... -count=1 -timeout 120s`：通过，包含现有 unit/e2e 测试。
- `go test -race ./internal/... -count=1 -timeout 180s`：**未执行**。当前 Windows 环境为 `CGO_ENABLED=0`，且没有可用的 `gcc/clang/cl`，Go 返回 `-race requires cgo`；需由新增的 Ubuntu CI race job 验证。
- `go test '-run=^$' '-bench=.' -benchmem ./internal/ratelimit`：命令通过，但输出没有任何 benchmark 条目，确认当前为空跑。

## 提交门槛

至少关闭全部 P0/P1，并补充能复现 P1-1、P1-3、P1-4 的并发/状态转换测试后再进入下一轮评审。UDP 在 SR-002 的 worker、PPS、状态和带宽限制完成前必须默认关闭。
