# Relay 来源 IP、限流与监控指南

本文说明公网 Relay 的 `--trust-source-ip`、限流配置、公共 STUN 行为和上线后的采样日志观察方法。所有限流配置在进程启动时加载，修改后需要重启 Relay。

## 1. `--trust-source-ip` 的选择

Relay 当前使用 TCP 连接的 `RemoteAddr()` 作为来源 IP，不解析 `X-Forwarded-For`，也不支持 PROXY protocol。

| 部署拓扑 | 参数 | 前置条件 |
|---|---:|---|
| 客户端直连 Relay | `--trust-source-ip=true` | Relay 看到真实客户端公网 IP |
| 四层 TCP 透传且不做 SNAT | `--trust-source-ip=true` | 抓包或日志确认后端看到真实 IP |
| ELB/NLB/反向代理做 SNAT | `--trust-source-ip=false` | 上游必须完成真实来源的连接数、速率和封禁控制 |

`false` 只旁路五类来源 IP 限制：连接数、会话数、create 速率、join 速率和 heartbeat 速率。per-connection 和 global 限制始终保留。

上线前从两个不同公网出口各建立一次连接，检查 Relay 的 `New connection from <ip>` 日志：

- 两个地址不同且与客户端出口一致，可以保持 `true`。
- 两个地址都显示为负载均衡器地址，必须使用 `false`，并先验收上游 per-IP 防护。
- 无法确认时不要直接设为 `false`；这会失去应用层来源 IP 隔离。

## 2. STUN 与 UDP relay

Relay 的 `--stun` 默认值为空，即不监听 UDP 3478。此时 TCP relay、协助码和 P2P 直连尝试仍可使用。

share/help 未显式设置 `--stun` 时，会先尝试 `Relay主机:3478`；失败后依次尝试内置的 Google/Cloudflare 公共 STUN 地址。因此关闭 Relay 自带 STUN 后，客户端仍能通过公共 STUN 发现公网地址并尝试直接打洞。

边界如下：

- 公共 STUN 只提供标准 Binding 地址发现，不支持本项目的自定义 UDP relay。
- P2P 直连成功时使用 UDP 直连；失败时 `--p2p=auto` 回退 TCP relay。
- 当前客户端内部仍复用一个地址表示 STUN discovery 和自定义 UDP relay。不要把公共 STUN 当作自定义 relay 部署；显式拆分两个地址属于后续协议改造。
- 只有确实需要自建 STUN/UDP relay，且已开放、防火墙和监控 UDP 3478 时，才配置 Relay `--stun=:3478`。
- UDP relay 的准入要同时满足两条：会话数据面已就绪，且 UDP 包的**来源 IP 属于该会话两端之一**（STUN 反射得到的公网地址、对端自报的私网地址、或 relay 观测到的 TCP 源 IP）。只比 IP 不比端口，对称 NAT 换端口仍然可用。
- 打洞包（`P2PTestPacket`）必须带协助码派生的 HMAC，且 MAC 绑定发送方是 share 还是 help。缺 MAC 或验不过一律丢弃并打一行日志。旧版客户端不发这个字段，因此与新版之间谈不成 P2P，`auto` 下回落 TCP relay。
- relay 上完成工具握手之后，P2P 隧道上再来的 `ToolHello` 一律忽略：合法的新版 help 只在隧道上探活并复用已协商的 key，放行会让注入方用自己的 nonce 改写 session key，把整条会话打到 `decrypt_failed`。
- 残留风险：与合法端共用同一出口 IP（同一 NAT 之后）的攻击者若掌握 sessionID，仍可能抢占槽位。彻底封堵需要控制面下发 relay-token 并改 relay 头部格式，属于后续协议改造。

## 3. 限流配置文件

默认值无需配置。查看完整生效基线：

```powershell
remote-assist-relay.exe --print-default-limits
```

使用 JSON 文件做局部覆盖：

```json
{
  "max_connections_total": 3000,
  "max_connections_per_ip": 192,
  "join_rate_global": 300,
  "join_burst_global": 600,
  "reject_audit_sample_every": 500,
  "udp": {
    "packets_global_rate": 30000,
    "packets_global_burst": 60000
  }
}
```

```powershell
remote-assist-relay.exe --limits-file C:\ProgramData\remote-assist\limits.json
```

