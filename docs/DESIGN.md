# 远程命令行协助工具 - 概要设计

## 1. 系统架构

### 1.1 整体架构
```
┌─────────────┐     TLS 1.3     ┌──────────────┐     TLS 1.3     ┌─────────────┐
│  协助客户端  │ ◄─────────────► │  中转服务器   │ ◄─────────────► │ 被协助客户端 │
│ (mode=help) │                  │  (Relay)     │                  │(mode=share) │
└─────────────┘                  └──────────────┘                  └─────────────┘
       │                                                              │
       └────────────────────── SSH over Tunnel ─────────────────────┘
```

### 1.2 P2P 架构 (可选)
```
┌─────────────┐                    ┌──────────────┐                    ┌─────────────┐
│  协助客户端  │ ◄────────────────── │  STUN 服务器  │ ──────────────────► │ 被协助客户端 │
│ (mode=help) │    UDP 打洞        │   (:3478)     │    UDP 打洞        │(mode=share) │
└─────────────┘                    └──────────────┘                    └─────────────┘
       │                                                                      │
       └────────────────────── SSH over UDP (P2P) ──────────────────────────┘
```

### 1.3 组件说明

| 组件 | 语言/框架 | 端口 | 说明 |
|------|----------|------|------|
| relay-server | Go | 8443 | 中转服务器，管理会话和协助码 |
| stun-server | Go | 3478 | STUN 服务器，用于 P2P 公网地址发现 |
| remote-cli | Go | - | **统一客户端**，通过 mode 参数切换身份 |

---

## 2. 目录结构

```
remote-assist-tool/
├── cmd/
│   ├── relay/              # 中转服务器入口
│   │   └── main.go
│   └── remote/             # 统一客户端入口
│       └── main.go
├── internal/
│   ├── relay/              # 中转服务器核心逻辑
│   │   ├── server.go       # TLS服务器
│   │   ├── session.go      # 会话管理
│   │   ├── code.go         # 协助码生成与验证
│   │   └── tunnel.go       # 隧道转发
│   ├── client/             # 客户端逻辑
│   │   ├── client.go       # 基础客户端
│   │   ├── clientid.go     # 持久化客户端ID
│   │   ├── share.go        # 被协助模式 (share)
│   │   └── help.go         # 协助模式 (help)
│   ├── p2p/                # P2P 相关
│   │   ├── stun.go         # STUN 协议实现
│   │   ├── stun_server.go  # STUN 服务器
│   │   ├── manager.go      # P2P 管理器
│   │   └── tunnel.go       # UDP 隧道
│   ├── crypto/             # 加密相关
│   │   └── tls.go          # TLS配置
│   ├── proto/              # 协议定义
│   │   └── message.go      # 消息结构
│   └── logger/             # 日志与审计
│       └── audit.go
├── pkg/
│   └── config/             # 配置加载
├── certs/                  # TLS证书（开发用）
├── go.mod
├── go.sum
└── README.md
```

---

## 3. 协议设计

### 3.1 消息类型

| 类型 | 方向 | 说明 |
|------|------|------|
| RegisterRequest | Share → Relay | 被协助端注册 |
| RegisterResponse | Relay → Share | 返回协助码 |
| JoinRequest | Help → Relay | 协助端使用协助码加入 |
| JoinResponse | Relay → Help | 加入结果 |
| TunnelData | Bidirectional | 隧道数据透传 |
| Heartbeat | Bidirectional | 心跳保活 |
| PeerAddrAdvertise | Client → Relay | 通告 P2P 地址 |
| PeerAddrReady | Relay → Client | 对等端地址就绪 |
| P2PTestPacket | Bidirectional | P2P 打洞测试包（含协助码派生的 HMAC，见 `internal/proto/punch.go`）|
| P2PConnected | Client → Relay | 报告 P2P 连接建立 |

### 3.1.1 工具通道协议版本

`proto.ToolProtocolVersion` 当前为 `"2"`，`SupportedToolVersions` 为 `["2", "1"]`（降序）。
已发布版本的分界线：`0.0.1`~`0.0.9` 是 v1，`1.0.0` 起是 v2。

**协商而非比对。** ToolHello 带两个版本字段：

