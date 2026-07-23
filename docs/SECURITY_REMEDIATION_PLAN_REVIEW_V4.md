# Relay 公网安全整改计划第四版评审

## 评审范围

- 计划源文件：`D:\claude-model\.claude-ifly\plans\dreamy-finding-parasol.md`
- 评审对象：第四版计划，重点复核第三轮 `4 P1 + 3 P2` 的落实情况。
- 评审边界：仅评审计划，不评审尚未完成的实现，不修改计划原文。

## 结论

第三轮 7 条意见均已进入第四版，并基本具备对应的设计、参数和测试项：SNAT 开关、O(1) `byConnID`、O(1) LRU、UDP relay 计费、UDP burst、worker 关闭协议和不可变审计密钥均已明确。

本轮结论仍为 **暂不通过**，新增发现 **3 项 P1、4 项 P2**。其中 P1 是可稳定触发的安全或可用性缺口；P2 是会导致实现分叉、无效验收或后续返工的设计问题。

## 评审发现

### P1-1：SNAT 开关仍漏掉 `maxSessionsPerIP`

计划第 36-43、105-109 行规定 `--trust-source-ip=false` 时旁路连接数、create 速率和 join 速率三类 per-IP 限制。但计划第 75、80 行仍保留 `SessionManager.perIP` 和 `maxSessionsPerIP=128`，且没有受该开关控制。

在 ELB SNAT 场景下，所有客户端仍会以 ELB 地址作为 `creatorIP`，共同占用 128 个会话数量配额。因此第四版仍没有完全实现“同一 ELB 地址只受全局约束”的验收目标。

计划应把以下四类限制统一纳入同一策略：

1. 并发连接数 `maxConnsPerIP`；
2. 活跃会话数 `maxSessionsPerIP`；
3. create per-IP 速率桶；
4. join per-IP 速率桶。

建议内部配置使用零值安全的负向字段，例如 `DisableSourceIPLimits bool`。如果直接增加 `TrustSourceIP bool`，`Config{}` 的零值为 false，会让测试、嵌入式 standalone relay 和其他未显式赋值的调用方意外关闭 per-IP 防护。命令行 `--trust-source-ip=false` 可映射为 `DisableSourceIPLimits=true`。

测试应同时断言 false 模式下同一地址可以超过 `maxSessionsPerIP`，但仍不能超过 `maxSessionsTotal`。

### P1-2：`IsValidSessionID` 仍把断开但保留的会话视为活跃

计划第 74 行规定带持久 ClientID 的会话在一端断开后保留到 TTL，以便重连；第 78 行的 UDP validator 却只检查 `sessions[id]` 存在且 `!closed`。因此以下会话仍会通过 UDP 校验：

- Share 已断开、等待 ClientID 重连的会话；
- Help 已断开且去抖 timer 已确认清除 Help 的会话；
- Help 正处于 `pendingHelpID` 去抖状态、实际控制连接已经断开的会话。

持有旧 sessionID 的已断开参与方仍可创建或维持 UDP relay 状态，这与计划第 97 行所称“活跃会话定位符”和风险评估要求的“绑定已认证 TCP 会话”不一致。

应把数据面资格定义为独立谓词，例如：session 在 map 中、`!closed`、`Share != nil`、`Help != nil` 且 Help 不处于 pending disconnect；仍然不检查协助码 TTL。建议命名为 `IsActiveDataSession`，避免把“存在”“协助码有效”“数据面活跃”三种语义混在一起。

测试至少覆盖：

- Share+Help 都在线且跨过 TTL：true；
- Share 断开但持久 session 被保留：false；
- Help timer 到期后：false；
- Help pending disconnect：false；
- 新 Help 重连成功后：true。

### P1-3：UDP 字节桶未明确消费顺序，可能由单会话耗尽全局额度

计划第 41、90 行只写“per-relay-session + 全局出口”两个 bucket，没有规定先后顺序。若实现沿用 UDP PPS 的“全局先行”，单个超过 2 MiB/s 的热 session 会先消耗 20 MiB/s 全局令牌，随后才被 session bucket 拒绝；被拒绝的流量仍可耗空全局额度并阻断其他正常 session。

字节出口限流应明确采用 **per-session -> global**：先拒绝超过单会话额度的流量，只有实际通过单会话准入的字节才扣全局出口额度。分布式多 session 再由全局桶封顶。

测试需增加两个 session 的公平性场景：session A 持续超过自身上限时，session B 在全局仍有容量的情况下可以正常转发；多个均未超单会话上限的 session 合计超过全局时，才由全局桶丢包。

### P2-1：“最大 UDP 包不超过 64 KiB”与现有协议和缓冲区不一致

计划第 90 行要求“单个最大 UDP 包（<=64 KiB）可过”。当前 UDP tunnel 的 `maxPacketSize=1400`（`internal/p2p/tunnel.go:21`），STUN/relay 接收缓冲区为 1500 字节（`internal/p2p/stun_server.go:78`），合法 relay 数据报应是约 1400 字节 payload 加 relay header，而不是 64 KiB。

