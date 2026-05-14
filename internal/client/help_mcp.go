package client

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/proto"
)

// inboundSink help 端 dispatch 投递 ToolResp/ToolStream 的契约（mcp.Bridge 实现之）
type inboundSink interface {
	HandleInbound(msg *proto.Message)
}

// dispatchHelpToolMessage 工具消息分流；返回 true 表示已消费
func dispatchHelpToolMessage(msg *proto.Message, b inboundSink) bool {
	switch msg.Type {
	case proto.MsgToolResp, proto.MsgToolStream, proto.MsgToolHelloAck:
		if b != nil {
			b.HandleInbound(msg)
		}
		return true
	}
	return false
}

// handshakeTool 工具通道握手；返回 session_key 与失败原因
func (h *HelpMode) handshakeTool() ([32]byte, error) {
	hello := proto.NewHello()
	if err := h.client.SendMessage(proto.MsgToolHello, &hello); err != nil {
		return [32]byte{}, err
	}
	h.client.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer h.client.SetReadDeadline(time.Time{})
	msg, err := h.client.ReadMessage()
	if err != nil {
		return [32]byte{}, err
	}
	if msg.Type != proto.MsgToolHelloAck {
		return [32]byte{}, fmt.Errorf("expected hello_ack, got %s", msg.Type)
	}
	var ack proto.HelloAck
	proto.DecodePayload(msg, &ack)
	if !ack.Accept {
		return [32]byte{}, fmt.Errorf("share rejected tool channel: %s", ack.ErrorMsg)
	}
	key := proto.DeriveSessionKey(h.code, ack.NonceB64, hello.NonceB64)
	return key, nil
}

// RunMCPMode 阻塞跑 MCP stdio server 到 stdin EOF / 隧道断开。
// 调用前 h.client.Connect + Join 已完成，但工具通道握手由本函数发起。
func (h *HelpMode) RunMCPMode(ctx context.Context) error {
	key, err := h.handshakeTool()
	if err != nil {
		return fmt.Errorf("tool handshake: %w", err)
	}
	bridge := mcp.NewBridge(h.client, key)
	// 后台 ReadMessage 循环，把工具消息投给 bridge
	go func() {
		for {
			h.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
			msg, err := h.client.ReadMessage()
			if err != nil {
				return
			}
			dispatchHelpToolMessage(msg, bridge)
		}
	}()

	srv := mcp.NewServer(bridge)
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		return fmt.Errorf("mcp serve: %w", err)
	}
	return nil
}