- `versions`：本端支持的全部版本，真正的协商依据。旧版不认识它，会忽略。
- `version`：兼容锚点。0.0.x 的 share 只看这个字段且做严格相等比对，所以开了兼容模式时
  这里填 `"1"` 骗过它的比对，默认则填 `"2"` 让它明确拒绝。同 TLS 1.3 的 `legacy_version` 手法。

`HelloAck.version` 承载**选定**的版本（0.0.x 恰好填的就是它唯一支持的 `"1"`，语义一致）。
发起方不能照单全收这个选择：`proto.InterpretHelloAck` 会再用本端的 `--min-proto` 校验一遍，
否则一条伪造的 `accept:true + version:"1"` 就能单方面把发起方拽进 v1 会话。

版本同时是会话密钥 HKDF 的 `info` 串（`rat-tool-v<协商版本>`），所以**必须在派生密钥之前
定下来**，事后改不了。打洞密钥的 `info` 则是与工具协议解耦的固定串 `rat-p2p-punch-v2`
（`proto.punchKeyInfo`）：打洞发生在工具握手之前，那时拿不到协商结果。

**协商到 v1 时两端都主动停掉 P2P**（share 侧 `handleRelayToolHello` →
`refuseP2PForLegacyPeer`，help 侧 `help_bootstrap.go` 不启动 `upgradeToP2P`）。
不能只改一端：两个方向各对应一种新旧组合。原因是打洞认证**单向生效** —— 本端会拒绝
0.0.x 不带 MAC 的包，但 0.0.x 只比对 `session_id`、不认识 `mac` 字段，会接受本端的包并
单方面认定 P2P 已通，把流量送进一条本端没建起来的隧道，形成只有它以为成立的状态。
这比"没有 P2P"糟得多，是静默黑洞。不发包才能让两端状态一致。

保证的边界要说准：新端此后不发任何打洞包、也不会把 daemon 切到隧道，所以黑洞不会发生。
但 share 侧的**地址通告可能已经发出去了** —— `launchP2PUpgrade` 在 `SessionReady` 之后、
`ToolHello` 之前就跑了，而 `advertiseAddr` 是在 `mgr.Start()` 内部调的，那时 `p2pMgr` 还没
attach，`endP2PSession` 也就无从关闭。旧对端因此仍会收到 `PeerAddrReady` 并自行打洞，
直到它自己超时。想彻底免掉就得推迟 P2P 启动，但握手到达前分不清这是工具会话还是 SSH
会话，推迟会让 SSH 的 P2P 一起失效，代价更大。

`--p2p=required` 下"对端太旧"按 P2P 失败处理，且在**握手阶段**就拒绝（`handleRelayToolHello`
回 `Accept:false` 并附理由），而不是先 Accept 再关连接：后者在对端那里只表现为
"握手成功 → tunnel_lost → 重连"的无理由循环，真正的原因只印在 share 本机。回一条带理由的
拒绝才能把话送到对端终端——0.0.x 会把 `ErrorMsg` 原样打印。两端对 `--p2p required` 的
承诺因此一致：help 硬失败，share 拒绝握手。

`InterpretHelloAck` 还多守一道：应答方的 `Versions` 是它「我支持什么」的证词。若其中存在
双方都支持、且比 `Version` 更新的版本，说明这次降级没有正当理由——发起方的提议很可能
被中间人改写过（把 `{"version":"1"}` 塞进去并删掉 `versions`，应答方就会「合法地」谈出 v1）。
这堵的是 `--min-proto=1` 打开后剩下的最后一条降级路径；真正的 0.0.x 不发 `Versions`，
不受影响。

**默认不降级**（`DefaultMinProto == ToolProtocolVersion`）。允许自动降级的话，不可信的
relay 只要删掉 `versions` 字段就能把两个 v2 端打回 v1，而降级成功后双方都不再做
transcript 绑定，事后无从察觉。放宽必须由用户显式指定 `--min-proto=1`，且**只能加在新版本
那一端**（旧版没有这个旗标）——拒绝方恰好就是那一端，所以提示直接指向本机。

v1/v2 的行为差异集中在 `proto.Session` 上（密钥 + 协商版本一起传递、一起原子替换）。
它的零值是 fail-safe 的：`Version == ""` 按最高版本解释，因此漏传版本的后果是"连不上"，
而不是"静默按无认证的 v1 跑"。

v2 相对 v1 的三处变更：

