package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// callToolHardTimeout 是 Bridge.CallTool 的硬超时上限。
// 防御性兜底：即便心跳保活失败、relay TCP 真死，单次工具调用最多挂 60s
// 就返 tunnel_lost 错误，MCP server 仍存活可接受新调用（含重新 connect）。
const callToolHardTimeout = 60 * time.Second

// MsgConn help 端发给 share 端的对外契约（client.Client 已经实现 SendMessage）
type MsgConn interface {
	SendMessage(t proto.MessageType, payload interface{}) error
}

// Bridge MCP server <-> 隧道工具消息
type Bridge struct {
	conn    MsgConn
	key     [32]byte
	nextID  uint64
	pending sync.Map // id -> chan proto.ToolResp
}

func NewBridge(c MsgConn, key [32]byte) *Bridge { return &Bridge{conn: c, key: key} }

// CallTool 发 ToolReq，阻塞等 ToolResp（或 ctx 取消）
func (b *Bridge) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	id := atomic.AddUint64(&b.nextID, 1)
	ch := make(chan proto.ToolResp, 1)
	b.pending.Store(id, ch)
	defer b.pending.Delete(id)

	encArgs := args
	if b.key != [32]byte{} && len(args) > 0 {
		wrapped, err := proto.AEADSealJSON(&b.key, args)
		if err != nil {
			return nil, err
		}
		encArgs = wrapped
	}
	if err := b.conn.SendMessage(proto.MsgToolReq, &proto.ToolReq{ID: id, Tool: name, ArgsJSON: encArgs}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		b.conn.SendMessage(proto.MsgToolCancel, &proto.Cancel{ID: id, Reason: "ctx_cancelled"})
		return nil, ctx.Err()
	case <-time.After(callToolHardTimeout):
		b.conn.SendMessage(proto.MsgToolCancel, &proto.Cancel{ID: id, Reason: "hard_timeout"})
		return nil, fmt.Errorf("tunnel_lost: no response in %s", callToolHardTimeout)
	case resp := <-ch:
		if !resp.OK {
			return nil, fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMsg)
		}
		result := resp.ResultJSON
		if b.key != [32]byte{} && len(result) > 0 {
			plain, err := proto.AEADOpenJSON(&b.key, result)
			if err != nil {
				return nil, err
			}
			result = plain
		}
		return result, nil
	}
}

// HandleInbound help 端 dispatch 收到 ToolResp / ToolStream 时调用
func (b *Bridge) HandleInbound(msg *proto.Message) {
	switch msg.Type {
	case proto.MsgToolResp:
		var r proto.ToolResp
		if err := proto.DecodePayload(msg, &r); err != nil {
			return
		}
		if v, ok := b.pending.Load(r.ID); ok {
			ch := v.(chan proto.ToolResp)
			select {
			case ch <- r:
			default:
			}
		}
	case proto.MsgToolStream:
		// v1: 流式工具已从 schema 删除；如仍收到此帧（旧 share 端版本），忽略。
	}
}
