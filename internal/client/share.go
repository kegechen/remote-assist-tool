package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/version"
)

// ErrPeerDisconnected 协助端断开连接（可恢复）
var ErrPeerDisconnected = errors.New("peer disconnected")

// ShareMode 被协助模式
type ShareMode struct {
	client    *Client
	sshAddr   string
	code      string
	expiresAt time.Time
}

// NewShareMode 创建被协助模式
func NewShareMode(cfg *Config, sshAddr string) *ShareMode {
	return &ShareMode{
		client:  NewClient(cfg),
		sshAddr: sshAddr,
	}
}

// Run 运行被协助模式，协助端断开时自动等待新连接
func (s *ShareMode) Run() (string, time.Time, error) {
	if err := s.client.Connect(); err != nil {
		return "", time.Time{}, err
	}
	defer s.client.Close()

	if err := s.register(); err != nil {
		return s.code, s.expiresAt, err
	}

	for {
		tunnelErr := s.waitAndHandleTunnel()
		if tunnelErr == nil {
			return s.code, s.expiresAt, nil
		}

		isPeerDisconnect := errors.Is(tunnelErr, ErrPeerDisconnected)
		codeExpired := time.Now().After(s.expiresAt)

		// 非对端断开 + 协助码已过期 → 不可恢复
		if !isPeerDisconnect && codeExpired {
			return s.code, s.expiresAt, tunnelErr
		}

		// 对端断开 + 协助码仍有效 → 在当前连接上等待新的协助端
		if isPeerDisconnect && !codeExpired {
			fmt.Printf("\n协助端已断开连接，协助码仍有效: %s\n", formatCode(s.code))
			fmt.Println("等待新的协助端连接...")
			continue
		}

		// 其余情况需要重连：协助码过期，或连接异常但码有效
		if codeExpired {
			fmt.Println("\n协助端已断开连接，协助码已过期，正在获取新协助码...")
		} else {
			fmt.Printf("\n连接中断，协助码仍有效: %s，正在重连...\n", formatCode(s.code))
		}
		s.client.Close()
		if err := s.client.Connect(); err != nil {
			return s.code, s.expiresAt, err
		}
		if err := s.register(); err != nil {
			return s.code, s.expiresAt, err
		}
	}
}

// register 向 relay 注册并获取协助码
func (s *ShareMode) register() error {
	clientID, _ := GetOrCreateClientID()
	if err := s.client.SendMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{ClientID: clientID, Version: version.Info()}); err != nil {
		return err
	}

	msg, err := s.client.ReadMessage()
	if err != nil {
		return err
	}

	if msg.Type != proto.MsgRegisterResponse {
		return fmt.Errorf("unexpected response: %s", msg.Type)
	}

	var resp proto.RegisterResponse
	if err := proto.DecodePayload(msg, &resp); err != nil {
		return err
	}
	s.code = resp.Code
	s.expiresAt = time.Unix(resp.ExpiresAt, 0)
	fmt.Printf("\n协助码: %s\n", formatCode(resp.Code))
	fmt.Printf("有效期至: %s\n\n", s.expiresAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Println("等待协助端连接...")
	return nil
}

// waitAndHandleTunnel 等待协助端连接，然后处理隧道
func (s *ShareMode) waitAndHandleTunnel() error {
	sessionID, err := s.waitSessionReady()
	if err != nil {
		return err
	}

	// 尝试 P2P 直连
	p2pMode := p2p.ParseP2PMode(s.client.config.P2PMode)
	if p2pMode != p2p.P2PModeDisabled {
		tunnel, err := s.negotiateP2P(p2pMode, sessionID)
		if err != nil {
			if p2pMode == p2p.P2PModeRequired {
				return fmt.Errorf("P2P 连接失败: %w", err)
			}
			log.Printf("P2P negotiation failed, falling back to relay: %v", err)
		}
		if tunnel != nil {
			fmt.Println("开始 P2P 直连转发SSH流量...")
			return s.handleTunnelP2P(tunnel)
		}
	}

	s.client.ResetDecoder() // P2P 协商超时会导致 json.Decoder 缓存错误
	fmt.Println("开始中转转发SSH流量...")
	return s.handleTunnel()
}

// waitSessionReady 等待会话就绪
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
			if ready.PeerVersion != "" {
				fmt.Printf("对端版本: %s\n", ready.PeerVersion)
			}
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

// negotiateP2P 尝试 P2P 直连协商
func (s *ShareMode) negotiateP2P(mode p2p.P2PMode, sessionID string) (*p2p.UDPTunnel, error) {
	mgr := p2p.NewP2PManager(mode, s.client.config.STUNServer, s.client.config.BindIP)
	mgr.SetRelayConn(s.client)

	resultCh, err := mgr.Start(sessionID, true)
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
	s.client.SetReadDeadline(time.Now().Add(negotiationTimeout))

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
		case proto.MsgError:
			s.client.SetReadDeadline(time.Time{})
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			mgr.Close()
			return nil, fmt.Errorf("server error: %s", errMsg.Message)
		}
	}

	s.client.SetReadDeadline(time.Time{})

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

// handleTunnelP2P 通过 P2P 隧道处理 SSH 流量
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

	buf := make([]byte, 32*1024)
	for {
		n, err := tunnel.Read(buf)
		if err != nil {
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			// P2P 隧道断开，视为协助端断开
			return ErrPeerDisconnected
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

// handleTunnel 处理隧道（支持多次SSH连接）
func (s *ShareMode) handleTunnel() error {
	// Start heartbeat to keep relay connection alive through NAT/firewalls
	s.client.StartHeartbeatLoop(30 * time.Second)

	// Shared state: current SSH connection to local SSH server
	var sshConn net.Conn
	var connMu sync.Mutex

	// connectSSH lazily connects to local SSH and starts a reader goroutine
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
		fmt.Println("已连接到本地SSH服务...")

		// Start SSH → relay goroutine
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					break
				}
				if err := s.client.SendMessage(proto.MsgTunnelData, &proto.TunnelData{Data: buf[:n]}); err != nil {
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

	// Main loop: read from relay, forward to SSH (connecting on demand)
	for {
		// Set read timeout to detect dead connections (reset after each successful read)
		s.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
		msg, err := s.client.ReadMessage()
		if err != nil {
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			if err.Error() == "EOF" {
				return nil
			}
			return err
		}

		switch msg.Type {
		case proto.MsgTunnelData:
			var dataMsg proto.TunnelData
			if err := json.Unmarshal(msg.Payload, &dataMsg); err != nil {
				continue
			}
			conn := connectSSH()
			if conn != nil {
				if _, err := conn.Write(dataMsg.Data); err != nil {
					connMu.Lock()
					if sshConn == conn {
						sshConn = nil
					}
					connMu.Unlock()
					conn.Close()
				}
			}
		case proto.MsgHeartbeat:
			// ignore
		case proto.MsgError:
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			if errMsg.Code == "PEER_DISCONNECTED" {
				return ErrPeerDisconnected
			}
			return fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		}
	}
}

// GetCode 获取协助码
func (s *ShareMode) GetCode() string {
	return s.code
}

// GetExpiresAt 获取过期时间
func (s *ShareMode) GetExpiresAt() time.Time {
	return s.expiresAt
}

func formatCode(code string) string {
	if len(code) < 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}
