# 安全加固对抗式 Review 报告

**项目**: remote-assist-tool
**日期**: 2026-07-23
**Review 类型**: 对抗式安全审计（Adversarial Security Review）
**范围**: 安全加固改动（8 文件，1136 行新增，305 行删除）
**方法**: 8 个 Opus 智能体并行分析（47 万 token，19 分钟）

---

## 执行摘要

本次安全加固**显著提升了公网部署的安全基线**，引入了多层防御体系：
- ✅ 多维度限流（per-IP / per-connection / global）
- ✅ 状态机加固（每连接一次注册）
- ✅ 资源上限保护（连接数、会话数、UDP session）
- ✅ 审计日志指纹化（HMAC 不可逆）

对抗式分析发现 **1 个需要立即修复的中危漏洞** 和 **4 个需要加固的中危设计缺陷**。

### 核心发现
1. 🔴 **P0 - KeyedLimiter 缓存填充拒绝服务**（中危，立即修复）
2. 🟠 **P1 - Join 失败计数器可被重连绕过**（中危）
3. 🟠 **P1 - UDP relay 准入 TOCTOU 竞态窗口**（中危）
4. 🟡 **P2 - HMAC 指纹 48 位碰撞风险**（中危）
5. 🟡 **P2 - SNAT 场景 per-IP 限流旁路无替代**（中危）

**修复完成后可安全部署到公网环境。**

---

## 1. 高/中危发现详情

### 🔴 P0: KeyedLimiter 缓存填充拒绝服务

**严重性**: 中危
**CVSS**: 5.3 (AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L)
**位置**: `internal/ratelimit/ratelimit.go:108-119`

#### 漏洞描述

`limiterMaxKeys=8192` + 保守拒绝策略允许攻击者用伪造 IP 填满限流器缓存并保活（每 9 分钟一次请求），导致真实用户新 IP 因 "no idle candidate" 被拒绝。

**攻击成本**: 8192 个伪造 IP + 每 IP 每 9 分钟 1 次请求 ≈ 15 req/s 维持成本

**代码分析**:
```go
// internal/ratelimit/ratelimit.go:108-119
if len(kl.buckets) >= kl.maxKeys {
    oldest := kl.lru.Back()
    if oldest != nil {
        ob := oldest.Value.(*keyedBucket)
        if now.Sub(ob.lastSeen) > kl.idleExpiry {
            // 可淘汰 idle key
        }
    }
    return false  // 表满且无 idle key → 保守拒绝新 key
}
```

#### 修复方案

**立即（P0）**:
```go
// server.go 常量定义
const (
-   limiterMaxKeys = 8192
+   limiterMaxKeys = 65536  // 提升到 64K，内存成本 ~1 MB
)
```

**短期（P1）**:
```go
// ratelimit.go:115 概率性接纳
if oldest == nil || now.Sub(ob.lastSeen) <= kl.idleExpiry {
    // 表满且无 idle key，10% 概率接纳
    if rand.Intn(10) == 0 {
        kl.evictOldest()  // 强制淘汰
    } else {
        return false
    }
}
```

**中期（P2）**: 为 DisableSourceIPLimits 场景提供基于 X-Forwarded-For 的真实 IP 提取。

---

### 🟠 P1: Join 失败计数器重连绕过

**严重性**: 中危（Workflow 判定为高危，验证后降级）
**CVSS**: 5.3 (AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:L/A:N)
**位置**: `internal/relay/session.go:45`, `server.go:1106`

#### 漏洞描述

`joinFailures` 计数器绑定在 `ClientConn` 上，攻击者每 4 次失败后主动断开重连即可清零计数器，避免连接被关闭（maxJoinFailures=5）。

**Workflow 误判澄清**:
原报告称"配合僵尸网络绕过 per-IP 限流，用全局 burst(400) 持续爆破"。经验证：
- `joinLimiterPerIP` 是 `KeyedLimiter`，按 **IP 地址** 存储在 Server 级别
- 重连后 per-IP 限流器状态**不变**，仍然是 5 req/s + burst 20
- 以全局 200 req/s 爆破 36^10 协助码空间需 **570,000 年**

**实际影响**:
- 攻击者避免了 TLS 握手开销，但仍受限流约束
- 真正问题：缺少 **IP 级失败追踪**，同一 IP 可通过多连接（每连接 5 次）累计更多尝试
- 单 IP 同时 128 连接 × 5 次/连接 = 理论 640 次尝试（实际受限流约束到 ~100 次）

#### 修复方案

