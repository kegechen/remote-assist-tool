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

// v1 对端的流式收敛。
//
// 0.0.x 的 agent 从不发终止帧：chunkSink.Finish 是 v2 之后的 8873779 才加的，在
// `git show 0.0.9:internal/agent/agent.go` 里搜不到任何 Fin 的发送点。所以"必须收到
// Fin 才算完整"这条 v2 规矩不能加在 v1 会话上，否则每一次流式调用（exec stream=true、
// tail_log follow）都会在等满补齐窗口后以 stream_incomplete 收场——而输出其实完整无缺。
//
// 这条路径 tests/proto_compat_test.go 抓不到：那里是本仓库的 Daemon 配本仓库的 Bridge，
// 而我们的 Daemon 即便在 v1 会话下也照发 Fin。只有手工构造"旧版形状"的帧才测得到。

// legacySealChunk 按 0.0.x 的方式加封流帧：无 AAD。
func legacySealChunk(t *testing.T, id uint64, seq uint32, stream, text string) *proto.Message {
	t.Helper()
	ct, err := proto.AEADSeal(&streamKey, []byte(text), nil)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	msg, err := proto.NewMessage(proto.MsgToolStream, &proto.StreamChunk{ID: id, Seq: seq, Stream: stream, Data: ct})
	if err != nil {
		t.Fatalf("new msg: %v", err)
	}
	return msg
}

// legacySealResp 按 0.0.x 的方式加封响应：无 AAD。
func legacySealResp(t *testing.T, id uint64, result string) *proto.Message {
	t.Helper()
	wrapped, err := proto.AEADSealJSON(&streamKey, json.RawMessage(result), nil)
	if err != nil {
		t.Fatalf("seal resp: %v", err)
	}
	msg, err := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: id, OK: true, ResultJSON: wrapped})
	if err != nil {
		t.Fatalf("new msg: %v", err)
	}
	return msg
}

func legacySession() proto.Session {
	return proto.Session{Key: streamKey, Version: proto.ToolProtocolVersionV1}
}

// TestBridgeLegacyStreamWithoutTerminator v1 会话下，没有终止帧的流式调用必须成功。
func TestBridgeLegacyStreamWithoutTerminator(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, legacySession())

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(legacySealChunk(t, r.ID, 0, "stdout", "hel"))
		br.HandleInbound(legacySealChunk(t, r.ID, 1, "stdout", "lo"))
		// 关键：0.0.x 到此为止，不会再发 Fin。
		br.HandleInbound(legacySealResp(t, r.ID, `{"exit_code":0}`))
	}()

	var mu sync.Mutex
	var stdout string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	out, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(stream string, data []byte) {
			mu.Lock()
			defer mu.Unlock()
			stdout += string(data)
		})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("v1 无终止帧的流式调用不应失败: %v", err)
	}
	if string(out) != `{"exit_code":0}` {
		t.Fatalf("result=%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if stdout != "hello" {
		t.Fatalf("stdout=%q，想要 \"hello\"", stdout)
	}
	// 也不该为了等一个永远不会来的终止帧白白耗满补齐窗口。
	if elapsed >= streamGapSettle {
		t.Fatalf("v1 下不该等待终止帧，实际耗时 %v（补齐窗口 %v）", elapsed, streamGapSettle)
	}
}

// TestBridgeV2StreamStillRequiresTerminator 反向锁定：v2 会话下同样的帧序列必须失败。
//
// 与上一条成对存在。只有上一条的话，把终止帧检查整个删掉测试也全绿，而那会让 v2 丢掉
// "区分「真的没有更多输出」和「最后一帧刚好丢了」"的能力。
func TestBridgeV2StreamStillRequiresTerminator(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, proto.Session{Key: streamKey})

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "hel"))
		br.HandleInbound(sealChunk(t, r.ID, 1, "stdout", "lo"))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(stream string, data []byte) {})
	if err == nil || !strings.Contains(err.Error(), "stream_incomplete") {
		t.Fatalf("v2 缺终止帧必须判 stream_incomplete，得到 %v", err)
	}
}

// TestBridgeLegacyStreamStillDetectsGaps v1 放宽的只是终止帧，缺帧检测照旧 —— Seq 在
// v1 下一样连续，一份被挖空的输出不能当成功返回。
func TestBridgeLegacyStreamStillDetectsGaps(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, legacySession())

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(legacySealChunk(t, r.ID, 0, "stdout", "hel"))
		// seq=1 丢失
		br.HandleInbound(legacySealChunk(t, r.ID, 2, "stdout", "lo"))
		br.HandleInbound(legacySealResp(t, r.ID, `{"exit_code":0}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(stream string, data []byte) {})
	if err == nil || !strings.Contains(err.Error(), "stream_incomplete") {
		t.Fatalf("v1 下缺帧仍必须判 stream_incomplete，得到 %v", err)
	}
}
