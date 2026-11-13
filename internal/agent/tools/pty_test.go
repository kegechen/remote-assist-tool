package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePTY 用管道模拟终端：写入的内容原样回显到输出。
type fakePTY struct {
	pr, pw   *io.PipeReader
	outW     *io.PipeWriter
	inR      *io.PipeReader
	exitCh   chan struct{}
	killOnce sync.Once
	mu       sync.Mutex
	cols     uint16
	rows     uint16
	kills    int
}

func newFakePTY(spec ptySpec) *fakePTY {
	pr, pw := io.Pipe()
	f := &fakePTY{pr: pr, outW: pw, exitCh: make(chan struct{}), cols: spec.Cols, rows: spec.Rows}
	return f
}

func (f *fakePTY) Read(p []byte) (int, error) { return f.pr.Read(p) }
func (f *fakePTY) Write(p []byte) (int, error) {
	select {
	case <-f.exitCh:
		return 0, io.ErrClosedPipe
	default:
	}
	return f.outW.Write(p) // 回显
}
func (f *fakePTY) Resize(c, r uint16) error {
	f.mu.Lock()
	f.cols, f.rows = c, r
	f.mu.Unlock()
	return nil
}
func (f *fakePTY) Wait() int { <-f.exitCh; return 7 }
func (f *fakePTY) Kill() {
	f.killOnce.Do(func() {
		f.mu.Lock()
		f.kills++
		f.mu.Unlock()
		close(f.exitCh)
	})
}
func (f *fakePTY) Finish() { f.outW.Close() }
func (f *fakePTY) Close()  { f.Kill(); f.outW.Close() }

type ptyCaptureSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *ptyCaptureSink) Send(stream string, data []byte) error {
	c.mu.Lock()
	c.buf.Write(data)
	c.mu.Unlock()
	return nil
}
func (c *ptyCaptureSink) String() string { c.mu.Lock(); defer c.mu.Unlock(); return c.buf.String() }

// fakePTYList 记录 m.start 造出的假终端。start 跑在 pty_open 的 goroutine 里，测试 goroutine
// 必须经锁读取；而且会话登记（m.Count()==1）早于 start 被调用，读之前要等元素真的出现。
type fakePTYList struct {
	mu   sync.Mutex
	list []*fakePTY
}

func (l *fakePTYList) add(f *fakePTY) {
	l.mu.Lock()
	l.list = append(l.list, f)
	l.mu.Unlock()
}

// at 等第 i 个假终端出现后返回。
func (l *fakePTYList) at(t *testing.T, i int) *fakePTY {
	t.Helper()
	var f *fakePTY
	waitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		if len(l.list) > i {
			f = l.list[i]
			return true
		}
		return false
	}, "假终端创建")
	return f
}

func (f *fakePTY) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kills
}

func newFakeTools(t *testing.T, disabled bool) (*PTYManager, *ptyOpenTool, *ptyWriteTool, *ptyResizeTool, *ptyCloseTool, *fakePTYList) {
	m := NewPTYManager(disabled)
	made := &fakePTYList{}
	m.start = func(spec ptySpec) (ptyProc, error) {
		f := newFakePTY(spec)
		made.add(f)
		return f, nil
	}
	return m, &ptyOpenTool{m}, &ptyWriteTool{m}, &ptyResizeTool{m}, &ptyCloseTool{m}, made
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	// 给足 10 秒：并行跑全量测试或机器繁忙时，goroutine 启动可能延迟，固定 2 秒会偶发超时
	for i := 0; i < 1000; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("超时等待: %s", what)
}

