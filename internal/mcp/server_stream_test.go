package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamCaller 同时实现 ToolCaller 与 StreamToolCaller，记录实际走了哪条路径。
type streamCaller struct {
	chunks []string // 要推的块

	mu        sync.Mutex
	sawStream bool // CallToolStream 被调用过
	sawPlain  bool // CallTool 被调用过
}

func (s *streamCaller) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	s.mu.Lock()
	s.sawPlain = true
	s.mu.Unlock()
	return json.RawMessage(`{"exit_code":0}`), nil
}

func (s *streamCaller) CallToolStream(ctx context.Context, name string, args json.RawMessage, onChunk func(string, []byte)) (json.RawMessage, error) {
	s.mu.Lock()
	s.sawStream = true
	s.mu.Unlock()
	for _, c := range s.chunks {
		onChunk("stdout", []byte(c))
	}
	return json.RawMessage(`{"exit_code":0}`), nil
}

func (s *streamCaller) paths() (stream, plain bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawStream, s.sawPlain
}

// lineSink 收集 server 写出的完整行，并在带 id 的响应帧（本次 tools/call 的收尾）
// 出现时发信号。server 分两次 Write（正文 + 换行），所以要自己攒到换行为止。
type lineSink struct {
	mu   sync.Mutex
	buf  []byte
	got  []string
	done chan struct{}
	once sync.Once
}

func newLineSink() *lineSink { return &lineSink{done: make(chan struct{})} }

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := string(s.buf[:i])
		s.buf = s.buf[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		s.got = append(s.got, line)
		// 通知帧无 id，响应帧有 —— 有 id 即本次调用收尾
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(line), &probe) == nil && len(probe.ID) > 0 {
			s.once.Do(func() { close(s.done) })
		}
	}
	return len(p), nil
}

func (s *lineSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// runCall 跑一条 tools/call 并返回 server 写出的所有行。
//
// 用 io.Pipe 而不是 strings.Reader：tools/call 是并发 dispatch 的（一条慢调用不能堵住
// 读循环），若 stdin 立刻 EOF，Serve 会在 handler 还没跑完就返回，什么都读不到。真实的
// stdin 是一直开着的，这里如实还原：写完请求后等响应到达再收尾。
func runCall(t *testing.T, caller ToolCaller, args string) []string {
	t.Helper()
	pr, pw := io.Pipe()
	defer pw.Close()
	sink := newLineSink()
	go NewServer(caller).Serve(context.Background(), pr, sink)

	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":` + args + `}}` + "\n"
	if _, err := io.WriteString(pw, req); err != nil {
		t.Fatalf("write req: %v", err)
	}
	select {
	case <-sink.done:
	case <-time.After(3 * time.Second):
		t.Fatal("超时：没等到 tools/call 的响应")
	}
	return sink.lines()
}

// TestServerEmitsStreamNotifications stream=true 时块以通知帧发出，且全部排在最终响应
// 之前；通知里带发起调用的 requestId，数据是 base64（块边界可能切断多字节字符）。
func TestServerEmitsStreamNotifications(t *testing.T) {
	caller := &streamCaller{chunks: []string{"one", "two"}}
	lines := runCall(t, caller, `{"argv":["x"],"stream":true}`)

	if streamed, _ := caller.paths(); !streamed {
		t.Fatal("stream=true 却没走 CallToolStream")
	}
	if len(lines) != 3 {
		t.Fatalf("想要 2 条通知 + 1 条响应，实际 %d 行:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, want := range []string{"one", "two"} {
		var note struct {
			Method string `json:"method"`
			Params struct {
				RequestID json.RawMessage `json:"requestId"`
				Stream    string          `json:"stream"`
				Data      string          `json:"data"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &note); err != nil {
			t.Fatalf("解析通知 %d: %v", i, err)
		}
		if note.Method != streamNotifyMethod {
			t.Fatalf("method=%q", note.Method)
		}
		if string(note.Params.RequestID) != "1" {
			t.Fatalf("requestId=%s，想要 1", note.Params.RequestID)
		}
		got, err := base64.StdEncoding.DecodeString(note.Params.Data)
		if err != nil {
			t.Fatalf("块数据不是 base64: %v", err)
		}
		if string(got) != want {
			t.Fatalf("块 %d = %q，想要 %q", i, got, want)
		}
	}
	// 最后一行必须是带 id 的响应（结果排在所有块之后）
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if string(resp.ID) != "1" || len(resp.Result) == 0 {
		t.Fatalf("最后一行不是本次调用的响应: %s", lines[2])
	}
}

// TestServerNoStreamWithoutOptIn 没传 stream 时必须走普通 CallTool、一条通知都不发。
// 这是 Claude 那条路径的保险：它不认识流式通知，多发只会是噪音。
func TestServerNoStreamWithoutOptIn(t *testing.T) {
	caller := &streamCaller{chunks: []string{"nope"}}
	lines := runCall(t, caller, `{"argv":["x"]}`)

	streamed, plain := caller.paths()
	if streamed {
		t.Fatal("没要求流式却走了 CallToolStream")
	}
	if !plain {
		t.Fatal("没走 CallTool")
	}
	if len(lines) != 1 {
		t.Fatalf("想要只有 1 条响应，实际 %d 行:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// plainCaller 只实现 ToolCaller（不支持流式）。
type plainCaller struct {
	mu     sync.Mutex
	called bool
}

func (p *plainCaller) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	p.called = true
	p.mu.Unlock()
	return json.RawMessage(`{"exit_code":0}`), nil
}

func (p *plainCaller) wasCalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called
}

// TestServerFallsBackWhenCallerCannotStream 调用方要流式、但底层 caller 不支持时，
// 退回同步调用照常返回结果，而不是报错。
func TestServerFallsBackWhenCallerCannotStream(t *testing.T) {
	caller := &plainCaller{}
	lines := runCall(t, caller, `{"argv":["x"],"stream":true}`)
	if !caller.wasCalled() {
		t.Fatal("没退回 CallTool")
	}
	if len(lines) != 1 {
		t.Fatalf("不支持流式却发了通知:\n%s", strings.Join(lines, "\n"))
	}
}
