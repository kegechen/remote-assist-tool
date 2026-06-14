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
| P2PTestPacket | Bidirectional | P2P 打洞测试包 |
| P2PConnected | Client → Relay | 报告 P2P 连接建立 |

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
