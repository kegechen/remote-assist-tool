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

// sealChunkAs 用 wantSeq/wantStream 加密，却把帧标成 seq/stream 发出——模拟"把某一帧
// 挪到别的位置重放"。
func sealChunkAs(t *testing.T, id uint64, sealSeq uint32, sealStream string, seq uint32, stream, text string) *proto.Message {
	t.Helper()
	ct, err := proto.AEADSeal(&streamKey, []byte(text), proto.StreamChunkAAD(id, sealSeq, sealStream))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	msg, err := proto.NewMessage(proto.MsgToolStream, &proto.StreamChunk{ID: id, Seq: seq, Stream: stream, Data: ct})
	if err != nil {
		t.Fatalf("new msg: %v", err)
	}
	return msg
}

// TestBridgeRejectsReorderedChunk 流帧的 seq 与 stream 都是外层明文。没有 AAD 时，
// 中间人可以把 stdout 的第 3 帧标成第 1 帧重发，把远端输出重排成完全不同的内容
// （日志片段换个顺序就能改变结论）。seq/stream 进 AAD 之后，这种帧必然解密失败，
// 由 markDamaged 记成空洞，收尾时整次调用被判为不完整。
func TestBridgeRejectsReorderedChunk(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunk(t, r.ID, 0, "stdout", "ok"))
		// 按 seq=1 加密，却标成 seq=2 投出去。
		br.HandleInbound(sealChunkAs(t, r.ID, 1, "stdout", 2, "stdout", "moved"))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	var mu sync.Mutex
	var got string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(_ string, data []byte) { mu.Lock(); got += string(data); mu.Unlock() })

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(got, "moved") {
		t.Fatalf("被挪位的帧仍投给了回调: %q", got)
	}
	if err == nil {
		t.Fatal("整次调用应被判为不完整")
	}
}

// TestBridgeRejectsCrossStreamChunk 把 stderr 的帧标成 stdout 同理：能让远端的错误
// 输出伪装成正常输出。
func TestBridgeRejectsCrossStreamChunk(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		br.HandleInbound(sealChunkAs(t, r.ID, 0, "stderr", 0, "stdout", "danger"))
		br.HandleInbound(sealResp(t, r.ID, `{"exit_code":0}`))
	}()

	var mu sync.Mutex
	var got string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := br.CallToolStream(ctx, "exec", json.RawMessage(`{"argv":["x"],"stream":true}`),
		func(_ string, data []byte) { mu.Lock(); got += string(data); mu.Unlock() })

	mu.Lock()
	defer mu.Unlock()
	if got != "" {
		t.Fatalf("换了流别的帧仍被投递: %q", got)
	}
	if err == nil {
		t.Fatal("整次调用应被判为不完整")
	}
}

// TestBridgeAlwaysSealsArgs 与 daemon 的"握手后 args 必须是密文"对称：非零 key 下
// bridge 绝不能发出明文 args，"没有参数"也要封一个 "{}"。否则 daemon 那侧的
// unauthenticated 判据会把合法的无参调用全部打掉。
func TestBridgeAlwaysSealsArgs(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		if len(r.ArgsJSON) == 0 || string(r.ArgsJSON) == "null" {
			return // 没封 args：不回响应，让调用超时失败
		}
		plain, err := proto.AEADOpenJSON(&streamKey, r.ArgsJSON, proto.ToolReqAAD(r.ID, r.Tool, r.DeadlineMs))
		if err != nil || string(plain) != `{}` {
			return
		}
		br.HandleInbound(sealResp(t, r.ID, `{"ok":1}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 传 nil args：bridge 应归一成 "{}" 再加封。
	out, err := br.CallTool(ctx, "process_list", nil)
	if err != nil {
		t.Fatalf("无参调用应成功: %v", err)
	}
	if string(out) != `{"ok":1}` {
		t.Fatalf("result=%s", out)
	}
}
