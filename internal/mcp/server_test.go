package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// TestHostHealthCheckLifecycle 模拟 MCP 宿主的完整 stdio 生命周期：握手后发
// ping 健康检查，继续使用工具目录，空闲一段时间，最后由宿主关闭 stdin。
func TestHostHealthCheckLifecycle(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		inputWriter.Close()
		inputReader.Close()
	})
	output := newLineSink()
	done := make(chan error, 1)
	go func() {
		done <- NewServer(nil).Serve(context.Background(), inputReader, output)
	}()

	initialRequests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	if _, err := io.WriteString(inputWriter, initialRequests); err != nil {
		t.Fatalf("write host requests: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(output.lines()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lines := output.lines(); len(lines) != 2 {
		t.Fatalf("initial responses=%d, want 2: %v", len(lines), lines)
	}
	select {
	case err := <-done:
		t.Fatalf("server exited while host stdin was open: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	afterIdleRequests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":3,"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	if _, err := io.WriteString(inputWriter, afterIdleRequests); err != nil {
		t.Fatalf("write requests after idle: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(output.lines()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	lines := output.lines()
	if len(lines) != 4 {
		t.Fatalf("responses after idle=%d, want 4: %v", len(lines), lines)
	}
	for i, wantID := range []int{1, 2, 3, 4} {
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcErr         `json:"error"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &response); err != nil {
			t.Fatalf("decode response %d: %v", i, err)
		}
		if response.ID != wantID || response.Error != nil {
			t.Fatalf("response %d=%s, want successful id=%d", i, lines[i], wantID)
		}
		if wantID == 3 && string(response.Result) != "{}" {
			t.Fatalf("ping result=%s, want {}", response.Result)
		}
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close host stdin: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("host shutdown returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after host closed stdin")
	}
}

type terminalErrorReader struct {
	reader io.Reader
	err    error
}

func (r *terminalErrorReader) Read(p []byte) (int, error) {
	if r.reader != nil {
		n, err := r.reader.Read(p)
		if !errors.Is(err, io.EOF) {
			return n, err
		}
		r.reader = nil
	}
	return 0, r.err
}

func TestServeTreatsClosedHostPipeAsNormalShutdown(t *testing.T) {
	input := &terminalErrorReader{
		reader: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"),
		err:    io.ErrClosedPipe,
	}
	if err := NewServer(nil).Serve(context.Background(), input, io.Discard); err != nil {
		t.Fatalf("closed host pipe should be a normal shutdown: %v", err)
	}
}

func TestServePreservesUnexpectedInputError(t *testing.T) {
	want := errors.New("unexpected stdin failure")
	input := &terminalErrorReader{err: want}
	if err := NewServer(nil).Serve(context.Background(), input, io.Discard); !errors.Is(err, want) {
		t.Fatalf("Serve error=%v, want %v", err, want)
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
