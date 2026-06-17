# P2P 直连集成实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 将已有但未集成的 P2P 模块（STUN、打洞、UDPTunnel）接入 share/help 主流程，实现自动 P2P 直连并支持 relay 回退。

**Architecture:** 会话建立后，双方通过 STUN 发现公网地址，经 relay 交换对端地址，UDP 打洞建立直连。成功则 SSH 流量走 UDPTunnel，失败回退 relay 中转。数据路径始终单一（P2P 或 relay）。

**Tech Stack:** Go 1.21+，标准库，零外部依赖。

**Design Doc:** `docs/plans/2026-03-06-p2p-direct-connection-design.md`

---

### Task 1: 清理 tunnel.go 中的冗余打洞实现

**Files:**
- Modify: `internal/p2p/tunnel.go:165-205`

**Step 1: 删除 TryHolePunching 函数**

删除 `internal/p2p/tunnel.go` 中的 `TryHolePunching()` 函数（第 165-205 行）。这个函数使用文本 "HELLO_P2P" 做打洞，与 `P2PManager` 的 JSON 测试包方式冲突。我们只保留 `P2PManager` 的实现。

删除后 `tunnel.go` 以 `RemoteAddr()` 函数结尾（第 163 行之后没有其他代码）。

**Step 2: 构建验证**

Run: `cd D:\src\remote-assist-tool && go build ./...`
Expected: 编译通过，无报错

**Step 3: 提交**

```bash
git add internal/p2p/tunnel.go
git commit -m "refactor: remove redundant TryHolePunching from tunnel.go"
```

---

### Task 2: 添加 ParseP2PMode 辅助函数

**Files:**
- Modify: `internal/p2p/manager.go`

**Step 1: 添加 ParseP2PMode 函数**

在 `internal/p2p/manager.go` 的 `P2PModeRequired` 常量定义后面添加：

```go
// ParseP2PMode 将字符串转换为 P2PMode
func ParseP2PMode(s string) P2PMode {
	switch s {
	case "auto":
		return P2PModeAuto
	case "required":
		return P2PModeRequired
	default:
		return P2PModeDisabled
	}
}
```

**Step 2: 构建验证**

Run: `cd D:\src\remote-assist-tool && go build ./...`
Expected: 编译通过

**Step 3: 提交**

```bash
git add internal/p2p/manager.go
git commit -m "feat: add ParseP2PMode helper function"
```

---

### Task 3: 重构 P2PManager 为 channel 驱动

将 P2PManager 从回调模式改为 channel 模式，`Start()` 返回结果 channel，调用方阻塞等待结果。

**Files:**
- Modify: `internal/p2p/manager.go`

**Step 1: 添加 P2PResult 类型，修改 P2PManager 结构体**

将 `P2PManager` 结构体中的 `onP2PReady func(*net.UDPConn)` 和 `onRelayReady func()` 替换为新字段。完整的新结构体：

```go
// P2PResult P2P 协商结果
type P2PResult struct {
	Tunnel *UDPTunnel // 非 nil 表示 P2P 成功
	Err    error      // 非 nil 表示 required 模式下失败
}

// P2PManager manages P2P connection attempts
type P2PManager struct {
	mode         P2PMode
	stunServer   string
	localConn    *net.UDPConn
	localAddr    *net.UDPAddr
	publicAddr   *net.UDPAddr
	peerInfo     *PeerInfo
	sessionID    string
	isShare      bool
	connected    bool
	connectedMu  sync.RWMutex
	relayConn    RelayConn
	resultCh     chan P2PResult
	stopChan     chan struct{}
	closeOnce    sync.Once
}
```

**Step 2: 删除 SetOnP2PReady 和 SetOnRelayReady 方法**

删除以下两个方法（约第 71-78 行）：

```go
// SetOnP2PReady sets the callback when P2P is ready
func (p *P2PManager) SetOnP2PReady(fn func(*net.UDPConn)) {
	p.onP2PReady = fn
}

// SetOnRelayReady sets the callback when falling back to relay
func (p *P2PManager) SetOnRelayReady(fn func()) {
	p.onRelayReady = fn
}
```

**Step 3: 修改 Start() 返回结果 channel**

