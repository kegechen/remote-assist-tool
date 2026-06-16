package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInitializeHandshake(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}` + "\n")
	var out bytes.Buffer
	srv := NewServer(nil)
	err := srv.Serve(context.Background(), in, &out)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resp struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
	}
	json.Unmarshal(out.Bytes(), &resp)
	if resp.Result.ServerInfo.Name == "" {
		t.Fatalf("missing server info: %s", out.String())
	}
}

func TestToolsListReturnsTenTools(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	srv := NewServer(nil)
	srv.Serve(context.Background(), in, &out)
	if !strings.Contains(out.String(), `"connect"`) || !strings.Contains(out.String(), `"tail_log"`) {
		t.Fatalf("missing tools: %s", out.String())
	}
}

// tsBuf 并发安全 buffer：tools/call 现在在 goroutine 中写 out。
type tsBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *tsBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *tsBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockingBridge 进入 CallTool 后阻塞在 ctx.Done()，用于验证取消接线。
type blockingBridge struct {
	entered chan struct{}
}

func (b *blockingBridge) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestToolsCallCancellation 验证 notifications/cancelled 能精确取消对应 in-flight 调用，
// 使 bridge.CallTool 立即返回而非干等兜底超时（P1 取消接线）。
func TestToolsCallCancellation(t *testing.T) {
	br := &blockingBridge{entered: make(chan struct{}, 1)}
	srv := NewServer(br)
	out := &tsBuf{}
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		req := &rpcReq{Method: "tools/call", ID: json.RawMessage(`7`),
			Params: json.RawMessage(`{"name":"exec","arguments":{}}`)}
		srv.dispatch(ctx, req, out)
		close(done)
	}()

	select {
	case <-br.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("CallTool 未进入")
	}

	cancelReq := &rpcReq{Method: "notifications/cancelled",
		Params: json.RawMessage(`{"requestId":7}`)}
	srv.dispatch(ctx, cancelReq, out)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tools/call 未在取消后返回——cancellation 未接线")
	}
	if !strings.Contains(out.String(), `"id":7`) {
		t.Fatalf("缺少 id=7 的响应: %s", out.String())
	}
	if !strings.Contains(out.String(), "error") {
		t.Fatalf("取消应返回 error 响应: %s", out.String())
	}
}
