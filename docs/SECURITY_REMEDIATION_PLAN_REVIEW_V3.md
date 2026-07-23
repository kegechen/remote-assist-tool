# Relay 公网安全整改计划第三版评审

## 评审范围

- 计划源文件：`D:\claude-model\.claude-ifly\plans\dreamy-finding-parasol.md`
- 评审对象：第三版计划，重点复核第二轮 8 条意见的落实情况及新增限流矩阵。
- 评审边界：仅评审计划，不评审尚未完成的实现，不修改计划原文。

## 结论

第二轮 8 条意见均已在第三版中找到对应设计和验收项；其中 validator 不查协助码 TTL、删除幂等、ClientID 原子复用和 Join 三层限流的主路径已经闭环。

本轮结论仍为 **暂不通过**。新增发现 4 项 P1、3 项 P2。P1 主要集中在：SNAT 降级没有可执行机制、已注册攻击者仍可触发 O(n) 查找和日志放大、KeyedLimiter 在伪造源 churn 下的复杂度与并发语义未定义、UDP relay 每 IP 状态计数没有完整生命周期。

## 评审发现

### P1-1：SNAT 场景不会自动“退化到全局限流”

计划第 24、28-30、80-81、119 行规定 TCP 先经过 per-IP 桶，并称 ELB SNAT 时 per-IP 限流“下降级到全局”。但当前和计划中的限流器都直接使用 `conn.RemoteAddr()`；如果 ELB SNAT，所有用户会共享 ELB 的同一个 per-IP 桶，实际结果是所有用户共同受 5/s 的 create/join 阈值约束，而不是只受全局 50/s、200/s 阈值约束。现有 `maxConnsPerIP=128` 也会继续聚合所有用户。

这与 `SECURITY_RISK_ASSESSMENT.md:308-311` 的要求不一致：后端只能看到 ELB 地址时，应由 ELB/云防火墙完成公网 per-IP 限流，Relay 只做全局限流。

计划需要增加明确的实施分支，二选一：

1. 增加配置，可同时关闭连接数、create、join 三类应用层 per-IP 限制，仅保留全局限制，并要求上游限流已验收；或
2. 将“后端看到 SNAT 地址”定义为阻断部署条件，直到可信源 IP 解析落地。

验收还应覆盖：模拟所有连接来自同一 ELB 地址时，不会错误宣称已经自动降级。

### P1-2：注册后仍可无限触发 O(n) 会话扫描和普通日志

计划第 45-49 行只阻止 `new` 状态发送业务消息；但 Relay 注册不需要已有凭据，公网攻击者可以合法 register 成为 `share`，随后无限发送业务消息：

- `Tool*`、`TunnelData` 会调用 `FindPeer`，当前在 `internal/relay/session.go:383-396` 遍历全部 session。
- `PeerAddrAdvertise` 会调用 `UpdatePeerAddr`，当前在 `internal/relay/session.go:321-354` 遍历全部 session，并在 `internal/relay/server.go:425-432` 写普通日志。
- `P2PConnected` 当前每条消息都在 `internal/relay/server.go:295-296` 写日志。

因此“未注册连接不能触发 O(n)”只提高了一步门槛，没有消除 CPU/日志 DoS；会话创建桶也只限制 register，不限制注册成功后的消息速率。

建议在 SessionManager 增加 `byConnID map[string]*TunnelSession`，让 `FindPeer`、`UpdatePeerAddr`、`DisconnectClient` 都按连接 ID O(1) 定位，并在 create/join/reuse/delete 中统一维护。对 `PeerAddrAdvertise`、`P2PConnected` 等控制消息还应规定每连接次数/速率或幂等状态，重复消息日志必须采样。测试需覆盖“攻击者注册一次后高频发送控制消息”的 CPU 和日志上限。

### P1-3：KeyedLimiter 的 LRU 复杂度和并发准入语义未定义

计划第 24、33、40 行要求 key 表满后 LRU 淘汰，并允许 UDP 在全局 20,000 PPS 后进入 per-IP limiter。当前草稿 `internal/ratelimit/ratelimit.go:95-117` 在表满时调用淘汰逻辑，`internal/ratelimit/ratelimit.go:127-133` 通过遍历 map 清理 key。若按类似方式寻找“最久未见”条目，伪造源每包换 IP 时会形成 `PPS * maxKeys` 的扫描成本，限流器自身即可成为 CPU DoS 点。

此外，当前实现取得 bucket 指针后先释放 KeyedLimiter 锁，再调用 `Bucket.Allow()`。该 entry 若同时被 LRU 淘汰，同一 key 可以创建一个装满令牌的新 bucket；旧请求又能从已脱离 map 的 bucket 消费，导致 churn 下突破 per-IP burst。`-race` 不能发现这种逻辑竞态。

计划应明确：