```go
// Start starts the P2P manager and returns a result channel
func (p *P2PManager) Start(sessionID string, isShare bool) (<-chan P2PResult, error) {
	p.sessionID = sessionID
	p.isShare = isShare
	p.resultCh = make(chan P2PResult, 1)

	if p.mode == P2PModeDisabled {
		p.resultCh <- P2PResult{} // nil tunnel = use relay
		return p.resultCh, nil
	}

	// Create local UDP socket
	var err error
	p.localConn, err = net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
	p.localAddr = p.localConn.LocalAddr().(*net.UDPAddr)

	// Discover public address via STUN
	if p.stunServer != "" {
		p.publicAddr, err = DiscoverPublicAddr(p.stunServer)
		if err != nil {
			log.Printf("STUN discovery failed: %v, will try without", err)
		} else {
			log.Printf("Discovered public address: %v", p.publicAddr)
		}
	}

	// Advertise our address via relay
	if err := p.advertiseAddr(); err != nil {
		log.Printf("Failed to advertise address: %v", err)
	}

	// Start receiving
	go p.receiveLoop()

	// Start timeout for P2P attempt
	go p.p2pTimeout()

	return p.resultCh, nil
}
```

**Step 4: 修改 onP2PConnected 创建 UDPTunnel 并发送结果**

```go
func (p *P2PManager) onP2PConnected(addr *net.UDPAddr) {
	p.connectedMu.Lock()
	if p.connected {
		p.connectedMu.Unlock()
		return
	}
	p.connected = true
	p.connectedMu.Unlock()

	log.Printf("P2P connection established with %v", addr)

	// Notify relay that we're switching to P2P
	if p.relayConn != nil {
		p.relayConn.SendMessage(proto.MsgP2PConnected, map[string]string{
			"session_id": p.sessionID,
		})
	}

	tunnel := NewUDPTunnel(p.localConn, addr)
	p.resultCh <- P2PResult{Tunnel: tunnel}
}
```

**Step 5: 修改 p2pTimeout 发送结果**

```go
func (p *P2PManager) p2pTimeout() {
	timeout := 10 * time.Second
	if p.mode == P2PModeRequired {
		timeout = 30 * time.Second
	}

	select {
	case <-p.stopChan:
		return
	case <-time.After(timeout):
		p.connectedMu.RLock()
		connected := p.connected
		p.connectedMu.RUnlock()

		if !connected {
			log.Printf("P2P connection timed out")
			if p.mode == P2PModeAuto {
				p.resultCh <- P2PResult{} // nil tunnel = fallback to relay
			} else {
				p.resultCh <- P2PResult{Err: fmt.Errorf("P2P 连接超时")}
			}
		}
	}
}
```

**Step 6: 修改 Close() 防止重复关闭和 UDP 连接冲突**

```go
// Close closes the P2P manager
func (p *P2PManager) Close() {
	p.closeOnce.Do(func() {
		close(p.stopChan)
		// Don't close localConn if it was handed off to UDPTunnel
		p.connectedMu.RLock()
		connected := p.connected
		p.connectedMu.RUnlock()
		if !connected && p.localConn != nil {
			p.localConn.Close()
		}
	})
}
```

**Step 7: 构建验证**

Run: `cd D:\src\remote-assist-tool && go build ./...`
Expected: 编译通过

**Step 8: 提交**

```bash
git add internal/p2p/manager.go
git commit -m "refactor: P2PManager channel-based results with UDPTunnel"
```

---

### Task 4: 添加 Client.SetReadDeadline 和 isNetTimeout

**Files:**
- Modify: `internal/client/client.go`

**Step 1: 在 Client 类型中添加 SetReadDeadline 方法**

在 `client.go` 的 `IsClosed()` 方法后面添加：

```go
// SetReadDeadline 设置读取截止时间
func (c *Client) SetReadDeadline(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.SetReadDeadline(t)
	}
}
```

**Step 2: 添加 isNetTimeout 辅助函数**

在 `client.go` 文件末尾（`pipeConn` 函数后面）添加：

```go
// isNetTimeout 检查错误是否为网络超时
func isNetTimeout(err error) bool {
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout()
	}
	return false
}
```

**Step 3: 构建验证**

Run: `cd D:\src\remote-assist-tool && go build ./...`
Expected: 编译通过

