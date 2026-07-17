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
	conn      MsgConn
	key       [32]byte
	nextID    uint64
	pending   sync.Map    // id -> chan proto.ToolResp
	streamCbs sync.Map    // id -> func(stream string, data []byte)，仅流式调用登记
	closed    atomic.Bool // 隧道断开后置 true，CallTool 立即快速失败
	closeMu   sync.Mutex  // 串行化 Disconnect，避免重复广播
}

func NewBridge(c MsgConn, key [32]byte) *Bridge { return &Bridge{conn: c, key: key} }

// Disconnect 标记隧道已断开，并唤醒所有在途 CallTool 立即返回 err。
//
// 设计意图：读循环 goroutine 检测到 ReadMessage 出错（隧道死）时调用本方法，
// 让已在飞的 CallTool 不必干等兜底 deadline（2~10 分钟），立刻拿到友好错误。
// pending 里每个 channel 是 buffered size 1，投递用非阻塞 select（HandleInbound
// 已占用名额时 default 兜底），避免死锁。
func (b *Bridge) Disconnect(err error) {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if b.closed.Load() {
		return // 已断开，避免重复广播
	}
	if err == nil {
		err = errors.New("tunnel_lost: 隧道已断开，请重新 connect")
	}
	b.closed.Store(true)
	// 遍历所有在途调用，投递失败结果（OK=false）唤醒它们。
	b.pending.Range(func(_, v interface{}) bool {
		ch := v.(chan proto.ToolResp)
		select {
		case ch <- proto.ToolResp{OK: false, ErrorCode: "tunnel_lost", ErrorMsg: err.Error()}:
		default: // channel 已有结果（HandleInbound 抢先投递过），无需再唤醒
		}
		return true
	})
}

// CallTool 发 ToolReq，阻塞等 ToolResp 或 ctx 完成。
// ctx 没设 deadline 时自动加 callToolFallbackTimeout（10 分钟）兜底。
// CallTool 同步调用一个远端工具，返回最终结果（不接收流式块）。
func (b *Bridge) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return b.callToolInner(ctx, name, args, nil)
}

// CallToolStream 调用工具并把中途的流式块通过 onChunk 实时回调（exec stream=true）；
// 最终结果仍由返回值给出。块按 share 端产出的顺序到达（同一条隧道读循环单线程投递）。
//
// onChunk 契约：**不可长时间阻塞**。它在 help 端的隧道读循环上被同步调用，堵住它就等于
// 堵住整条隧道（心跳也读不到，最终被判 tunnel_lost）。需要慢速消费的调用方自己排队——
// gui.MCPClient 就为此配了每条流一个 pump goroutine。
func (b *Bridge) CallToolStream(ctx context.Context, name string, args json.RawMessage, onChunk func(stream string, data []byte)) (json.RawMessage, error) {
	return b.callToolInner(ctx, name, args, onChunk)
}

func (b *Bridge) callToolInner(ctx context.Context, name string, args json.RawMessage, onChunk func(string, []byte)) (json.RawMessage, error) {
	// 隧道已断开：立即快速失败，不再发消息、不再干等兜底 deadline。
	if b.closed.Load() {
		return nil, fmt.Errorf("not_connected: 隧道已断开，请让远端重跑 share 后重新 connect")
	}

	id := atomic.AddUint64(&b.nextID, 1)
	ch := make(chan proto.ToolResp, 1)
	b.pending.Store(id, ch)
	defer b.pending.Delete(id)
	if onChunk != nil {
		b.streamCbs.Store(id, onChunk)
		defer b.streamCbs.Delete(id)
	}

	// 再查一次 closed：堵住 Store 与 Disconnect.Range 之间的竞态窗口——若 Disconnect
	// 在我们 Store 之前已遍历完 pending，本次调用不会被它唤醒，这里兜底快速失败。
	if b.closed.Load() {
		return nil, fmt.Errorf("not_connected: 隧道已断开，请让远端重跑 share 后重新 connect")
	}

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
		var c proto.StreamChunk
		if err := proto.DecodePayload(msg, &c); err != nil {
			return
		}
		v, ok := b.streamCbs.Load(c.ID)
		if !ok {
			// 非流式调用、或调用已结束（ToolResp 先到 / 超时后 share 端仍在吐）：丢弃。
			return
		}
		data := c.Data
		if b.key != [32]byte{} && len(data) > 0 {
			plain, err := proto.AEADOpen(&b.key, data)
			if err != nil {
				return // 帧损坏/换了 key：丢这一帧，不影响后续帧与最终 ToolResp
			}
			data = plain
		}
		if len(data) > 0 {
			v.(func(string, []byte))(c.Stream, data)
		}
	}
}