- 使用 map + 双向链表实现 O(1) 查找、触碰和淘汰，或给出其他有界复杂度方案。
- 明确 `maxKeys`、`idleExpiry` 在 TCP/UDP 各维度的具体值和容量依据。
- key 的查找、LRU 触碰/淘汰与本次准入必须具有一致的原子语义。
- 增加满表下随机新 key 的复杂度基准，以及“同一 key 与持续淘汰并发时 burst 不被刷新”的确定性测试。

### P1-4：UDP relay 每 IP 状态计数缺少所有权和释放规则

计划第 71 行仅说明按 `fromAddr` 增加每 IP relay session 上限，没有定义一个双 peer session 计入哪个 IP、第二个 peer 是否计数、端口/IP 更新如何迁移计数，以及 TTL 清理、validator 失效、Close 时如何递减。

如果只增不在所有删除路径严格配对递减，某 IP 达到上限后会一直被拒；如果一个 session 对两个 peer 都计数，又只按创建者递减，则同样会漂移。当前测试第 112 行只覆盖“超上限拒绝”，没有验证清理后容量恢复。

计划应定义 relay session/peer 的计费主体，并新增类似 `deleteRelaySessionLocked` 的幂等删除路径，统一处理 TTL、失效 session、关闭及显式删除，同时维护总量和 per-IP 计数。测试必须覆盖双 peer 不同 IP、端口变化、重复删除和清理后重新准入。

### P2-1：UDP 令牌桶缺少 burst 和精确单位

限流矩阵第 30-31 行只给出 UDP PPS `20000/s` 和字节速率 `2MB/s`、`20MB/s`，但 `NewBucket` 同时需要 rate 与 burst；缺少 burst 无法直接实施，也无法判断允许多大的瞬时流量。`MB` 也未说明采用十进制还是 `MiB`。

建议为 UDP PPS、per-session bytes、global bytes 全部给出 rate、burst、单位和容量依据，例如统一用 bytes 与 `MiB`，并增加“单个最大 UDP 包可通过、超过 burst 的请求稳定拒绝、空闲补充不超过 burst”的测试。

### P2-2：worker pool 没有关闭和等待协议

计划第 68 行新增 64 个 worker 和有界 channel，但没有说明 `Close` 如何停止 worker、何时关闭 job channel、cleanup goroutine 如何立即退出，以及是否等待 goroutine 完成。当前 cleanup 在 `internal/p2p/stun_server.go:260-282` 最长要等一分钟 ticker 才观察到关闭；新增 worker 若一直阻塞收 channel，会在重复启停和测试中泄漏。

建议 STUNServer 增加 `done`/context 与 `sync.WaitGroup`，`Close` 只执行一次：关闭 UDP conn、通知 serve/cleanup/workers、等待退出。增加多次 Close、启动后立即 Close、队列满时 Close 以及 goroutine 数恢复测试。

### P2-3：审计密钥测试注入 API 仍可破坏“绝不空 key”不变量

计划第 85 行提供导出的 `SetAuditKey(k)`，但没有规定长度校验、nil/空切片拒绝、是否复制调用方切片，以及第二次调用行为。调用方在注入后修改原切片会造成数据竞态或运行中改变指纹；`sync.Once` 也使测试第 113 行的“异 key 异值”无法在同一进程通过该 API 直接完成。

建议生产代码不暴露可变 setter：将 HMAC 计算拆成接收 `[32]byte` 的纯函数供测试，生产路径使用 Once 初始化的不可变 `[32]byte`。如果保留 setter，则必须只接受 32 字节、进行防御性复制、拒绝 nil/空值，并明确定义重复调用。

## 已验证通过的上一轮整改

- `IsValidSessionID` 不再使用协助码 TTL 判断数据面有效性。
- `deleteSessionLocked` 增加 map identity 幂等门、timer 停止和条件删除别名映射。
- Join 明确每连接、per-IP、全局三层限制，TCP 使用 per-IP 后全局的消费顺序。
- `new` 状态完整拒绝非 register/join 消息；现有 Share 在 register 成功后才启动 heartbeat（`internal/client/share.go:121-135`），不会因此产生兼容性回归。
- 会话创建增加 per-IP 与全局速率桶。
- ClientID 查找和复用合并为 SessionManager 内单锁操作。
- UDP 增加 per-session 和全局字节桶。
- 审计密钥增加一次性随机初始化，随机源失败不降级为空 key。

## 下一版最低通过条件

1. 解决 SNAT 分支的可执行降级策略，并纳入连接数、create、join 三类 per-IP 限制。
2. 将已注册连接的 session 定位改为 O(1)，限制可重复控制消息和相关日志。
3. 明确 KeyedLimiter 的 O(1) LRU、容量参数和并发准入语义。
4. 明确 UDP relay 状态计数的所有权、幂等释放及容量恢复测试。
5. 补齐 UDP bucket burst、worker 关闭协议和审计 key 注入约束。