**Step 4: 提交**

```bash
git add internal/client/client.go
git commit -m "feat: add Client.SetReadDeadline and isNetTimeout helper"
```

---

### Task 5: 集成 P2P 到 share.go

这是核心集成任务。需要修改 share.go 的 `waitSessionReady()` 返回 sessionID，添加 P2P 协商和 P2P 隧道处理逻辑，并在 `Run()` 中串联。

**Files:**
- Modify: `internal/client/share.go`

**Step 1: 修改 waitSessionReady 返回 sessionID**

将 `waitSessionReady()` 的签名和实现改为：

```go
// waitSessionReady 等待会话就绪，返回 sessionID
func (s *ShareMode) waitSessionReady() (string, error) {
	for {
		msg, err := s.client.ReadMessage()
		if err != nil {
			return "", err
		}

		switch msg.Type {
		case proto.MsgSessionReady:
			var ready proto.SessionReady
			if err := proto.DecodePayload(msg, &ready); err != nil {
				return "", err
			}
			fmt.Println("协助端已连接！")
			return ready.SessionID, nil
		case proto.MsgHeartbeat:
		case proto.MsgError:
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			return "", fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		default:
			log.Printf("Unexpected message: %s", msg.Type)
		}
	}
}
```

**Step 2: 添加 negotiateP2P 方法**

在 `waitSessionReady()` 后面添加：

```go
// negotiateP2P 尝试建立 P2P 直连
func (s *ShareMode) negotiateP2P(mode p2p.P2PMode, sessionID string) (*p2p.UDPTunnel, error) {
	mgr := p2p.NewP2PManager(mode, s.client.config.STUNServer)
	mgr.SetRelayConn(s.client)

	resultCh, err := mgr.Start(sessionID, true)
	if err != nil {
		return nil, err
	}

	// 先检查是否为 disabled 模式（立即返回结果）
	select {
	case result := <-resultCh:
		return result.Tunnel, result.Err
	default:
	}

	fmt.Println("正在尝试 P2P 直连...")

	// 设置协商超时：比 P2P 打洞超时稍长
	negotiationTimeout := 12 * time.Second
	if mode == p2p.P2PModeRequired {
		negotiationTimeout = 32 * time.Second
	}
	s.client.SetReadDeadline(time.Now().Add(negotiationTimeout))

	// 阶段1：从 relay 读取 PeerAddrReady
	peerReady := false
	for !peerReady {
		msg, err := s.client.ReadMessage()
		if err != nil {
			s.client.SetReadDeadline(time.Time{})
			if isNetTimeout(err) {
				mgr.Close()
				if mode == p2p.P2PModeRequired {
					return nil, fmt.Errorf("P2P 协商超时：对端未响应")
				}
				fmt.Println("P2P 协商超时，回退到中转模式")
				return nil, nil
			}
			mgr.Close()
			return nil, err
		}
		switch msg.Type {
		case proto.MsgPeerAddrReady:
			var ready proto.PeerAddrReady
			if err := proto.DecodePayload(msg, &ready); err == nil {
				mgr.HandlePeerAddrReady(&ready)
			}
			peerReady = true
		case proto.MsgHeartbeat:
			// ignore
		case proto.MsgError:
			s.client.SetReadDeadline(time.Time{})
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			mgr.Close()
			return nil, fmt.Errorf("server error: %s", errMsg.Message)
		}
	}

	// 清除 relay 读取超时
	s.client.SetReadDeadline(time.Time{})

	// 阶段2：等待打洞结果
	result := <-resultCh
	if result.Tunnel != nil {
		fmt.Println("P2P 直连已建立！")
	} else if result.Err == nil {
		fmt.Println("P2P 打洞超时，回退到中转模式")
		mgr.Close()
	} else {
		mgr.Close()
	}
	return result.Tunnel, result.Err
}
```

**Step 3: 添加 handleTunnelP2P 方法**

在 `negotiateP2P()` 后面添加。这个方法与 `handleTunnel()` 结构相同但数据经过 UDPTunnel 而非 relay：