```go
// internal/relay/session.go 新增 IP 级追踪
type SessionManager struct {
    ...
    ipJoinFailures map[string]*ipFailureRecord
    ipFailureMu    sync.Mutex
}

type ipFailureRecord struct {
    count        int
    firstFailAt  time.Time
    blockedUntil time.Time
}

func (sm *SessionManager) RecordJoinFailure(ip, code string) {
    sm.ipFailureMu.Lock()
    defer sm.ipFailureMu.Unlock()

    rec := sm.ipJoinFailures[ip]
    if rec == nil {
        rec = &ipFailureRecord{firstFailAt: time.Now()}
        sm.ipJoinFailures[ip] = rec
    }

    rec.count++
    // 1 小时窗口内累计 50 次失败 → 封禁 1 小时
    if rec.count >= 50 && time.Since(rec.firstFailAt) < time.Hour {
        rec.blockedUntil = time.Now().Add(time.Hour)
    }
}

func (sm *SessionManager) IsIPBlocked(ip string) bool {
    sm.ipFailureMu.Lock()
    defer sm.ipFailureMu.Unlock()

    rec := sm.ipJoinFailures[ip]
    if rec == nil {
        return false
    }
    return time.Now().Before(rec.blockedUntil)
}

// server.go:handleJoin 入口检查
if s.sessions.IsIPBlocked(host) {
    return s.rejectJoin(client, code, "ip_blocked", true)
}
```

---

### 🟠 P1: UDP Relay 准入 TOCTOU 竞态

**严重性**: 中危
**CVSS**: 4.3 (AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:L/A:N)
**位置**: `internal/p2p/stun_server.go:478, 490`

#### 漏洞描述

锁外预检与锁内复检之间存在 TOCTOU 窗口：

```go
// stun_server.go:478 锁外预检
if s.dataSessionValidator == nil || !s.dataSessionValidator(sessionID) {
    s.logSampledInvalidPacket(addr, "inactive_data_session")
    return
}

s.relayMu.Lock()  // 370 行
// 490 行锁内复检
if !s.dataSessionValidator(sessionID) {
    s.relayMu.Unlock()
    return
}
```

**利用条件**: 在 478→370 窗口内，控制面将 `dataPlaneReady` 从 true 切换为 false（如 Help 断开），已通过预检的包仍能占用 relay session 槽位。

**影响**: 单次窗口内可注入初始状态，后续包被拒绝。无法持续占槽，但可能触发状态不一致。

#### 修复方案

```diff
  func (s *STUNServer) handleRelayPacket(data []byte, fromAddr *net.UDPAddr) {
      sessionID, payload, ok := parseRelayHeader(data)
      if !ok || len(payload) == 0 {
          s.logSampledInvalidPacket(fromAddr, "bad_relay_header")
          return
      }

-     // 删除锁外预检（365-368 行）
-     if s.dataSessionValidator == nil || !s.dataSessionValidator(sessionID) {
-         s.logSampledInvalidPacket(fromAddr, "inactive_data_session")
-         return
-     }

      s.relayMu.Lock()
+     defer s.relayMu.Unlock()

      // 只保留锁内的权威校验
      if !s.dataSessionValidator(sessionID) {
-         s.relayMu.Unlock()
+         s.logSampledInvalidPacket(fromAddr, "inactive_data_session_recheck")
          return
      }
      // ... 后续逻辑
  }
```

**权衡**: 去掉预检后所有 UDP 包都进锁，吞吐可能下降 ~10%，但消除状态不一致窗口。

---

### 🟡 P2: HMAC 审计指纹 48 位碰撞

**严重性**: 中危
**CVSS**: 4.3 (AV:N/AC:L/PR:L/UI:N/S:U/C:N/I:L/A:N)
**位置**: `internal/logger/audit.go:131`

#### 漏洞描述

`CodeFingerprint` 取 6 字节（48 位）HMAC 输出，生日攻击在 2^24（1600 万）次尝试后有 50% 碰撞概率。

**影响**: 攻击者无法反推协助码，但可以伪造日志关联（同一 `code_fp` 关联不同会话）。

#### 修复方案

```go
// internal/logger/audit.go:131
func CodeFingerprint(s string) string {
    ensureAuditKey()
    sum := hmacSum(auditKey, s)
-   return hex.EncodeToString(sum[:6])  // 48 位
+   return hex.EncodeToString(sum[:12]) // 96 位，碰撞概率 2^-48
}
```

**额外建议**:
1. 对高价值事件（`session_established`）记录完整 32 字节 HMAC 到 `details['code_hmac_full']`
2. 实现定期密钥轮换（每小时），缩短单密钥生命周期：
```go
var (
    auditKey       [32]byte
    auditKeyRotate time.Time
    auditKeyMu     sync.Mutex
)

func ensureAuditKey() {
    auditKeyMu.Lock()
    defer auditKeyMu.Unlock()
    if time.Since(auditKeyRotate) > time.Hour {
        rand.Read(auditKey[:])
        auditKeyRotate = time.Now()
    }
}
```

---

### 🟡 P2: SNAT 场景限流旁路无替代

**严重性**: 中危
**CVSS**: 5.3 (AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L)
**位置**: `internal/relay/server.go:100`

#### 漏洞描述

`DisableSourceIPLimits=true` 时，所有 per-IP 限流被旁路。SNAT 场景下单个公网 IP 可能承载数千用户，全局限流（200 req/s）可能被合法流量耗尽。

#### 修复方案

