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

// handshakeTool 工具通道握手；返回 session_key 与失败原因。
// 等待 HelloAck 期间会跳过非相关消息（PeerAddrReady、Heartbeat 等），
// 防止 relay 主动推的 P2P 寻址通知抢先到达打断握手。
func (h *HelpMode) handshakeTool() ([32]byte, error) {
	hello := proto.NewHello()
	if err := h.client.SendMessage(proto.MsgToolHello, &hello); err != nil {
		return [32]byte{}, err
	}
	h.client.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer h.client.SetReadDeadline(time.Time{})
	for {
		msg, err := h.client.ReadMessage()
		if err != nil {
			return [32]byte{}, err
		}
		switch msg.Type {
		case proto.MsgToolHelloAck:
			var ack proto.HelloAck
			proto.DecodePayload(msg, &ack)
			if !ack.Accept {
				return [32]byte{}, fmt.Errorf("share rejected tool channel: %s", ack.ErrorMsg)
			}
			return proto.DeriveSessionKey(h.code, ack.NonceB64, hello.NonceB64), nil
		case proto.MsgHeartbeat, proto.MsgPeerAddrReady, proto.MsgError:
			// 跳过 relay 在 Join 之后主动推送的消息，继续等 Ack
			continue
		default:
			// 其他类型也忽略（保守做法，让握手尽量稳定）
			continue
		}
	}
}

// RunMCPMode 阻塞跑 MCP stdio server 到 stdin EOF / 隧道断开。
// 调用前 h.client.Connect + Join 已完成，但工具通道握手由本函数发起。
func (h *HelpMode) RunMCPMode(ctx context.Context) error {
	key, err := h.handshakeTool()
	if err != nil {
		return fmt.Errorf("tool handshake: %w", err)
	}
	bridge := mcp.NewBridge(h.client, key)
	// 心跳保活：每 30s 发 Heartbeat，relay 回 echo，避免读 deadline 因空闲被触发，
	// 导致后台 goroutine 退出 → MCP 工具调用全部失效。
	h.client.StartHeartbeatLoop(30 * time.Second)
	// 后台 ReadMessage 循环，把工具消息投给 bridge
	go func() {
		for {
			// 读 deadline 兜底检测隧道死亡：心跳每 30s 且 relay 回 echo，健康隧道每 30s
			// 必有帧重置本 deadline；取 75s（容 2 个心跳周期）比原 2min 更快发现静默死亡
			// → 更早 Disconnect 唤醒在途调用。
			h.client.SetReadDeadline(time.Now().Add(75 * time.Second))
			msg, err := h.client.ReadMessage()
			if err != nil {
				// 隧道死了：唤醒所有在途 CallTool 立即返回友好错误，不再干等兜底。
				bridge.Disconnect(fmt.Errorf("tunnel_lost: 隧道已断开（%w），请重新 connect", err))
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
