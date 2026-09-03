package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// streamKey 非零 key，用来同时覆盖块的 AEAD 解密路径。
var streamKey = [32]byte{1, 2, 3, 4, 5}

// sealChunk 模拟 share 端的 chunkSink：块数据用裸 AEADSeal 加密后放进 Data。
func sealChunk(t *testing.T, id uint64, seq uint32, stream, text string) *proto.Message {
	t.Helper()
	ct, err := proto.AEADSeal(&streamKey, []byte(text), proto.StreamChunkAAD(id, seq, stream))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	msg, err := proto.NewMessage(proto.MsgToolStream, &proto.StreamChunk{ID: id, Seq: seq, Stream: stream, Data: ct})
	if err != nil {
		t.Fatalf("new msg: %v", err)
	}
	return msg
}

func sealResp(t *testing.T, id uint64, result string) *proto.Message {
	t.Helper()
	wrapped, err := proto.AEADSealJSON(&streamKey, json.RawMessage(result), proto.ToolRespAAD(id))
	if err != nil {
		t.Fatalf("seal resp: %v", err)
	}
	msg, err := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: id, OK: true, ResultJSON: wrapped})
	if err != nil {
		t.Fatalf("new msg: %v", err)
	}
	return msg
}

// TestBridgeCallToolStreamDecryptsAndOrdersChunks 覆盖 bridge 的流式收敛：块要解密、
// 按序回调，最终结果照常返回。
func TestBridgeCallToolStreamDecryptsAndOrdersChunks(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "hel"))
		br.HandleInbound(sealChunk(t, r.ID, 1, "stdout", "lo"))
		br.HandleInbound(sealChunk(t, r.ID, 2, "stderr", "oops"))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	var mu sync.Mutex
	var stdout, stderr string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(stream string, data []byte) {
			mu.Lock()
			defer mu.Unlock()
			if stream == "stderr" {
				stderr += string(data)
			} else {
				stdout += string(data)
			}
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(out) != `{"exit_code":0}` {
		t.Fatalf("result=%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if stdout != "hello" {
		t.Fatalf("stdout=%q，想要 \"hello\"（顺序错或解密失败）", stdout)
	}
	if stderr != "oops" {
		t.Fatalf("stderr=%q", stderr)
	}
}

// TestBridgeIgnoresChunksForNonStreamCall 普通 CallTool 没登记回调，收到块也不能 panic
// 或串到别的调用上——旧版 share 端仍会为某些工具发块。
func TestBridgeIgnoresChunksForNonStreamCall(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "ignored"))
		br.HandleInbound(sealResp(t, r.ID, `{"ok":1}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := br.CallTool(ctx, "exec", json.RawMessage(`{"argv":["x"]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(out) != `{"ok":1}` {
		t.Fatalf("result=%s", out)
	}
}

// TestBridgeDropsChunksAfterCallReturns 调用结束后迟到的块必须被丢掉：share 端可能在
// host 兜底超时后才把剩余输出吐完，那时回调早已失效，不能再往里投。
func TestBridgeDropsChunksAfterCallReturns(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	var id uint64
	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		id = r.ID
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	var mu sync.Mutex
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(string, []byte) { mu.Lock(); calls++; mu.Unlock() }); err != nil {
		t.Fatalf("call: %v", err)
	}
	// 调用已返回，回调已摘除；此刻投块应被静默丢弃（不 panic、不回调）
	br.HandleInbound(sealChunk(t, id, 9, "stdout", "late"))
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("调用结束后仍回调了 %d 次", calls)
	}
}
