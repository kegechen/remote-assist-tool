package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeServer 冒充 remote 子进程的另一端：读 client 发来的请求，回响应与流式通知。
type fakeServer struct {
	toClient   *io.PipeWriter
	fromClient *bufio.Scanner
}

// newFakeClient 造一个不拉子进程的 MCPClient，两端用 io.Pipe 对接。
func newFakeClient() (*MCPClient, *fakeServer) {
	srvToCliR, srvToCliW := io.Pipe()
	cliToSrvR, cliToSrvW := io.Pipe()
	c := &MCPClient{
		stdin:    cliToSrvW,
		nextID:   1,
		pending:  make(map[int64]chan rpcResponse),
		streamQs: make(map[int64]chan chunkMsg),
		done:     make(chan struct{}),
	}
	c.out = bufio.NewScanner(srvToCliR)
	go c.readLoop()
	return c, &fakeServer{toClient: srvToCliW, fromClient: bufio.NewScanner(cliToSrvR)}
}

// readReq 读一条 client 发来的请求，返回它的 id。
func (s *fakeServer) readReq(t *testing.T) int64 {
	t.Helper()
	if !s.fromClient.Scan() {
		t.Fatal("client 没发出请求")
	}
	var r struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(s.fromClient.Bytes(), &r); err != nil {
		t.Fatalf("解析请求: %v", err)
	}
	return r.ID
}

func (s *fakeServer) notify(t *testing.T, id int64, stream, data string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  streamNotifyMethod,
		"params":  map[string]any{"requestId": id, "stream": stream, "data": []byte(data)},
	})
	if _, err := s.toClient.Write(append(b, '\n')); err != nil {
		t.Fatalf("发通知: %v", err)
	}
}

// notifyQuiet 同 notify，但不碰 *testing.T —— 供并发 goroutine 使用（t.Fatalf 只能在
// 测试主 goroutine 里调）。
func (s *fakeServer) notifyQuiet(id int64, stream, data string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  streamNotifyMethod,
		"params":  map[string]any{"requestId": id, "stream": stream, "data": []byte(data)},
	})
	s.toClient.Write(append(b, '\n'))
}

