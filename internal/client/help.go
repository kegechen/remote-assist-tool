package client

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/remote-assist/tool/internal/proto"
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
		fmt.Printf("\n在另一个终端运行:  ssh -p %s user@127.0.0.1\n", getPort(h.listenAddr))

		return h.handleTunnel()
	}

	return fmt.Errorf("unexpected response: %s", msg.Type)
}

// handleTunnel 处理隧道（支持多次SSH连接）
func (h *HelpMode) handleTunnel() error {
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
			msg, err := h.client.ReadMessage()
			if err != nil {
				relayDone <- err
				return
			}
			if msg.Type == proto.MsgTunnelData {
				var dataMsg proto.TunnelData
				if err := json.Unmarshal(msg.Payload, &dataMsg); err == nil {
					connMu.Lock()
					conn := currentConn
					connMu.Unlock()
					if conn != nil {
						conn.Write(dataMsg.Data)
					}
				}
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
