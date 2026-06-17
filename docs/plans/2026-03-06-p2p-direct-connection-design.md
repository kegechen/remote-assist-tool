# P2P 直连集成设计

## 背景

项目已有完整的 P2P 模块（STUN 客户端/服务器、P2PManager、UDPTunnel），但从未集成到主流程中。
当前所有 SSH 流量都通过 relay 中转，增加了服务器压力和延迟。

## 目标

在 share 和 help 配对成功后，尝试 UDP 打洞直连。成功则 SSH 流量走 P2P，失败则回退到 relay 中转。
对用户透明，默认行为为 auto 模式。

## 设计决策

| 决策 | 选择 | 原因 |
|------|------|------|
| P2P 通道加密 | 不加密 | SSH 本身已加密，UDP 传输 SSH 流量是安全的 |
| 默认模式 | auto | 能打洞就直连，不能就中转，用户无感知 |
| P2P 中断处理 | 断开重连 | 不做运行时回退，实现简单，用户重新 SSH 即可 |
| 整体方案 | 顺序尝试（A方案） | 先尝试 P2P，超时后回退 relay，数据路径始终单一 |

## 架构流程

```
会话建立（始终走 relay TCP）：
  Share → [注册] → Relay → [协助码]
  Help  → [加入] → Relay → [会话就绪] → Share

P2P 协商（配对成功后）：
  双方：STUN 发现公网地址 → 通过 relay 通告地址 → relay 转发对端地址 → UDP 打洞

数据传输（二选一）：
  P2P 成功：Help ←→ [UDP Tunnel] ←→ Share
  P2P 失败：Help ←→ [Relay TCP]  ←→ Share
```

## 具体改动

### 1. 统一打洞实现

- 删除 `tunnel.go` 中的 `TryHolePunching()` 函数（基于文本 "HELLO_P2P" 的独立实现）
- 仅保留 `P2PManager` 的 JSON 测试包方式，它是会话感知的

### 2. Client RelayConn 适配器

- 在 `client` 包中创建适配器，让 `*Client` 满足 `p2p.RelayConn` 接口
- `RelayConn` 接口：`SendMessage()`、`ReadMessage()`、`Close()`

### 3. share.go 改动

- `waitSessionReady()` 返回后，如果 P2P 模式非 disabled：
  1. 创建 P2PManager，设置 RelayConn 适配器
  2. 调用 `P2PManager.Start()` 开始 STUN 发现和地址通告
  3. 在 relay 消息读取循环中处理 `MsgPeerAddrReady`，转发给 P2PManager
  4. 等待 P2P 结果（通过 channel）
- P2P 成功：使用 `UDPTunnel` 做双向管道 ↔ 本地 SSH
- P2P 失败（auto 模式）：回退到现有 relay handleTunnel
- P2P 失败（required 模式）：返回错误

### 4. help.go 改动

- 收到 JoinResponse 后，如果 P2P 模式非 disabled：
  1. 同样创建 P2PManager 并启动
  2. 在 relay 消息循环中处理 `MsgPeerAddrReady`
  3. 等待 P2P 结果
- P2P 成功：监听本地端口，将 localConn ↔ UDPTunnel 双向管道
- P2P 失败：回退到现有 relay handleTunnel

### 5. P2P 隧道处理

两端都需要支持多次 SSH 连接：
- **Share 端**：UDPTunnel 读到数据时按需连接本地 SSH（复用现有 lazy connect 模式）
- **Help 端**：UDPTunnel 持续运行，Accept 循环接受多个 SSH 连接

### 6. P2PManager 改进

- `Start()` 方法需要接收一个 channel 来通知结果（P2P 就绪 or 超时回退）
- 移除 `TryHolePunching()` 独立函数
- `onP2PReady` 回调改为传递 `*UDPTunnel`（而非裸 `*net.UDPConn`）

### 7. CLI 默认值

- `--p2p` 默认值从 `"disabled"` 改为 `"auto"`

### 8. Relay 服务器

- 无需改动，已支持 `PeerAddrAdvertise` 消息转发

## 不做的事情

- P2P UDP 通道加密（SSH 本身已加密）
- P2P 中断时自动回退到 relay（断开重连即可）
- TURN 中转协议（超出范围，relay 已是中转方案）