```go
// handleTunnelP2P 通过 P2P 直连处理隧道（支持多次 SSH 连接）
func (s *ShareMode) handleTunnelP2P(tunnel *p2p.UDPTunnel) error {
	defer tunnel.Close()

	var sshConn net.Conn
	var connMu sync.Mutex

	connectSSH := func() net.Conn {
		connMu.Lock()
		defer connMu.Unlock()

		if sshConn != nil {
			return sshConn
		}

		conn, err := net.Dial("tcp", s.sshAddr)
		if err != nil {
			log.Printf("Failed to connect to local SSH: %v", err)
			return nil
		}
		sshConn = conn
		fmt.Println("已连接到本地SSH服务 (P2P直连)...")

		// SSH → UDPTunnel goroutine
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					break
				}
				if _, err := tunnel.Write(buf[:n]); err != nil {
					break
				}
			}
			connMu.Lock()
			if sshConn == conn {
				sshConn = nil
			}
			connMu.Unlock()
			conn.Close()
			fmt.Println("本地SSH连接已断开，等待新的SSH会话...")
		}()

		return conn
	}

	// 主循环：从 UDPTunnel 读取，写入本地 SSH
	buf := make([]byte, 32*1024)
	for {
		n, err := tunnel.Read(buf)
		if err != nil {
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			return err
		}
		if n == 0 {
			continue
		}

		conn := connectSSH()
		if conn != nil {
			if _, err := conn.Write(buf[:n]); err != nil {
				connMu.Lock()
				if sshConn == conn {
					sshConn = nil
				}
				connMu.Unlock()
				conn.Close()
			}
		}
	}
}
```

**Step 4: 修改 Run() 集成 P2P**

将 `Run()` 中 `waitSessionReady` 之后的流程改为：

```go
// Run 运行被协助模式
func (s *ShareMode) Run() (string, time.Time, error) {
	if err := s.client.Connect(); err != nil {
		return "", time.Time{}, err
	}
	defer s.client.Close()

	clientID, _ := GetOrCreateClientID()
	if err := s.client.SendMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{ClientID: clientID}); err != nil {
		return "", time.Time{}, err
	}

	msg, err := s.client.ReadMessage()
	if err != nil {
		return "", time.Time{}, err
	}

	if msg.Type == proto.MsgRegisterResponse {
		var resp proto.RegisterResponse
		if err := proto.DecodePayload(msg, &resp); err != nil {
			return "", time.Time{}, err
		}
		s.code = resp.Code
		s.expiresAt = time.Unix(resp.ExpiresAt, 0)
		fmt.Printf("\n协助码已生成: %s\n", formatCode(resp.Code))
		fmt.Printf("有效期至: %s\n\n", s.expiresAt.Local().Format("2006-01-02 15:04:05"))
		fmt.Println("等待协助端连接...")

		sessionID, err := s.waitSessionReady()
		if err != nil {
			return s.code, s.expiresAt, err
		}

		// 尝试 P2P 直连
		p2pMode := p2p.ParseP2PMode(s.client.config.P2PMode)
		if p2pMode != p2p.P2PModeDisabled {
			tunnel, err := s.negotiateP2P(p2pMode, sessionID)
			if err != nil && p2pMode == p2p.P2PModeRequired {
				return s.code, s.expiresAt, fmt.Errorf("P2P 连接失败: %w", err)
			}
			if tunnel != nil {
				fmt.Println("开始 P2P 直连转发SSH流量...")
				return s.code, s.expiresAt, s.handleTunnelP2P(tunnel)
			}
		}

		// relay 模式（P2P disabled 或 P2P 回退）
		fmt.Println("开始中转转发SSH流量...")
		return s.code, s.expiresAt, s.handleTunnel()
	}

	return s.code, s.expiresAt, fmt.Errorf("unexpected response: %s", msg.Type)
}
```

**Step 5: 添加必要的 import**

在 `share.go` 的 import 中添加：

```go
"github.com/remote-assist/tool/internal/p2p"
```

确保 import 列表完整（已有的保留）：

```go
import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)
```

**Step 6: 构建验证**

Run: `cd D:\src\remote-assist-tool && go build ./...`
Expected: 编译通过

**Step 7: 提交**

```bash
git add internal/client/share.go
git commit -m "feat: integrate P2P direct connection into share mode"
```

---

### Task 6: 集成 P2P 到 help.go

