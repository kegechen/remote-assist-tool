package mcp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
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
	connMu    sync.RWMutex // 保护 conn/key：P2P 热升级会在读循环外并发替换二者
	conn      MsgConn
	key       [32]byte
	nextID    uint64
	pending   sync.Map    // id -> chan proto.ToolResp
	streamCbs sync.Map    // id -> *streamRecv，仅流式调用登记
	closed    atomic.Bool // 隧道断开后置 true，CallTool 立即快速失败
	closeMu   sync.Mutex  // 串行化 Disconnect，避免重复广播
}

// streamRecv 一次流式调用的接收状态：回调 + Seq 连续性。
//
// StreamChunk.Seq 以前是只写不读的：share 端每帧递增，help 端从来不看。于是任何一处丢帧
// （relay 限流丢弃、AEAD 解密失败）都表现为"输出中间静静少了几 KB，最终 ToolResp 仍然
// OK"——调用方拿到一份看起来完整、实际被挖空的结果。这里补上校验：一旦发现空洞就记下，
// 调用结束时把整次调用判为失败，宁可报错也不交出残缺输出。
type streamRecv struct {
	cb func(stream string, data []byte)

	mu   sync.Mutex
	next uint32 // 期望的下一个 Seq（share 端 chunkSink 从 0 开始）
	gaps int    // 累计缺失的帧数
}

// observe 记录一帧的 Seq 并返回是否发现空洞。乱序不会发生（同一条隧道的读循环单线程
// 投递，P2P 与 relay 之间的切换也是原子换连接），所以 Seq != next 即视为丢帧。
// 小于 next 的（重复/滞后帧）不计入缺失，也不回退期望值。
func (s *streamRecv) observe(seq uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < s.next {
		return
	}
	if seq > s.next {
		s.gaps += int(seq - s.next)
	}
	s.next = seq + 1
}

// markGap 用于"帧到了但内容用不了"的情况（AEAD 解密失败）：Seq 是连续的，但数据丢了。
func (s *streamRecv) markGap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gaps++
}

func (s *streamRecv) gapCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gaps
}

func NewBridge(c MsgConn, key [32]byte) *Bridge {
	return &Bridge{conn: c, key: key, nextID: newIDEpoch()}
}

// newIDEpoch 给每个 Bridge 实例的请求 ID 取一个随机高位起点。
//
// 为什么不能从 0 开始数：share 端的 agent.Daemon 由 daemonOnce 保证进程内只建一次，它的
// cancels 表和在途 handleReq 跨会话存活；而 help 端每次 connect 都新建 Bridge。两边从 1
// 开始重新数的话，重连后的第一条调用和上一会话的残留请求撞上同一个 ID：滞后的
// ToolResp 会命中新调用的 pending，滞后的 cancel 清理会抹掉新调用的 cancels 登记。
//
// 随机高 32 位 + 从 0 递增的低 32 位，让两次会话撞号的概率降到可忽略，且不改动线上协议
// （ID 本来就是 uint64，两端都只当作不透明标识）。
// 拿不到随机源时退回 0（等价于旧行为），不因为熵不足就让整个 bridge 起不来。
func newIDEpoch() uint64 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return uint64(binary.BigEndian.Uint32(b[:])) << 32
}

// SwapConn 原子替换工具消息的出口连接与会话密钥，用于 relay ⇄ P2P 热切换：
// 连接先在 relay 上完成握手并可用，P2P 打洞成功且双向证实后再切到隧道；
// P2P 隧道中途断掉时再切回 relay。对端由 agent.Daemon.SwapConn 做对称切换。
//
// P2P 升级复用 relay 握手协商出的同一把 key（key 由 code+双方 nonce 派生，与传输
// 通道无关），此时传入原 key 即可；只有重新握手（降级回 relay）才会换 key。
func (b *Bridge) SwapConn(c MsgConn, key [32]byte) {
	b.connMu.Lock()
	b.conn = c
	b.key = key
	b.connMu.Unlock()
}

// snapshot 取当前 conn/key 的一致快照。单次 CallTool 全程用同一份快照，保证请求的
// 加密 key、发送通道、响应解密 key 三者配对，不会被中途的 SwapConn 撕裂。
func (b *Bridge) snapshot() (MsgConn, [32]byte) {
	b.connMu.RLock()
	defer b.connMu.RUnlock()
	return b.conn, b.key
}

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
	var recv *streamRecv
	if onChunk != nil {
		recv = &streamRecv{cb: onChunk}
		b.streamCbs.Store(id, recv)
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

	// 全程用同一份 conn/key 快照，避免中途 SwapConn 导致「用旧 key 加密、却按新 key 解密」。
	conn, key := b.snapshot()

	encArgs := args
	if key != [32]byte{} && len(args) > 0 {
		wrapped, err := proto.AEADSealJSON(&key, args)
		if err != nil {
			return nil, err
		}
		encArgs = wrapped
	}
	if err := conn.SendMessage(proto.MsgToolReq, &proto.ToolReq{ID: id, Tool: name, ArgsJSON: encArgs}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		// 区分 deadline 到期（疑似 tunnel 死）与上层主动取消（用户中断）。
		reason := "ctx_cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = "deadline_exceeded"
		}
		// cancel 是后发的，要走**当前**活跃通道：请求发出后若已切到 P2P，
		// 往旧 relay conn 发 cancel share 端收不到。
		curConn, _ := b.snapshot()
		curConn.SendMessage(proto.MsgToolCancel, &proto.Cancel{ID: id, Reason: reason})
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("tunnel_lost: no response within deadline")
		}
		return nil, ctx.Err()
	case resp := <-ch:
		if !resp.OK {
			return nil, fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMsg)
		}
		// 流式输出缺了帧就不能当成功返回：调用方（GUI 终端 / AI）没有别的办法察觉，
		// 一份被挖空却标着 OK 的输出比一次明确的失败危险得多。
		if recv != nil {
			if n := recv.gapCount(); n > 0 {
				return nil, fmt.Errorf("stream_incomplete: 流式输出缺失 %d 帧（relay 限流或隧道异常），结果不完整", n)
			}
		}
		result := resp.ResultJSON
		if key != [32]byte{} && len(result) > 0 {
			plain, err := proto.AEADOpenJSON(&key, result)
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
		recv := v.(*streamRecv)
		recv.observe(c.Seq)
		data := c.Data
		if _, key := b.snapshot(); key != [32]byte{} && len(data) > 0 {
			plain, err := proto.AEADOpen(&key, data)
			if err != nil {
				// 帧损坏/换了 key：这一帧的内容没了，但后续帧与最终 ToolResp 不受影响。
				// 记成一个空洞，由 callToolInner 在收尾时把整次调用判为不完整。
				recv.markGap()
				return
			}
			data = plain
		}
		if len(data) > 0 {
			recv.cb(c.Stream, data)
		}
	}
}