也可以通过环境变量指定文件，适合 systemd、容器和部署平台：

```text
REMOTE_RELAY_LIMITS_FILE=/etc/remote-assist/limits.json
```

解析器会拒绝未知字段、多个 JSON 值以及显式的零/负数，避免字段拼写错误后静默降级。未写出的字段保留默认值。启动日志中的 `Relay limits: {...}` 是最终生效值。

## 4. TCP 默认值与依据

rate 单位为每秒事件数，burst 为令牌桶瞬时容量。

| JSON 字段 | 默认值 | 调整依据 |
|---|---:|---|
| `max_connections_total` | 2000 | goroutine、socket、内存和文件描述符容量 |
| `max_connections_per_ip` | 128 | 为企业 NAT/CGNAT 留余量，同时限制单来源耗尽 |
| `max_active_sessions_total` | 5000 | SessionManager 常驻状态和业务容量 |
| `max_active_sessions_per_ip` | 10 | 正常终端通常只有少量同时分享会话 |
| `max_join_failures` | 5 | 单连接协助码枚举上限 |
| `join_rate_per_ip` / `join_burst_per_ip` | 5 / 20 | 正常 Help 每次连接仅 Join 一次，burst 容纳重试 |
| `join_rate_global` / `join_burst_global` | 200 / 400 | 多来源攻击下的全局 CPU/审计保护 |
| `create_rate_per_ip` / `create_burst_per_ip` | 2 / 10 | 正常 Share 注册低频，允许短时重连 |
| `create_rate_global` / `create_burst_global` | 100 / 200 | 限制会话创建和随机码生成总速率 |
| `heartbeat_rate_per_ip` / `heartbeat_burst_per_ip` | 10 / 20 | 正常客户端约 30 秒一次，保留共享 NAT 余量 |
| `heartbeat_rate_global` / `heartbeat_burst_global` | 500 / 1000 | 限制全局心跳处理和响应写入 |
| `data_rate_per_connection` / `data_burst_per_connection` | 100 / 200 | Tunnel 消息数，不是字节数 |
| `data_rate_global` / `data_burst_global` | 5000 / 10000 | 限制全局消息调度和转发开销 |
| `tool_rate_kib_per_connection` / `tool_burst_kib_per_connection` | 16384 / 32768 | 工具通道 16 MiB/s、32 MiB burst；单位是 KiB/s |
| `tool_rate_kib_global` / `tool_burst_kib_global` | 131072 / 262144 | 全局 128 MiB/s、256 MiB burst |
| `control_rate_per_connection` / `control_burst_per_connection` | 2 / 5 | PeerAddr/P2PConnected 正常仅在协商时出现 |
| `control_rate_global` / `control_burst_global` | 1000 / 2000 | 限制全局控制消息和日志开销 |
| `limiter_max_keys` | 8192 | 限流器自身内存上限；满表时复用 LRU 桶且不刷新活跃桶 burst |
| `limiter_idle_seconds` | 600 | 10 分钟无访问后允许回收 key |
| `reject_audit_sample_every` | 1000 | 默认每 1000 次高频拒绝记录一条 |

不要只提高 per-IP 阈值而不检查 global 阈值。global 至少应覆盖预计并发来源的正常峰值，但仍必须低于主机压测得到的稳定容量。

### 工具通道为什么单独一组，且按字节计费

`MsgToolHello/HelloAck/Req/Resp/Stream/Cancel` 走 `tool_*` 这组限额，与 Tunnel data 分开：

- **按字节而非条数**：`exec stream=true` 是管道读到多少发多少（单帧上限 32 KiB），`cat` 一个大日志轻松超过 100 帧/秒。按条数限流会把一次完全正常的输出判成洪水。每帧另有 16 KiB 的最低计费，把帧率一并压住（16 MiB/s ÷ 16 KiB ≈ 1024 帧/秒）。
- **超限是节流，不是丢帧**：relay 读一条转一条，超速时读循环让路，TCP 窗口自然把发送端压慢，一个字节都不丢。工具通道的帧丢不起——payload 是 AEAD 的、每帧独立 nonce，少掉一帧不影响后续解密，两端谁都发现不了，用户拿到的是一份被悄悄挖空却仍标着成功的输出。
- **burst 必须装得下最大的单条消息**（4 MiB，relay 读循环的硬上限 `maxMessageSize`），否则这条帧永远等不到额度；`ValidateLimits` 会直接拒绝这种配置。
- 只有等待超过 2 秒仍拿不到额度才回 `RATE_LIMITED` 并断连。按默认限额走不到这条路，它是配置离谱时的兜底。

