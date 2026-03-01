package client

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

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

// handleTunnel 处理隧道
func (h *HelpMode) handleTunnel() error {
	listener, err := net.Listen("tcp", h.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer listener.Close()

	localConn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer localConn.Close()

	fmt.Println("本地SSH连接已建立，开始转发流量...")

	errChan := make(chan error, 2)

	go func() {
		for {
			msg, err := h.client.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			if msg.Type == proto.MsgTunnelData {
				var dataMsg proto.TunnelData
				if err := json.Unmarshal(msg.Payload, &dataMsg); err == nil {
					if _, err := localConn.Write(dataMsg.Data); err != nil {
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
			n, err := localConn.Read(buf)
			if err != nil {
				errChan <- err
				return
			}

			dataMsg := &proto.TunnelData{Data: buf[:n]}
			if err := h.client.SendMessage(proto.MsgTunnelData, dataMsg); err != nil {
				errChan <- err
				return
			}
		}
	}()

	err = <-errChan
	if err != nil && err != io.EOF {
		log.Printf("Tunnel error: %v", err)
		return err
	}
	return nil
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