与 share.go 对称的改动。help 端在收到 JoinResponse 后尝试 P2P，成功则监听本地端口并通过 UDPTunnel 转发。

**Files:**
- Modify: `internal/client/help.go`

**Step 1: 添加 negotiateP2P 方法**

在 `handleTunnel()` 方法前添加（与 share.go 的逻辑几乎相同，但 `isShare=false`）：

```go
// negotiateP2P 尝试建立 P2P 直连
func (h *HelpMode) negotiateP2P(mode p2p.P2PMode, sessionID string) (*p2p.UDPTunnel, error) {
	mgr := p2p.NewP2PManager(mode, h.client.config.STUNServer)
	mgr.SetRelayConn(h.client)

	resultCh, err := mgr.Start(sessionID, false)
	if err != nil {
		return nil, err
	}

	select {
	case result := <-resultCh:
		return result.Tunnel, result.Err
	default:
	}

	fmt.Println("正在尝试 P2P 直连...")

	negotiationTimeout := 12 * time.Second
	if mode == p2p.P2PModeRequired {
		negotiationTimeout = 32 * time.Second
	}
	h.client.SetReadDeadline(time.Now().Add(negotiationTimeout))

	peerReady := false
	for !peerReady {
		msg, err := h.client.ReadMessage()
		if err != nil {
			h.client.SetReadDeadline(time.Time{})
			if isNetTimeout(err) {
				mgr.Close()
				if mode == p2p.P2PModeRequired {
					return nil, fmt.Errorf("P2P 协商超时：对端未响应")
				}
				fmt.Println("P2P 协商超时，回退到中转模式")
				return nil, nil
			}
			mgr.Close()
			return nil, err
		}
		switch msg.Type {
		case proto.MsgPeerAddrReady:
			var ready proto.PeerAddrReady
			if err := proto.DecodePayload(msg, &ready); err == nil {
				mgr.HandlePeerAddrReady(&ready)
			}
			peerReady = true
		case proto.MsgHeartbeat:
			// ignore
		case proto.MsgError:
			h.client.SetReadDeadline(time.Time{})
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			mgr.Close()
			return nil, fmt.Errorf("server error: %s", errMsg.Message)
		}
	}

	h.client.SetReadDeadline(time.Time{})

	result := <-resultCh
	if result.Tunnel != nil {
		fmt.Println("P2P 直连已建立！")
	} else if result.Err == nil {
		fmt.Println("P2P 打洞超时，回退到中转模式")
		mgr.Close()
	} else {
		mgr.Close()
	}
	return result.Tunnel, result.Err
}
```

**Step 2: 添加 handleTunnelP2P 方法**

在 `negotiateP2P()` 后面添加。结构与 `handleTunnel()` 相同但数据经过 UDPTunnel：

```go
// handleTunnelP2P 通过 P2P 直连处理隧道（支持多次 SSH 连接）
func (h *HelpMode) handleTunnelP2P(tunnel *p2p.UDPTunnel) error {
	defer tunnel.Close()

	listener, err := net.Listen("tcp", h.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer listener.Close()

	var currentConn net.Conn
	var connMu sync.Mutex
	tunnelDone := make(chan error, 1)

	// 从 UDPTunnel 读取，写入本地 SSH 连接
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tunnel.Read(buf)
			if err != nil {
				tunnelDone <- err
				listener.Close()
				return
			}
			if n == 0 {
				continue
			}
			connMu.Lock()
			conn := currentConn
			connMu.Unlock()
			if conn != nil {
				conn.Write(buf[:n])
			}
		}
	}()

	// Accept 循环
	for {
		fmt.Printf("\n等待本地SSH连接 (P2P直连)... (ssh -p %s user@127.0.0.1)\n", getPort(h.listenAddr))

		localConn, err := listener.Accept()
		if err != nil {
			select {
			case tunnelErr := <-tunnelDone:
				return tunnelErr
			default:
			}
			return err
		}

		fmt.Println("本地SSH连接已建立，P2P 直连转发中...")

		connMu.Lock()
		currentConn = localConn
		connMu.Unlock()

		// 从本地 SSH 读取，写入 UDPTunnel
		buf := make([]byte, 32*1024)
		for {
			n, err := localConn.Read(buf)
			if err != nil {
				break
			}
			if _, err := tunnel.Write(buf[:n]); err != nil {
				break
			}
		}

		connMu.Lock()
		currentConn = nil
		connMu.Unlock()
		localConn.Close()

		select {
		case err := <-tunnelDone:
			return err
		default:
			fmt.Println("SSH会话已结束 (P2P)")
		}
	}
}
```

