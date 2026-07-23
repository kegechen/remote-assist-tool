# Relay 安全整改计划定稿修改建议

## 使用方式

第五版主体不再重写。只需把本文三个修改项并入对应章节和测试清单；并入后，计划评审应转为 **通过，进入实现阶段**。后续发现的普通实现问题在代码评审中处理，不再继续扩大计划正文。

## 修改一：控制面主动重置 UDP relay 状态

本轮不引入新的 UDP 报文 generation，避免扩大客户端协议改造。采用“控制连接变化时主动失效 + 重建窗口内 validator 保持 false”的最小方案；per-datagram MAC/generation 仍归 SR-002b。

### 数据结构

`TunnelSession` 增加：

```go
dataPlaneReady bool
```

`IsActiveDataSession(id)` 返回 true 必须同时满足：

```text
session 存在
&& !session.closed
&& session.Share != nil
&& session.Help != nil
&& session.pendingHelpID == ""
&& session.dataPlaneReady
```

### STUNServer 接口

新增：

```go
func (s *STUNServer) InvalidateRelaySession(sessionID string)
```

该方法持 `relayMu` 查找当前 entry，并调用统一的 `deleteRelaySessionLocked(id, expected)`。不存在时 no-op。不得反向调用 SessionManager。

### 状态切换顺序

所有 SessionManager 方法只更新会话状态并返回需要失效的 sessionID；不得在持有 `sm.mu` 时调用 STUNServer。

Server 按以下顺序协调：

1. **Help 断开**：SessionManager 在 `sm.mu` 内立即设 `dataPlaneReady=false`、设置 pending 状态并返回 sessionID；释放锁后 Server 调用 `InvalidateRelaySession`。
2. **Share 断开**：在清 Share 前设 `dataPlaneReady=false`；释放锁后主动失效 UDP entry。
3. **Share ClientID 复用**：替换 Share 时先设 `dataPlaneReady=false` 并返回 sessionID；Server 释放 SessionManager 锁后失效旧 UDP entry。
4. **新 Help Join/替换 pending Help**：JoinSession 写入新 Help，但保持 `dataPlaneReady=false`；Server 先失效旧 UDP entry，再调用 `ActivateDataPlane(sessionID, expectedShareID, expectedHelpID)`。
5. **删除/过期/Close**：删除结果返回 sessionID；Server 在 SessionManager 解锁后失效 UDP entry。

`ActivateDataPlane` 必须在 `sm.mu` 内重新确认 session 仍存在、Share/Help ID 与 expected 一致、无 pending disconnect，然后才设 true。激活成功后，Server 才发送 JoinResponse/SessionReady，允许两端开始 P2P。

这样可以保证失效和新配对之间 validator 始终为 false，旧 Share 的后台 UDP 包也不能在清理窗口重新创建 relay entry。

### 接口返回值建议

避免 SessionManager 直接依赖 p2p 包，可扩展现有结果对象：

```go
type DataPlaneChange struct {
    SessionID string
    Reset     bool
}
```

`DisconnectClient`、`ReuseSessionByClientID`、`JoinSession` 和清理方法返回该信息，由 Server 统一调用 STUNServer。也可以使用各方法自己的结果类型，但必须保持“SessionManager 解锁后再调用 STUNServer”的边界。

### 验收

- 建立 A/B UDP relay 后，B 断开且期间不发送任何 UDP 包，新 Help C 从不同公网 IP 在 cleanup 前 Join：旧 entry 已删除，C 可立即成为新 peer。
- Share ClientID 从不同 IP 重连时同样重置旧 entry。
- reset 到 activate 之间从旧地址发送 UDP 包，不得创建 relay state。
- 旧 entry 的总量/per-IP 计数只减一次，新 entry 重新计数。
- `go test -race` 下无 `sm.mu`/`relayMu` 锁反转。

### 残留风险说明

激活后，知道当前 sessionID 的恶意旧参与方仍可能抢 peer 槽。这属于已明确接受、在 SR-002b MAC/generation 落地前通过“UDP 默认关闭”控制的残留风险，不在本轮继续扩展。

## 修改二：所有 Join 失败走统一出口

新增一个唯一失败处理函数。示意签名：