func TestPTYSessionLifecycleAndEcho(t *testing.T) {
	m, open, write, resize, closeT, made := newFakeTools(t, false)
	sink := &ptyCaptureSink{}
	done := make(chan json.RawMessage, 1)
	go func() {
		res, err := open.Run(context.Background(), json.RawMessage(`{"session":"s1","cols":100,"rows":30}`), sink)
		if err != nil {
			t.Errorf("open: %v", err)
		}
		done <- res
	}()
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")

	// 重复 id 被拒
	if _, err := open.Run(context.Background(), json.RawMessage(`{"session":"s1"}`), sink); err == nil {
		t.Fatal("重复 session id 应被拒绝")
	}
	// 写入回显（字节原样，含非 UTF-8）
	payload := []byte{0x03, 'h', 'i', 0xff, 0xfe, '\r'}
	args, _ := json.Marshal(map[string]any{"session": "s1", "data": payload})
	if _, err := write.Run(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sink.String() == string(payload) }, "回显")

	if _, err := resize.Run(context.Background(), json.RawMessage(`{"session":"s1","cols":120,"rows":40}`), nil); err != nil {
		t.Fatal(err)
	}
	f := made.at(t, 0)
	f.mu.Lock()
	if f.cols != 120 || f.rows != 40 {
		t.Fatalf("resize 未生效: %d x %d", f.cols, f.rows)
	}
	f.mu.Unlock()

	res, err := closeT.Run(context.Background(), json.RawMessage(`{"session":"s1"}`), nil)
	if err != nil || !strings.Contains(string(res), `"closed":true`) {
		t.Fatalf("close: %s %v", res, err)
	}
	select {
	case r := <-done:
		var out ptyOpenResult
		json.Unmarshal(r, &out)
		if out.Reason != "closed" || out.ExitCode != 7 {
			t.Fatalf("open 结果不符: %s", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pty_open 在 close 后未返回")
	}
	if m.Count() != 0 {
		t.Fatal("会话表应已清空")
	}
	// 幂等：再关一次、对已结束会话写入
	res, err = closeT.Run(context.Background(), json.RawMessage(`{"session":"s1"}`), nil)
	if err != nil || !strings.Contains(string(res), `"closed":false`) {
		t.Fatalf("重复 close 应成功且 closed=false: %s %v", res, err)
	}
	if _, err := write.Run(context.Background(), args, nil); err == nil || !strings.Contains(err.Error(), "pty_not_found") {
		t.Fatalf("对已结束会话写入应报 pty_not_found: %v", err)
	}
	if f.kills != 1 {
		t.Fatalf("Kill 应只执行一次，实际 %d", f.kills)
	}
}

func TestPTYCancelClosesSession(t *testing.T) {
	m, open, _, _, _, made := newFakeTools(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan json.RawMessage, 1)
	go func() {
		res, _ := open.Run(ctx, json.RawMessage(`{"session":"c1"}`), &ptyCaptureSink{})
		done <- res
	}()
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")
	cancel()
	select {
	case r := <-done:
		var out ptyOpenResult
		json.Unmarshal(r, &out)
		if out.Reason != "cancelled" {
			t.Fatalf("reason=%q", out.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后未返回")
	}
	if made.at(t, 0).killCount() != 1 || m.Count() != 0 {
		t.Fatal("取消应杀进程并清表")
	}
}

func TestPTYShellExitsNaturally(t *testing.T) {
	m, open, _, _, _, made := newFakeTools(t, false)
	done := make(chan json.RawMessage, 1)
	go func() {
		res, _ := open.Run(context.Background(), json.RawMessage(`{"session":"e1"}`), &ptyCaptureSink{})
		done <- res
	}()
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")
	made.at(t, 0).Kill() // 模拟 shell 自己退出
	select {
	case r := <-done:
		var out ptyOpenResult
		json.Unmarshal(r, &out)
		if out.Reason != "exited" {
			t.Fatalf("reason=%q", out.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell 退出后 pty_open 未返回")
	}
}

func TestPTYResizeValidation(t *testing.T) {
	m, open, _, resize, _, _ := newFakeTools(t, false)
	go open.Run(context.Background(), json.RawMessage(`{"session":"r1"}`), &ptyCaptureSink{})
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")
	for _, bad := range []string{
		`{"session":"r1","cols":0,"rows":10}`,
		`{"session":"r1","cols":10,"rows":0}`,
		`{"session":"r1","cols":-1,"rows":10}`,
		`{"session":"r1","cols":1001,"rows":10}`,
		`{"session":"r1","cols":10,"rows":70000}`,
		`{"session":"r1"}`,
	} {
		if _, err := resize.Run(context.Background(), json.RawMessage(bad), nil); err == nil || !strings.Contains(err.Error(), "bad args") {
			t.Errorf("%s 应报 bad args，得到 %v", bad, err)
		}
	}
	if _, err := resize.Run(context.Background(), json.RawMessage(`{"session":"nope","cols":10,"rows":10}`), nil); err == nil || !strings.Contains(err.Error(), "pty_not_found") {
		t.Errorf("不存在的会话应报 pty_not_found: %v", err)
	}
	m.mu.Lock()
	s := m.sessions["r1"]
	m.mu.Unlock()
	// 生产代码里调用 closeWith 的地方都先等 ready（proc 由 pty_open 在 ready 前写入），这里照做。
	if !s.waitReady() {
		t.Fatal("会话未就绪")
	}
	s.closeWith("closed")
}

func TestPTYOpenArgValidation(t *testing.T) {
	_, open, _, _, _, _ := newFakeTools(t, false)
	for _, bad := range []string{`{}`, `{"session":"a b"}`, `{"session":"ok","cols":5000}`, `not json`} {
		if _, err := open.Run(context.Background(), json.RawMessage(bad), &ptyCaptureSink{}); err == nil {
			t.Errorf("%s 应被拒绝", bad)
		}
	}
}

func TestPTYDisabledByDefault(t *testing.T) {
	for _, tool := range NewPTYTools(true) {
		_, err := tool.Run(context.Background(), json.RawMessage(`{"session":"x","cols":10,"rows":10,"data":""}`), &ptyCaptureSink{})
		if err == nil || !strings.Contains(err.Error(), "exec_denied") {
			t.Errorf("%s 在 disabled 下应返回 exec_denied: %v", tool.Name(), err)
		}
	}
}

// 真终端由 share 决定是否开放（默认开，设 --allow-exec/--root 时关）；此处只验证策略函数语义。
func TestPTYDisabledPolicy(t *testing.T) {
	if !PTYDisabled(false) {
		t.Error("allowPTY=false 必须禁用真终端")
	}
	if PTYDisabled(true) {
		t.Error("allowPTY=true 应开放真终端")
	}
}

func TestPTYDisabledMessageMentionsAllowPTY(t *testing.T) {
	if !strings.Contains(errPTYDisabled.Error(), "--allow-exec") || !strings.Contains(errPTYDisabled.Error(), "exec_denied") {
		t.Errorf("禁用提示应包含 exec_denied 与 --allow-exec: %v", errPTYDisabled)
	}
}

func TestPTYMaxSessions(t *testing.T) {
	m, open, _, _, _, _ := newFakeTools(t, false)
	for i := 0; i < ptyMaxSessions; i++ {
		id, _ := json.Marshal(map[string]string{"session": "m" + string(rune('a'+i))})
		go open.Run(context.Background(), id, &ptyCaptureSink{})
	}
	waitFor(t, func() bool { return m.Count() == ptyMaxSessions }, "会话占满")
	if _, err := open.Run(context.Background(), json.RawMessage(`{"session":"over"}`), &ptyCaptureSink{}); err == nil || !strings.Contains(err.Error(), "server_busy") {
		t.Fatalf("超限应 server_busy: %v", err)
	}
	// 先在锁内取出会话再逐个关：等 ready 期间不能持有 m.mu（pty_open 就绪前的 m.touch 也要拿它）；
	// 生产代码里调用 closeWith 的地方都先等 ready（proc 由 pty_open 在 ready 前写入），这里照做。
	m.mu.Lock()
	var all []*ptySession
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		if s.waitReady() {
			s.closeWith("closed")
		}
	}
}

func TestPTYInputNeverLogged(t *testing.T) {
	var logBuf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(old)
	m, open, write, _, closeT, _ := newFakeTools(t, false)
	sink := &ptyCaptureSink{}
	go open.Run(context.Background(), json.RawMessage(`{"session":"l1"}`), sink)
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")
	secret := "SuperSecretPassword123"
	args, _ := json.Marshal(map[string]any{"session": "l1", "data": []byte(secret)})
	if _, err := write.Run(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sink.String() == secret }, "回显")
	closeT.Run(context.Background(), json.RawMessage(`{"session":"l1"}`), nil)
	if strings.Contains(logBuf.String(), secret) {
		t.Fatalf("输入内容泄漏进日志: %s", logBuf.String())
	}
}