如果实现保持 1500 字节缓冲，该验收无法通过；如果为满足计划把队列中的包扩大到 64 KiB，`queue=1024` 会允许约 64 MiB 的排队 payload，且会引入 IP 分片攻击面，属于未评估的行为变更。

计划应定义明确的 `maxRelayDatagramSize`，与 tunnel `maxPacketSize + relayHeader` 契约一致。建议读取时能识别并拒绝截断/超限包，不把截断数据交给 parser。测试应使用“最大合法 relay 数据报”，并覆盖超限包拒绝和队列内存上界。

### P2-2：`deleteRelaySessionLocked` 的签名与幂等门不一致

计划第 93 行定义 `deleteRelaySessionLocked(id)`，但幂等门写成 `if s.relaySessions[id]==session`，其中 `session` 不是参数，也没有说明由函数内部取得。

应在计划中选择一种明确契约：

- `deleteRelaySessionLocked(id string, expected *relaySession)`，只在 map 仍指向 expected 时删除；或
- `deleteRelaySessionLocked(id string) bool`，在锁内查当前 entry，存在则删除并返回 true，不存在则 no-op。

如果 cleanup 会先保存指针后延迟删除，应使用 expected identity 版本，避免旧清理动作误删同 ID 的新 entry。所有计数递减必须只发生在函数实际删除成功时。

### P2-3：审计指纹纯函数的返回类型和截断位置矛盾

计划第 115 行声明 `fingerprintWith(key [32]byte, s string) string`，第 117 行又写 `hex(fingerprintWith(auditKey,s)[:6])`。如果函数返回 string，这既不能直接传给 `hex.EncodeToString`，也容易误实现为先生成 hex 字符串再截前 6 个字符，最终只有 24 bit 指纹强度，而原设计要求是截取 HMAC 原始结果前 6 字节后编码，即 48 bit。

建议明确为以下二选一：

```go
func hmacSum(key [32]byte, s string) [32]byte
// CodeFingerprint: hex.EncodeToString(sum[:6])
```

或：

```go
func fingerprintWith(key [32]byte, s string) string
// 函数内部完成 HMAC、截取 6 个原始字节并 hex 编码，调用方直接返回该 string。
```

测试除稳定性外还应断言输出为 12 个十六进制字符，防止截断位置回归。

### P2-4：两个关键性能/生命周期验收不会被当前 CI 命令可靠执行

计划第 55、145 行使用 benchmark 验证 LRU 操作不随 `maxKeys` 线性增长，但第 129、147 行的 CI 只有 `go test -race ./internal/...`；Go benchmark 默认不会由 `go test` 执行。因此该验收目前只是人工描述，不是交付门禁。

计划第 88、144 行用 `runtime.NumGoroutine` 前后对比验证 worker 全部退出。该指标包含测试进程内其他后台 goroutine，容易受测试顺序、定时器和并行测试影响，可能误报或漏报。

建议：

- LRU 的 O(1) 性质用结构和确定性操作计数测试保证；如保留 benchmark，CI 增加单独的 `-bench` 步骤并将结果作为观察数据，而不是依赖脆弱的绝对耗时断言。
- worker 生命周期直接通过内部 WaitGroup/done 信号断言 `Close` 已等待所有自有 goroutine 退出；`NumGoroutine` 只能作为辅助诊断。

## 已验证通过的第三轮整改

- O(1) LRU 已明确为 map + 双向链表，并规定准入过程原子化。
- join/create/UDP KeyedLimiter 已给出 `maxKeys` 和 `idleExpiry`。
- `byConnID` 覆盖 FindPeer、UpdatePeerAddr、DisconnectClient，并纳入 create/join/reuse/delete 生命周期。
- 注册后控制消息增加每连接桶、幂等处理和日志采样。
- UDP relay 明确创建者 IP 计费、总量/per-IP 上限和统一删除路径。
- UDP PPS/字节桶已补充 burst 和 MiB 单位。
- STUN worker 已增加 done、WaitGroup 和幂等 Close 设计。
- 审计密钥取消可变 setter，改为不可变数组和纯函数测试入口。
- 现有 Share 在 register 成功后才启动 heartbeat（`internal/client/share.go:121-135`），`new` 状态禁 heartbeat 不会造成现有客户端兼容性回归。

## 下一版最低通过条件

1. `--trust-source-ip=false` 同时旁路 `maxSessionsPerIP`，并采用零值安全的内部配置语义。
2. UDP 数据面 validator 只接受两端控制连接实际在线的 session，同时继续允许活跃会话跨协助码 TTL。
3. UDP 字节桶明确采用 per-session 后 global，并增加双 session 公平性测试。
4. 将 UDP 最大报文契约改为与 1400 字节 tunnel 包和 relay header 一致。
5. 修正 relay 删除函数签名、HMAC helper 返回类型及性能/生命周期验收方式。
