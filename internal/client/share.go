package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

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

		if err := s.waitSessionReady(); err != nil {
			return s.code, s.expiresAt, err
		}

		return s.code, s.expiresAt, s.handleTunnel()
	}

	return s.code, s.expiresAt, fmt.Errorf("unexpected response: %s", msg.Type)
}

// waitSessionReady 等待会话就绪
func (s *ShareMode) waitSessionReady() error {
	for {
		msg, err := s.client.ReadMessage()
		if err != nil {
			return err
		}

		switch msg.Type {
		case proto.MsgSessionReady:
			fmt.Println("协助端已连接！开始转发SSH流量...")
			return nil
		case proto.MsgHeartbeat:
		case proto.MsgError:
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			return fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		default:
			log.Printf("Unexpected message: %s", msg.Type)
		}
	}
}

// handleTunnel 处理隧道（支持多次SSH连接）
func (s *ShareMode) handleTunnel() error {
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
