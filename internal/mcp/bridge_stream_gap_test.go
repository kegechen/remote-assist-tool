package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// TestBridgeStreamGapFailsCall 中途丢了一帧就不能报成功。
//
// relay 的限流以前会静默丢掉超限的 MsgToolStream，Seq 又从来没人读，于是调用方拿到的是
// "中间被挖掉几 KB、最终 ToolResp 仍然 OK" 的输出——没有任何办法察觉。
func TestBridgeStreamGapFailsCall(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "head"))
		// seq 1 被丢掉（模拟 relay 限流丢帧）
		br.HandleInbound(sealChunk(t, r.ID, 2, "stdout", "tail"))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(string, []byte) {})
	if err == nil {
		t.Fatal("流中间丢了一帧，调用却报成功——残缺输出被当成完整结果交给了调用方")
	}
	if !strings.Contains(err.Error(), "stream_incomplete") {
		t.Fatalf("错误没指明是流不完整: %v", err)
	}
}

// TestBridgeStreamUndecryptableChunkFailsCall 帧到了但解不开也是空洞：Seq 连续，
// 内容却没了。以前这里是 return 静默丢弃。
func TestBridgeStreamUndecryptableChunkFailsCall(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "ok"))
		bad, _ := proto.NewMessage(proto.MsgToolStream,
			&proto.StreamChunk{ID: r.ID, Seq: 1, Stream: "stdout", Data: []byte("not-a-valid-aead-frame")})
		br.HandleInbound(bad)
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(string, []byte) {}); err == nil {
		t.Fatal("有一帧解密失败，调用却报成功")
	}
}

// TestBridgeStreamNoGapStillSucceeds 正常连续的流不能被误判。
func TestBridgeStreamNoGapStillSucceeds(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		for i := uint32(0); i < 5; i++ {
			br.HandleInbound(sealChunk(t, r.ID, i, "stdout", "x"))
		}
		// 重复投递一帧（滞后帧）不应被算成缺失
		br.HandleInbound(sealChunk(t, r.ID, 3, "stdout", ""))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	var mu sync.Mutex
	got := 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(string, []byte) { mu.Lock(); got++; mu.Unlock() })
	if err != nil {
		t.Fatalf("连续的流被误判为不完整: %v", err)
	}
	if string(out) != `{"exit_code":0}` {
		t.Fatalf("result=%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != 5 {
		t.Fatalf("回调 %d 次，想要 5", got)
	}
}

// TestBridgeStreamOutOfOrderIsNotAGap 乱序到达不是丢帧。
//
// help 端在 P2P 升级后同时跑两条读循环——relay 那条按设计不会停（help_bootstrap.go
// 「始终投递」），share 端换出口也不是原子的。于是同一次调用的帧可能一部分从 relay 来、
// 一部分从 P2P 来，两条 goroutine 之间没有任何顺序保证。把"Seq 不等于期望值"直接判成丢帧
// 会把一次完全正常的调用打成 stream_incomplete。
func TestBridgeStreamOutOfOrderIsNotAGap(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "a"))
		br.HandleInbound(sealChunk(t, r.ID, 1, "stdout", "b"))
		// 3 先于 2 到（P2P 那条读循环跑赢了还在 relay 上飞的那一帧）
		br.HandleInbound(sealChunk(t, r.ID, 3, "stdout", "d"))
		br.HandleInbound(sealChunk(t, r.ID, 2, "stdout", "c"))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	var mu sync.Mutex
	got := 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(string, []byte) { mu.Lock(); got++; mu.Unlock() }); err != nil {
		t.Fatalf("乱序到达被误判为丢帧: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != 4 {
		t.Fatalf("回调 %d 次，想要 4", got)
	}
}

// TestBridgeStreamLateChunkAfterRespIsNotAGap 最终 ToolResp 抢在最后一帧前面到达，
// 同样不能判成丢帧。
//
// 这正是 relay⇄P2P 热切换的典型时序：resp 走新通道先到，尾帧还在旧通道上飞。收尾时看一眼
// 就下结论会误杀，所以发现空洞后要留一个补齐窗口（streamGapSettle）。
func TestBridgeStreamLateChunkAfterRespIsNotAGap(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "a"))
		br.HandleInbound(sealChunk(t, r.ID, 2, "stdout", "c"))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
		time.Sleep(40 * time.Millisecond) // 尾帧还在旧通道上
		br.HandleInbound(sealChunk(t, r.ID, 1, "stdout", "b"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(string, []byte) {}); err != nil {
		t.Fatalf("迟到的尾帧在补齐窗口内到了，却仍被判为不完整: %v", err)
	}
}

// TestBridgeRequestIDsDifferAcrossConnects 每次 connect 新建的 Bridge 不能都从同一个 ID
// 开始数。
//
// share 端的 agent.Daemon 是进程级单例（daemonOnce），cancels 表和在途 handleReq 跨会话
// 存活；help 端却每次重连都新建 Bridge。两边都从 1 开始的话，重连后第一条调用就和上一
// 会话的残留请求撞号：滞后的 ToolResp 命中新调用的 pending（表现为续传 stat 拿到
// offset=0、整个文件重传），或滞后的 cancel 清理抹掉新调用的登记（后续取消变成 no-op）。
func TestBridgeRequestIDsDifferAcrossConnects(t *testing.T) {
	firstID := func() uint64 {
		conn := &stubConn{sent: make(chan *proto.Message, 1)}
		br := NewBridge(conn, [32]byte{})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		go br.CallTool(ctx, "ping", json.RawMessage(`{}`))
		select {
		case req := <-conn.sent:
			var r proto.ToolReq
			if err := proto.DecodePayload(req, &r); err != nil {
				t.Fatalf("decode: %v", err)
			}
			return r.ID
		case <-time.After(2 * time.Second):
			t.Fatal("没等到 ToolReq")
			return 0
		}
	}

	a, b := firstID(), firstID()
	if a == b {
		t.Fatalf("两次 connect 的首个请求 ID 相同 (%d)：跨会话的滞后帧会串到新调用上", a)
	}
	if a == 1 || b == 1 {
		t.Fatalf("请求 ID 仍然从 1 开始 (a=%d b=%d)", a, b)
	}
}
