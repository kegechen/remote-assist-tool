# 安全对抗评审复核记录

日期：2026-07-23

复核范围：`docs/SECURITY_ADVERSARIAL_REVIEW.md` 列出的 5 个主要发现和 3 个低危建议，基于当前工作区代码逐项验证。

## 结论

| 发现 | 复核结论 | 处理 |
|---|---|---|
| KeyedLimiter 满表拒绝新 key | 成立，UDP 来源地址可伪造，固定提高容量不能根治 | 已修复 |
| Join 失败计数可被重连绕过 | 不构成报告所述旁路 | 不修改 |
| UDP relay 准入 TOCTOU | 不成立，报告忽略了锁内权威复检和控制面主动失效 | 不修改 |
| 48 bit HMAC 指纹碰撞空间偏小 | 作为审计完整性加固合理 | 已修复为 96 bit |
| SNAT 下关闭 per-IP 限流无替代 | 是已知部署取舍，但报告方案不适配当前协议 | 不修改 |
| 固定间隔日志采样可预测 | 不构成安全旁路 | 不修改 |
| 活跃会话超过协助码 TTL 后保留 | 是预期语义，不是僵尸会话泄漏 | 不修改 |
| 时钟回拨影响令牌桶 | 当前实现已使用 `time.Now` 的单调时钟分量 | 不修改 |

## 已修复项

### 1. KeyedLimiter 满表准入

原实现只允许淘汰超过 `idleExpiry` 的 LRU 桶。攻击者可填满并持续触碰所有 key，使新来源永久返回 `false`。该问题在 UDP 入口尤其现实，因为攻击者不需要完成 TCP 握手即可伪造来源地址。

报告建议把 `limiterMaxKeys` 从 8192 增至 65536，只会把首次填充成本提高 8 倍；建议的 10% 随机接纳也会刷新新桶 burst，并引入不可预测准入。

当前修复保持 8192 的内存上限：

- 有空闲过期 LRU 桶时，按原行为淘汰并创建满 burst 的新桶。
- 所有桶仍活跃时，复用 LRU 桶并换绑新 key。
- 活跃桶换绑时保留 `tokens` 和 `lastRefill`，防止攻击者利用 key churn 刷新 burst。
- 增加满表接纳、LRU 淘汰和不刷新 burst 的回归测试。

### 2. 审计指纹长度

`CodeFingerprint` 从 HMAC-SHA256 前 6 字节（48 bit、12 个十六进制字符）扩展到前 12 字节（96 bit、24 个十六进制字符），保留同一进程内跨事件关联能力。

未采纳完整 32 字节 HMAC 和每小时密钥轮换：完整输出没有必要；定时轮换会让跨轮换边界的同一协助码产生不同指纹，破坏现有审计关联。当前进程启动时生成、仅存内存、进程重启轮换的密钥生命周期更符合该用途。

## 未修改项依据

### Join 失败计数

`joinFailures` 是单连接快速断开机制，不是唯一爆破边界。重连不会重置 `Server` 级的 per-IP 令牌桶，也不会绕过全局令牌桶。报告建议的“单 IP 50 次后封禁一小时”会允许共享 NAT 后任一攻击者封禁同出口的全部合法用户，因此不采用。

### UDP relay TOCTOU

锁外校验只用于快速拒绝，`relayMu` 内的第二次 `dataSessionValidator` 调用才是准入线性化点。若控制面先失效，锁内复检返回 false；若数据包先通过复检，控制面在状态变更后调用 `InvalidateRelaySession`，并在取得 `relayMu` 后删除状态。删除锁外预检不能改变这一并发顺序，只会让所有无效 UDP 包竞争全局锁。

### SNAT 真实来源

Relay 使用原始 TCP/TLS 协议，不存在可直接读取的 HTTP `X-Forwarded-For` 或 `X-Real-IP` Header。盲目信任客户端可写 Header 会形成来源伪造。当前 `--trust-source-ip=false` 明确要求上游承担真实来源限流；若后续需要 Relay 自身恢复来源，应单独设计受信代理白名单和 PROXY protocol 支持。

### 三个低危建议

- 固定间隔采样不会影响限流准入，采样计数也不向远端客户端暴露；确定性计数便于根据 `sample_total` 估算拒绝量。
- `CodeTTL` 是新 Help 加入凭证的有效期，不是已建立远程会话的最长时长。过期后删除 `byCode`，已配对且仍有流量的连接继续工作是预期行为；2 分钟读空闲超时负责清理失活连接。
- `Bucket` 和 `KeyedLimiter` 的时间值均源自 `time.Now()`，同一进程内 `Time.Sub` 使用单调时钟分量，系统墙钟回拨不会导致报告描述的令牌异常。

## 验证

已通过：

```text
go test ./internal/ratelimit ./internal/logger ./internal/p2p ./internal/relay -count=1 -timeout 120s
go test ./internal/ratelimit ./internal/logger ./internal/p2p ./internal/relay -count=20 -timeout 120s
go test ./... -count=1 -timeout 180s
go vet ./...
git diff --check
```

Windows 当前 Go 环境为 `CGO_ENABLED=0`，且未发现可用 C 编译器，因此本机不能执行 `go test -race`。并发正确性由互斥锁覆盖和现有并发单元测试验证，但不把未执行的 race detector 记为通过。