**Step 3: 修改 Run() 集成 P2P**

将 `Run()` 中收到 JoinResponse 之后的流程改为：

```go
// Run 运行协助模式
func (h *HelpMode) Run() error {
	if err := h.client.Connect(); err != nil {
		return err
	}
	defer h.client.Close()

	req := &proto.JoinRequest{Code: h.code}
	if err := h.client.SendMessage(proto.MsgJoinRequest, req); err != nil {
		return err
	}

	msg, err := h.client.ReadMessage()
	if err != nil {
		return err
	}

	if msg.Type == proto.MsgJoinResponse {
		var resp proto.JoinResponse
		if err := proto.DecodePayload(msg, &resp); err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("failed to join: %s", resp.Error)
		}

		fmt.Println("已连接到被协助端！")
		fmt.Printf("会话ID: %s\n", resp.SessionID)
		fmt.Printf("本地监听: %s\n", h.listenAddr)

		// 尝试 P2P 直连
		p2pMode := p2p.ParseP2PMode(h.client.config.P2PMode)
		if p2pMode != p2p.P2PModeDisabled {
			tunnel, err := h.negotiateP2P(p2pMode, resp.SessionID)
			if err != nil && p2pMode == p2p.P2PModeRequired {
				return fmt.Errorf("P2P 连接失败: %w", err)
			}
			if tunnel != nil {
				fmt.Printf("\n在另一个终端运行:  ssh -p %s user@127.0.0.1\n", getPort(h.listenAddr))
				return h.handleTunnelP2P(tunnel)
			}
		}

		// relay 模式
		fmt.Printf("\n在另一个终端运行:  ssh -p %s user@127.0.0.1\n", getPort(h.listenAddr))
		return h.handleTunnel()
	}

	return fmt.Errorf("unexpected response: %s", msg.Type)
}
```

**Step 4: 添加必要的 import**

更新 `help.go` 的 import：

```go
import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)
```

**Step 5: 构建验证**

Run: `cd D:\src\remote-assist-tool && go build ./...`
Expected: 编译通过

**Step 6: 提交**

```bash
git add internal/client/help.go
git commit -m "feat: integrate P2P direct connection into help mode"
```

---

### Task 7: 修改 CLI 默认 P2P 模式

**Files:**
- Modify: `cmd/remote/main.go`

**Step 1: 修改 share 和 help 的 --p2p 默认值**

在 `runShare()` 中（约第 38 行），将：

```go
p2pMode := fs.String("p2p", "disabled", "P2P mode: disabled, auto, required")
```

改为：

```go
p2pMode := fs.String("p2p", "auto", "P2P mode: disabled, auto, required")
```

在 `runHelp()` 中（约第 85 行），同样修改：

```go
p2pMode := fs.String("p2p", "auto", "P2P mode: disabled, auto, required")
```

**Step 2: 构建验证**

Run: `cd D:\src\remote-assist-tool && go build ./...`
Expected: 编译通过

**Step 3: 提交**

```bash
git add cmd/remote/main.go
git commit -m "feat: change default P2P mode from disabled to auto"
```

---

### Task 8: 全量构建和测试

**Step 1: 运行所有测试**

Run: `cd D:\src\remote-assist-tool && go test ./...`
Expected: 所有测试通过

**Step 2: 构建两个二进制**

Run: `cd D:\src\remote-assist-tool && go build -o remote.exe ./cmd/remote && go build -o relay.exe ./cmd/relay`
Expected: 两个可执行文件生成成功

**Step 3: 验证 --help 输出**

Run: `./remote.exe share -h`
Expected: 显示 `--p2p` 默认值为 `auto`

Run: `./remote.exe help -h`
Expected: 显示 `--p2p` 默认值为 `auto`

**Step 4: 提交（如有修复）**

如果步骤 1-3 发现问题并修复了，提交修复。