1. **AAD**：`tool_req` / `tool_resp` / `tool_stream` 的 AEAD 带附加认证数据，把外层明文字段
   （`tool`、`id`、`deadline_ms`、`ok`、`error_code`、`error_msg`、`seq`、`stream`、`fin`）
   绑进密文，见 `internal/proto/aad.go`。三个方向各有自己的标签，请求的密文也无法当成响应
   或流帧重放。响应方向额外要求「每条都加封」（结果为空的错误响应、以及 inbound 缓冲满时
   回的 `server_busy` 都封一个 `{}`），接收侧先验真再看 `ok` —— 否则清空 `result` 就能把
   一次成功变成「空结果 + 成功」。流帧同理：握手后不存在合法的空帧（`AEADSeal` 对空 data
   也产出非空密文），所以解不开的帧一律计入缺帧，且必须**先验真再记 seq** —— 否则
   丢掉真帧 N、补一条 `{seq:N, data:空}` 就能抹平空洞，把被挖空的输出伪装成完整输出。
2. **握手后 args 必须是密文**：包括无参调用（host 封 `{}`）。判据从「有 args 才解密」
   改为「没有合法密文一律拒绝」，且拒绝发生在 `Registry.Dispatch` 之前。
3. **抗重放**：接收侧按调用 ID 做 1024 位滑动窗口去重（`internal/agent/replay.go`），
   窗口每把 key 一份 —— 重新握手（换 key）时重置，P2P 热升级（同 key 换通道）时保留。

与 v1 对端通话时（`--min-proto=1`），以上三项都要按 v1 的规矩关掉，且**收发两侧必须对称**：
AAD 传 `nil`、空 args 不加封、空 result 不加封、不做抗重放。任一侧记错，协商照样成功，
然后每条请求以 `decrypt_failed` 收场 —— 正是版本协商本该避免的失败形态。这些分支由
`tests/proto_compat_test.go`（Bridge 与 Daemon 真实对接）和 `internal/agent/legacy_v1_test.go`
成对钉住（每条 v1 断言都配一条 v2 反向断言，防止"顺手统一"掉某个分支时测试仍然全绿）。

版本不匹配时在握手阶段就收到带升级指引的拒绝，而不是逐条请求的 `decrypt_failed`。

### 3.2 协助码规则
- 字符集: `ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789` (排除 I, i, L, l, O, o, 0, 1)
- 长度: 10 位
- 格式: `XXXX-XXXXXX` (带短横线分隔，输入时可忽略)
- 有效期: 默认 30 分钟，可配置

---

## 4. 核心流程

### 4.1 被协助端 (Share 模式)
```
1. remote-cli share --server relay.example.com:8443
2. 连接 relay-server (TLS 1.3)
3. 发送 RegisterRequest
4. 接收协助码并显示给用户
5. 等待 relay 通知有 helper 加入
6. 建立隧道，开始转发数据到本地 SSH (127.0.0.1:22)
```

### 4.2 协助端 (Help 模式)
```
1. remote-cli help --server relay.example.com:8443 --code <协助码>
2. 连接 relay-server (TLS 1.3)
3. 发送 JoinRequest
4. 验证通过后建立隧道
5. 本地监听端口 (默认 2222)
6. 用户 ssh -p 2222 127.0.0.1 即可连接 target
```

### 4.3 中转服务器 (Relay) 流程
```
1. 监听 TLS 端口
2. 接收 Share 端注册，生成协助码，保存会话
3. 接收 Help 端加入请求，验证协助码
4. 匹配 Share 和 Help，建立双向转发
5. 记录所有操作日志
```

---

## 5. 安全设计

| 层次 | 措施 |
|------|------|
| 传输 | TLS 1.3, AES-256-GCM |
| 协助码 | 10位随机，防止暴力破解 |
| 日志 | 完整审计日志 |
| 配置 | 禁用弱加密套件 |

---

## 6. CLI 使用方式

```bash
# 启动中转服务器
relay --listen :8443 --cert certs/server.crt --key certs/server.key

# 被协助端：分享SSH访问
remote-cli share --server relay.example.com:8443

# 协助端：使用协助码连接
remote-cli help --server relay.example.com:8443 --code ABCD-EFGHIJ

# 然后在另一个终端：
ssh -p 2222 user@127.0.0.1
```
