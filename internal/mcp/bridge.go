package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// callToolFallbackTimeout 是 Bridge.CallTool 的兜底 deadline，仅在调用方没在 ctx
// 上显式设过 deadline 时生效。
//
// 设计意图：
//   - 调用方（MCP server / 测试代码）若已通过 ctx.WithDeadline 表达了"我愿意等多久"，
//     就尊重调用方意图；这样长跑工具（exec 默认 5 分钟、grep 大仓库等）只要 ctx
//     deadline 够长就不会被 bridge 多事切掉。
//   - 调用方没设 deadline → 加一个保守的 10 分钟兜底，防止单条 ToolResp 永远不来
//     导致 MCP server 整个挂死，又不至于把合理长跑工具误杀。
//
// 上一版用 time.After(60s) 硬切：太短，60s 内跑不完的工具被误杀（回归 bug）。
const (
	// callToolFallbackTimeout 长跑工具（exec / 大仓库 grep / glob / 文件分块传输）的兜底上限。
	callToolFallbackTimeout = 10 * time.Minute
	// callToolQuickFallbackTimeout 元数据类快工具的兜底上限：当 ToolResp 被丢或隧道
	// 异常时快速失败，不必干等 10 分钟。只用于“确定性快、输出有界”的工具，避免重蹈
	// 上面提到的“60s 硬切误杀长跑工具”覆辙。
	callToolQuickFallbackTimeout = 2 * time.Minute
)

// fallbackTimeoutFor 按工具名选择兜底超时。只有确定快、输出有界的元数据类工具走短
// 兜底；exec/grep/glob/read_file/write_file/文件传输等可能合理长跑的仍用 10 分钟。
func fallbackTimeoutFor(name string) time.Duration {
	switch name {
	case "stat", "list_dir", "process_list", "tail_log":
		return callToolQuickFallbackTimeout
	default:
		return callToolFallbackTimeout
	}
}

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

// CallTool 发 ToolReq，阻塞等 ToolResp 或 ctx 完成。
// ctx 没设 deadline 时自动加 callToolFallbackTimeout（10 分钟）兜底。
func (b *Bridge) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	id := atomic.AddUint64(&b.nextID, 1)
	ch := make(chan proto.ToolResp, 1)
	b.pending.Store(id, ch)
	defer b.pending.Delete(id)

	// 调用方没设 deadline 才加兜底；尊重调用方显式 deadline。
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fallbackTimeoutFor(name))
		defer cancel()
	}

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
		// 区分 deadline 到期（疑似 tunnel 死）与上层主动取消（用户中断）。
		reason := "ctx_cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = "deadline_exceeded"
		}
		b.conn.SendMessage(proto.MsgToolCancel, &proto.Cancel{ID: id, Reason: reason})
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("tunnel_lost: no response within deadline")
		}
		return nil, ctx.Err()
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
