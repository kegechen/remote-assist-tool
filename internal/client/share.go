package client

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
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

// handleTunnel 处理隧道
func (s *ShareMode) handleTunnel() error {
	sshConn, err := net.Dial("tcp", s.sshAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to local SSH: %w", err)
	}
	defer sshConn.Close()

	errChan := make(chan error, 2)

	go func() {
		for {
			msg, err := s.client.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			if msg.Type == proto.MsgTunnelData {
				var dataMsg proto.TunnelData
				if err := json.Unmarshal(msg.Payload, &dataMsg); err == nil {
					if _, err := sshConn.Write(dataMsg.Data); err != nil {
						errChan <- err
						return
					}
				}
			}
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := sshConn.Read(buf)
			if err != nil {
				errChan <- err
				return
			}

			dataMsg := &proto.TunnelData{Data: buf[:n]}
			if err := s.client.SendMessage(proto.MsgTunnelData, dataMsg); err != nil {
				errChan <- err
				return
			}
		}
	}()

	err = <-errChan
	if err != nil && err != io.EOF {
		return err
	}
	return nil
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
