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

// peerGoneError 把 relay 推来的 MsgError 翻译成「本次会话已经没了」的错误；不属于这
// 一类时返回 nil，调用方按普通日志处理即可。
//
// help 端两条读循环过去都只是把 MsgError 打条日志就接着读。可这两个码之后，这条 relay
// 连接上再也不会有工具响应了——PEER_RECONNECTED 之后 relay 会立刻关连接，
// PEER_DISCONNECTED 之后被协助端已经不在。不拆会话的代价是 doConnect 里的
// b.activeTarget 还留着，下一次同参数 connect 命中幂等分支、返回一个指向死会话的
// connected=true，此后每个工具调用都只能干等超时，且永远自愈不了。
func peerGoneError(msg *proto.Message) error {
	var errMsg proto.ErrorMessage
	if err := proto.DecodePayload(msg, &errMsg); err != nil {
		return nil
	}
	switch errMsg.Code {
	case proto.ErrCodePeerDisconnected:
		return fmt.Errorf("peer_gone: 被协助端已断开连接，请重新 connect")
	case proto.ErrCodePeerReconnected:
		return fmt.Errorf("peer_gone: 被协助端连接已更新（重启或热升级），请重新 connect")
	}
	return nil
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
	return h.handshakeToolCapturing(nil)
}

// handshakeToolCapturing 同 handshakeTool，但把握手窗口内到达的 PeerAddrReady 交给
// capture 而不是丢弃。
//
// 这条消息**绝不能丢**：relay 对每次 PeerAddrAdvertise 只推送一次，且只推给对端
// （server.go handlePeerAddrAdvertise → UpdatePeerAddr → sendPeerAddrReady(update.Peer)）。
// share 会在收到 help 的 ToolHello 之前就完成 advertise，所以对端地址正好落在这个
// 握手窗口里；丢了它 help 就永远拿不到对端地址，startHolePunching 因 peerInfo == nil
// 直接返回，P2P 静默失效——只有 LAN / 全锥形 NAT 下靠 share 的单向包才碰巧能通。
func (h *HelpMode) handshakeToolCapturing(capture func(*proto.PeerAddrReady)) ([32]byte, error) {
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
		case proto.MsgPeerAddrReady:
			if capture != nil {
				var ready proto.PeerAddrReady
				if err := proto.DecodePayload(msg, &ready); err == nil {
					capture(&ready)
				}
			}
			continue
		case proto.MsgHeartbeat, proto.MsgError:
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
			if msg.Type == proto.MsgError {
				if gone := peerGoneError(msg); gone != nil {
					// 对端没了，这条连接不会再有工具响应：唤醒在途调用并关掉 relay，
					// 否则要干等 75s 读超时才发现，且 relay 端 Help 槽也一直占着。
					bridge.Disconnect(gone)
					h.client.Close()
					return
				}
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
