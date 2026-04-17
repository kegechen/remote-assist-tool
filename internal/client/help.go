package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/version"
)

// HelpMode 协助模式
type HelpMode struct {
	client     *Client
	code       string
	listenAddr string
}

// NewHelpMode 创建协助模式
func NewHelpMode(cfg *Config, code, listenAddr string) *HelpMode {
	return &HelpMode{
		client:     NewClient(cfg),
		code:       normalizeCode(code),
		listenAddr: listenAddr,
	}
}

// Run 运行协助模式
func (h *HelpMode) Run() error {
	if err := h.client.Connect(); err != nil {
		return err
	}
	defer h.client.Close()

	req := &proto.JoinRequest{Code: h.code, Version: version.Info()}
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
		if resp.PeerVersion != "" {
			fmt.Printf("对端版本: %s\n", resp.PeerVersion)
		}
		fmt.Printf("会话ID: %s\n", resp.SessionID)
		fmt.Printf("本地监听: %s\n", h.listenAddr)

		// 尝试 P2P 直连
		p2pMode := p2p.ParseP2PMode(h.client.config.P2PMode)
		if p2pMode != p2p.P2PModeDisabled {
			tunnel, err := h.negotiateP2P(p2pMode, resp.SessionID)
			if err != nil {
				if p2pMode == p2p.P2PModeRequired {
					return fmt.Errorf("P2P 连接失败: %w", err)
				}
				log.Printf("P2P negotiation failed, falling back to relay: %v", err)
			}
			if tunnel != nil {
				fmt.Printf("\n在另一个终端运行:  ssh -p %s user@127.0.0.1\n", getPort(h.listenAddr))
				return h.handleTunnelP2P(tunnel)
			}
		}

		// relay 模式
		h.client.ResetDecoder() // P2P 协商超时会导致 json.Decoder 缓存错误
		fmt.Printf("\n在另一个终端运行:  ssh -p %s user@127.0.0.1\n", getPort(h.listenAddr))
		return h.handleTunnel()
	}

	return fmt.Errorf("unexpected response: %s", msg.Type)
}

// negotiateP2P 尝试 P2P 直连协商
func (h *HelpMode) negotiateP2P(mode p2p.P2PMode, sessionID string) (*p2p.UDPTunnel, error) {
	mgr := p2p.NewP2PManager(mode, h.client.config.STUNServer, h.client.config.BindIP)
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

// handleTunnelP2P 通过 P2P 隧道处理本地监听
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
				if _, err := conn.Write(buf[:n]); err != nil {
					connMu.Lock()
					currentConn = nil
					connMu.Unlock()
					conn.Close()
				}
			}
		}
	}()

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

// handleTunnel 处理隧道（支持多次SSH连接）
func (h *HelpMode) handleTunnel() error {
	// Start heartbeat to keep relay connection alive through NAT/firewalls
	h.client.StartHeartbeatLoop(30 * time.Second)

	listener, err := net.Listen("tcp", h.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer listener.Close()

	// Shared state: current local SSH connection
	var currentConn net.Conn
	var connMu sync.Mutex
	relayDone := make(chan error, 1)

	// Single long-lived goroutine: reads from relay, writes to current local SSH connection
	go func() {
		for {
			// Set read timeout to detect dead connections (reset after each successful read)
			h.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
			msg, err := h.client.ReadMessage()
			if err != nil {
				relayDone <- err
				listener.Close() // unblock Accept
				return
			}
			switch msg.Type {
			case proto.MsgTunnelData:
				var dataMsg proto.TunnelData
				if err := json.Unmarshal(msg.Payload, &dataMsg); err == nil {
					connMu.Lock()
					conn := currentConn
					connMu.Unlock()
					if conn != nil {
						conn.Write(dataMsg.Data)
					}
				}
			case proto.MsgError:
				var errMsg proto.ErrorMessage
				proto.DecodePayload(msg, &errMsg)
				relayDone <- fmt.Errorf("%s", errMsg.Message)
				listener.Close() // unblock Accept
				return
			}
		}
	}()

	// Accept loop: each iteration handles one SSH session
	for {
		fmt.Printf("\n等待本地SSH连接... (ssh -p %s user@127.0.0.1)\n", getPort(h.listenAddr))

		localConn, err := listener.Accept()
		if err != nil {
			select {
			case relayErr := <-relayDone:
				return relayErr
			default:
			}
			return err
		}

		fmt.Println("本地SSH连接已建立，开始转发流量...")

		connMu.Lock()
		currentConn = localConn
		connMu.Unlock()

		// Read from local SSH, send to relay (blocks until SSH closes)
		buf := make([]byte, 32*1024)
		for {
			n, err := localConn.Read(buf)
			if err != nil {
				break
			}
			dataMsg := &proto.TunnelData{Data: buf[:n]}
			if err := h.client.SendMessage(proto.MsgTunnelData, dataMsg); err != nil {
				break
			}
		}

		connMu.Lock()
		currentConn = nil
		connMu.Unlock()
		localConn.Close()

		// Check if relay is still alive
		select {
		case err := <-relayDone:
			return err
		default:
			fmt.Println("SSH会话已结束")
		}
	}
}

func normalizeCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '_' {
			return -1
		}
		return r
	}, code)
}

func getPort(addr string) string {
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[idx+1:]
	}
	return addr
}