日志里 `Tool channel throttled` 每次调用只记一条（不按重试次数记），走独立的 `tool_throttle_sample_total`；`Tool channel rate limit exceeded past throttle window` 才是真正的拒绝，计入 `sample_total`，出现即说明限额配置有问题。

## 5. UDP 默认值与依据

字节速率按 bytes/s，`2097152` 即 2 MiB/s。

| `udp` 字段 | 默认值 | 调整依据 |
|---|---:|---|
| `worker_count` | 4 | STUN/relay 单包处理轻量，固定 worker 防 goroutine 洪水 |
| `task_queue_depth` | 256 | 有界背压，避免排队内存无界增长 |
| `packets_per_ip_rate` / `packets_per_ip_burst` | 1000 / 2000 | 单来源 UDP PPS 上限 |
| `packets_global_rate` / `packets_global_burst` | 20000 / 40000 | 主机 UDP 接收、分配与 GC 总预算 |
| `max_relay_sessions_total` | 5000 | UDP peer 状态总量 |
| `max_relay_sessions_per_ip` | 64 | 单来源可创建的 UDP 状态量 |
| `relay_bytes_per_session` / `relay_bytes_burst_session` | 2097152 / 4194304 | 单会话 2 MiB/s、4 MiB burst |
| `relay_bytes_global` / `relay_bytes_burst_global` | 20971520 / 41943040 | 全局 20 MiB/s、40 MiB burst |
| `limiter_max_keys` / `limiter_idle_seconds` | 8192 / 600 | UDP 来源桶容量和回收时间；满表不会永久拒绝新来源 |
| `invalid_log_sample_every` | 1000 | UDP 丢弃日志采样间隔 |

提高 UDP 阈值前必须做目标主机上的 PPS、带宽和丢包压测；不要根据链路标称带宽直接推导安全值。

## 6. 部署后监控与调参

采样日志包含：

```text
sample_total=<该计数器启动后的累计拒绝数>, sample_every=<当前采样间隔>
```

拒绝计数器分为三组：Join 审计拒绝、TCP 高频消息拒绝、UDP 无效/限流丢包。相邻两条同组日志的 `sample_total` 差值可估算期间拒绝总量；最后不足一个采样间隔的尾数不会立即写日志。P2P 协商信息使用独立的 `p2p_sample_total`，工具通道节流使用独立的 `tool_throttle_sample_total`，两者都不计入拒绝量——节流只是把发送端压慢，一帧都没丢，混进拒绝计数会让上面那个差值算错。

建议上线流程：

1. 保持默认阈值和 `sample_every=1000`，观察至少一个完整业务高峰。
2. 按 reason 分组统计每分钟采样日志、`sample_total` 增量和来源 IP 分布。
3. Join 的 `code_invalid` 持续增长通常表示枚举或客户端使用旧码；`rate_limited_*` 持续增长说明已触发容量保护。
4. `Data message rate limited`、`Control message rate limited` 在正常业务中应接近零；持续出现时先区分攻击、客户端热循环和真实容量不足。`Tool channel throttled` 偶发属正常（大文件传输/长输出会短暂触发背压），持续出现说明工具通道限额偏紧。
5. UDP 的 `pps_limited_*`、`relay_state_limited`、`relay_bytes_*` 持续增长时，同时检查主机 CPU、丢包、出口带宽和会话数。
6. 只有确认合法流量被误限后才提高对应 rate/burst；每次只改一组，并保留变更前后监控对比。

`reject_audit_sample_every` 只改变日志量，不改变准入阈值：

- 日志太多时增大，例如 `5000`。
- 需要短时精确诊断时可降到 `100`，极短受控窗口可设 `1`。
- 不要长期在公网攻击期间使用 `1`，否则会重新引入日志 I/O 放大。

建议告警初始条件：5 分钟内同组 `sample_total` 持续增长、出现 global 限流，或正常基线时段出现任何 `relay_state_limited`。积累一周基线后再按实际流量设定静态阈值。