```go
func (s *Server) rejectJoin(client *ClientConn, internalReason error) (closeConn bool)
```

### 处理规则

1. `ErrCodeInvalid`、`ErrCodeExpired`、`ErrSessionHasHelper`、`ErrSessionNotFound`、per-IP 限流、全局限流全部进入 `rejectJoin`。
2. 每次进入均执行 `client.joinFailures++`。
3. 对外始终返回相同的 `JoinResponse`：

```go
JoinResponse{
    Success: false,
    Error:   "join failed",
}
```

不得把内部 error 文本、是否过期、是否已有 Help 等状态返回客户端。

4. 内部原因只写受采样保护的审计日志；不得记录完整协助码，可记录 `CodeFingerprint(normalizedCode)`、来源 IP 和内部 reason 枚举。
5. 第 5 次失败发送一次通用响应后返回 `closeConn=true`；读循环立即退出。
6. 畸形 payload 不调用 `rejectJoin`，按 `PROTO_DECODE` 直接关闭。
7. 真正 Join 成功后设置 `joinFailures=0`，再进入 help 状态。不能在尝试 Join 前因 `joinFailures==4` 预先关闭。

### 推荐执行顺序

```text
Decode payload
-> per-IP bucket（启用时）
-> global bucket
-> JoinSession
-> 失败统一 rejectJoin / 成功建立角色
```

per-IP bucket 拒绝后不扣全局令牌，但仍调用 `rejectJoin`。所有 rate-limit 分支不得逐请求写普通日志。

### 验收

- invalid、expired、already-has-helper、share-disconnected 的公开响应序列化结果完全相同。
- 连续 5 次内部 Join 失败，第 5 次后连接关闭。
- 连续 5 次 limiter 拒绝，第 5 次后同样关闭。
- 前 4 次失败、第 5 次使用合法 code 时 Join 成功，不被提前关闭。
- 审计日志只有指纹和内部 reason，不包含完整 code。

## 修改三：用 `max+1` 缓冲识别超长 UDP 数据报

不使用平台相关的 `MSG_TRUNC`，直接采用跨平台的 `max+1` 方案。

### 常量和读取

```go
const maxSessionIDLen = 64

const maxRelayDatagramSize = maxPacketSize + 2 + maxSessionIDLen

buf := make([]byte, maxRelayDatagramSize+1)
n, remoteAddr, err := conn.ReadFromUDP(buf)
if err != nil {
    // 按 done/关闭状态处理
}
if n > maxRelayDatagramSize {
    sampledInvalidPacketLog(remoteAddr, "datagram_too_large")
    continue
}
```

`ReadFromUDP` 保证 `n <= len(buf)`；使用 `max+1` 后，所有超过业务上限的数据报都会返回至少 `maxRelayDatagramSize+1` 个已复制字节，从而进入拒绝分支。仍不会为 64 KiB 数据报分配队列对象。

### Header 校验

`parseRelayHeader` 额外要求：

```go
sidLen := int(data[1])
if sidLen == 0 || sidLen > maxSessionIDLen {
    return "", nil, false
}
if len(data) < 2+sidLen {
    return "", nil, false
}
```

解析失败、空 payload 和超长 sessionID 均不得创建或刷新 relaySession，也不得逐包写日志。

### 验收

通过真实 UDP socket 测试，不只直接调用 parser：

- 大小恰好为 `maxRelayDatagramSize` 的合法包可进入处理队列。
- 大小为 `maxRelayDatagramSize+1` 的包被拒绝，不入队、不创建状态。
- 更大的数据报同样被拒绝，队列单包内存仍不超过业务上限。
- `sidLen=0`、`sidLen=65`、header 声明长度超过实际数据均被拒绝。

## 定稿结论

第五版并入以上三项后，计划层面的安全边界、并发顺序、资源上限和验收条件已经足够明确，应标记为 **计划评审通过** 并开始实现。

实现阶段重点审查：

- SessionManager 与 STUNServer 之间不得交叉持锁。
- 所有计数器增减必须走幂等单一路径。
- 所有限流拒绝必须避免逐请求日志和敏感状态泄露。
- UDP 继续默认关闭，直到 SR-002b 完成独立评审和实现。