```go
// internal/relay/server.go:Config
type Config struct {
    ...
    DisableSourceIPLimits bool
+   TrustedProxyHeaders   []string // ["X-Forwarded-For", "X-Real-IP"]
}

// 新增真实 IP 提取函数
func extractRealIP(conn Conn, trustedHeaders []string) string {
    // 1. 尝试从 TLS 连接的 PROXY protocol 提取
    // 2. 或从 HTTP header 提取（需在 relay 层解析 HTTP）
    // 3. 验证 IP 格式
    // 4. 失败则回退到 conn.RemoteAddr()
    return realIP
}

// server.go:acquireConnSlot / handleJoin 前调用
host := extractRealIP(client.Conn, s.config.TrustedProxyHeaders)
```

**注意**: 需要确保 `TrustedProxyHeaders` 只在可信代理（如内部 LB）后启用，否则客户端可伪造 header。


---

## 2. ����֤��Ч�ķ�����13 �

### 2.1 ���ķ�������

1. **״̬����ÿ����һ��ע��** - ��Ч��server.go:413-414, 432-434 ���̶߳�ѭ�����м��
2. **������������ȫ�� 2000 + per-IP 128��** - ��Ч��server.go:243-254 ԭ�Ӳ���
3. **��Ծ�Ự���ޣ�per-IP 10 + global 5000��** - ��Ч��session.go:121-129 ���ڼ��
4. **����Ͱ������4 �� �� ˫�㣩** - ��Ч��Join/Create/Heartbeat/Data ��������
5. **UDP relay �ֽ�����** - ��Ч���� session �� global�����ȻỰ����
6. **��Ϣ��С���ƣ�4 MB + ��ʱ��** - ��Ч��scanner ���� + ��д��ʱ
7. **UDP ����С���ƣ�1466 �ֽڣ�** - ��Ч��max+1 �����ⳬ����
8. **Worker �أ�4 �̶� + 256 ���У�** - ��Ч��������ʱ����
9. **Help ����ȥ����5 �룩** - ��Ч��timer �ӳ�����
10. **�����滻��ʧЧ�� UDP entry** - ��Ч������ InvalidateRelaySession
11. **������־��1/1000��** - ������Ч���̶�ģʽ��Ԥ��
12. **UDP Relay �޷Ŵ�0.96����** - ��Ч��relay ͷ����
13. **���������Ͻ�** - ��Ч��sync.Once, atomic, closeOnce

---

## 3. ��Σ���֣�����Ľ���

### 3.1 ������־�̶�ģʽ��Ԥ��
- λ��: server.go:263, stun_server.go:114
- �޸�: ����������� rand.Intn(1000)==0

### 3.2 �Ự���ڵ���������ʬ�Ự��
- λ��: session.go:521-524
- �޸�: ����ǿ�ƹ���ʱ�� + ���ָ��

### 3.3 ʱ�ӻز�Ӱ����������
- λ��: ratelimit.go:47-54
- �޸�: ���� time.Since() �� monotonic clock

---

## 4. ��ظ澯���飨14 �

### �������ָ��
- Join ʧ���ʣ��� IP + reason��- �� IP > 10 ��/min
- Э���뱬��ģʽ - �� IP ������ͬ code_fp
- KeyedLimiter ʹ���� - >= 80% �澯
- HMAC ��ײ��� - ͬһ code_fp ��ͬ sessionID

### �����뽡��ָ��
- ��Ծ�Ự�� - �� IP �ӽ� maxPerIP
- ȫ���������ܾ��� - > 10% ���� 5 ����
- UDP relay session ���������쳣
- Worker ���б��Ͷ� p99 > 200

---

## 5. �޸����ȼ��빤����

| ���ȼ� | ���� | ������ | ʱ�� | Ӱ�� |
|--------|------|--------|------|------|
| P0 | KeyedLimiter ������� | С | 30���� | ��ʵ�û�DoS |
| P1 | Join ʧ�ܼ������ƹ� | �� | 4Сʱ | ���Ƴɱ����� |
| P1 | UDP relay TOCTOU | С | 1Сʱ | ״̬��һ�� |
| P2 | HMAC ָ�� 48 λ | С | 1Сʱ | ��־α�� |
| P2 | SNAT IP ��ȡ | �� | 2�� | ����ʧЧ |

�ܼ�: Լ 3-4 �������գ������ԣ�

---

## 6. ���ս���

���ΰ�ȫ�ӹ�**�����ߡ���ƺ���**��������ϵ������

**�ؼ�����**:
- ���������Ч������Դ�ľ��� DoS
- ״̬���ϸ��޲�����̬
- ���ָ�ƻ���Э���벻���ļ�¼
- UDP relay �޷Ŵ󣬲��߱� DRDoS ��ֵ

**��Ҫ�ӹ�**:
- KeyedLimiter ��������ƫ�ͣ�P0��
- IP ��ʧ��׷��ȱʧ��P1��
- UDP relay ׼�뾺̬���ڣ�P1��

**������**: �޸� P0+P1 �󼴿ɰ�ȫ���𵽹���������

---

**��������**: 2026-07-23
**�汾**: v1.0
**�´����**: ����� 1 ���»��ش�ܹ����ʱ