// readNotify 读一条 client 发来的通知帧，返回 method 与 params.requestId。
func (s *fakeServer) readNotify(t *testing.T) (string, int64) {
	t.Helper()
	if !s.fromClient.Scan() {
		t.Fatal("client 没发出通知")
	}
	var r struct {
		Method string `json:"method"`
		Params struct {
			RequestID int64 `json:"requestId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(s.fromClient.Bytes(), &r); err != nil {
		t.Fatalf("解析通知: %v", err)
	}
	return r.Method, r.Params.RequestID
}

func (s *fakeServer) respond(t *testing.T, id int64, result string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": json.RawMessage(result),
	})
	if _, err := s.toClient.Write(append(b, '\n')); err != nil {
		t.Fatalf("发响应: %v", err)
	}
}

// TestCallToolStreamDeliversChunksBeforeResult 块按序回调，且全部在结果返回之前送达
// ——前端据此"先渲染输出、最后打 exit"，顺序反了输出就会掉在 exit 后面。
func TestCallToolStreamDeliversChunksBeforeResult(t *testing.T) {
	c, srv := newFakeClient()
	defer c.Close()

	type call struct {
		res json.RawMessage
		err error
	}
	done := make(chan call, 1)
	var mu sync.Mutex
	var got string
	go func() {
		res, err := c.CallToolStream(context.Background(), "exec",
			map[string]any{"argv": []string{"x"}, "stream": true},
			func(stream string, data []byte) {
				mu.Lock()
				got += string(data)
				mu.Unlock()
			})
		done <- call{res, err}
	}()

	id := srv.readReq(t)
	srv.notify(t, id, "stdout", "hel")
	srv.notify(t, id, "stdout", "lo")
	srv.respond(t, id, `{"exit_code":0}`)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("call: %v", r.err)
		}
		if string(r.res) != `{"exit_code":0}` {
			t.Fatalf("result=%s", r.res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("流式调用没返回")
	}
	// CallToolStream 返回时，所有块必须已经回调完（内部排空 pump 后才返回）
	mu.Lock()
	defer mu.Unlock()
	if got != "hello" {
		t.Fatalf("块内容/顺序错: %q", got)
	}
}

// TestSlowStreamConsumerDoesNotBlockOtherCalls 是这层最要紧的保险：readLoop 是单
// goroutine，如果它同步回调消费者，一个卡住的浏览器就能拖死这个 client 上的所有调用
// （本仓库为"一条卡住全卡死"付过代价）。这里把消费者钉死，另一条调用仍必须畅通。
func TestSlowStreamConsumerDoesNotBlockOtherCalls(t *testing.T) {
	c, srv := newFakeClient()
	defer c.Close()

	gate := make(chan struct{})
	streamDone := make(chan error, 1)
	go func() {
		_, err := c.CallToolStream(context.Background(), "exec",
			map[string]any{"stream": true},
			func(string, []byte) { <-gate }) // 消费者卡死不动
		streamDone <- err
	}()

	streamID := srv.readReq(t)
	for i := 0; i < 5; i++ {
		srv.notify(t, streamID, "stdout", "x")
	}

	// 消费者卡着的同时，另一条普通调用必须照常完成
	other := make(chan error, 1)
	go func() {
		_, err := c.call(context.Background(), "tools/list", nil)
		other <- err
	}()
	otherID := srv.readReq(t)
	srv.respond(t, otherID, `{"tools":[]}`)
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("第二条调用: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("流式消费者卡住时第二条调用也被堵死——readLoop 被慢消费者拖住了")
	}

	close(gate) // 放行消费者，流式调用随后收尾
	srv.respond(t, streamID, `{"exit_code":0}`)
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("流式调用: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("流式调用没收尾")
	}
}

// TestStreamCancelNotifiesSubprocess ctx 取消时必须把 notifications/cancelled 发给子进程。
//
// 这是让远端命令停下来的**唯一**入口：ctx 只活在本进程里，子进程那次 tools/call 有它自己的
// ctx。少了这条通知，用户关掉标签页后远端命令照跑不误，块继续吐进没人要的队列。
// （曾经这里只是 return，注释却写着"取消会一路传到 bridge"——链子在第一环就断了。）
func TestStreamCancelNotifiesSubprocess(t *testing.T) {
	c, srv := newFakeClient()
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.CallToolStream(ctx, "exec", map[string]any{"stream": true}, func(string, []byte) {})
	}()
	id := srv.readReq(t)

	cancel()
	// 取消必须立刻脱身，不能卡在"发通知"上——stdin 写没有超时，子进程不读就会一直堵着，
	// 那样等于让取消卡在它想取消的东西上（所以通知是异步发的）。
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后调用没能及时返回（八成同步阻塞在发通知上了）")
	}

	method, reqID := srv.readNotify(t)
	if method != "notifications/cancelled" {
		t.Fatalf("method=%q，想要 notifications/cancelled", method)
	}
	if reqID != id {
		t.Fatalf("requestId=%d，想要 %d（对不上的话子进程取消的是别的调用）", reqID, id)
	}
}

// TestStreamCtxCancelRacesChunks 复现"关页面时正好还有块在路上"。
//
// 这条竞态真实可达：调用除了被响应唤醒，还会被 ctx 取消唤醒——而 GUI 的 /api/exec/stream
// 用的正是 r.Context()，用户关掉标签页它立刻取消，此时远端命令还在吐块。取消不经过
// readLoop，于是"handleNotify 已取出队列、正要投递"与"callStream 收尾关队列"能真正重叠。
// 若两者不在同一把锁下，这里就是 send on closed channel，直接崩掉整个 GUI 进程
// （readLoop 是裸 goroutine，没人 recover）。
func TestStreamCtxCancelRacesChunks(t *testing.T) {
	for round := 0; round < 300; round++ {
		c, srv := newFakeClient()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.CallToolStream(ctx, "exec", map[string]any{"stream": true}, func(string, []byte) {})
		}()
		id := srv.readReq(t)
		go func() {
			for i := 0; i < 30; i++ {
				srv.notifyQuiet(id, "stdout", "x")
			}
		}()
		cancel() // 与上面的块投递并发：正是关页面那一刻
		<-done
		c.Close()
	}
}

// TestStreamChunksAfterCallEndDoNotPanic 调用结束后队列已关闭，迟到的通知不能往已关的
// channel 里发（会 panic 掉整个 GUI 进程——readLoop 是裸 goroutine，没人兜）。
func TestStreamChunksAfterCallEndDoNotPanic(t *testing.T) {
	c, srv := newFakeClient()
	defer c.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.CallToolStream(context.Background(), "exec", map[string]any{"stream": true}, func(string, []byte) {})
	}()
	id := srv.readReq(t)
	srv.respond(t, id, `{"exit_code":0}`)
	<-done

	// 调用已返回、队列已关：这些块必须被静默丢弃
	for i := 0; i < 3; i++ {
		srv.notify(t, id, "stdout", "late")
	}
	// 再跑一条调用确认 readLoop 还活着（若上面 panic 了，这里必然超时）
	next := make(chan error, 1)
	go func() { _, err := c.call(context.Background(), "tools/list", nil); next <- err }()
	nextID := srv.readReq(t)
	srv.respond(t, nextID, `{"tools":[]}`)
	select {
	case err := <-next:
		if err != nil {
			t.Fatalf("后续调用: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("迟到的块把 readLoop 弄挂了")
	}
}
