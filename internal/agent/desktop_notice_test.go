package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// failingTool 总是失败，模拟未开启远程桌面时的 desktop_disabled。
type failingTool struct{ name string }

func (f *failingTool) Name() string { return f.name }
func (f *failingTool) Run(ctx context.Context, in json.RawMessage, out StreamSink) (json.RawMessage, error) {
	return nil, errors.New("desktop_disabled: off")
}

type deskRecorder struct {
	mu  sync.Mutex
	got []string
}

func (r *deskRecorder) add(tool string) { r.mu.Lock(); r.got = append(r.got, tool); r.mu.Unlock() }
func (r *deskRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// callTool 投递一次请求并等到响应，保证钩子（在发响应前调用）已执行。
// 会话 key 非零时 daemon 只接受 AEAD 封装的参数，这里按真实客户端的方式封装。
func callTool(t *testing.T, d *Daemon, out chan *proto.Message, sess proto.Session, id uint64, tool string) {
	t.Helper()
	sealed, err := proto.AEADSealJSON(&sess.Key, []byte(`{}`), sess.ReqAAD(id, tool, 0))
	if err != nil {
		t.Fatal(err)
	}
	req := proto.ToolReq{ID: id, Tool: tool, ArgsJSON: sealed}
	msg, _ := proto.NewMessage(proto.MsgToolReq, &req)
	d.Inject(msg)
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no response", tool)
	}
}

// 远程桌面的成功调用在控制台是静默的，OnDesktopUse 是被协助方唯一能看到的提示：
// 每个会话里首次看屏、首次操控各提示一次；失败（如未开启）不提示；新会话重新提示。
func TestDaemonDesktopUseNoticeOncePerSession(t *testing.T) {
	out := make(chan *proto.Message, 16)
	conn := &fakeConn{in: make(chan *proto.Message, 1), out: out}
	r := NewRegistry()
	r.Register(&fakeTool{name: "screen_capture"})
	r.Register(&fakeTool{name: "input"})
	r.Register(&fakeTool{name: "exec"})
	sess := proto.Session{Key: [32]byte{1}}
	d := NewDaemon(r, conn, sess)
	rec := &deskRecorder{}
	d.OnDesktopUse = rec.add
	go d.RunLoop(context.Background())

	var id uint64
	next := func() uint64 { id++; return id }

	callTool(t, d, out, sess, next(), "exec")
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("exec must not trigger desktop notice, got %v", got)
	}
	for i := 0; i < 3; i++ {
		callTool(t, d, out, sess, next(), "screen_capture")
	}
	callTool(t, d, out, sess, next(), "input")
	callTool(t, d, out, sess, next(), "input")
	if got := rec.snapshot(); len(got) != 2 || got[0] != "screen_capture" || got[1] != "input" {
		t.Fatalf("want one notice per tool per session [screen_capture input], got %v", got)
	}

	// 新的协助端握手（换 key）后应重新提示。
	sess = proto.Session{Key: [32]byte{2}}
	d.RotateSession(sess)
	id = 0 // 新会话的抗重放窗口已重置
	callTool(t, d, out, sess, next(), "screen_capture")
	if got := rec.snapshot(); len(got) != 3 || got[2] != "screen_capture" {
		t.Fatalf("new session must notify again, got %v", got)
	}
}

func TestDaemonDesktopUseNoticeSkipsFailedCalls(t *testing.T) {
	out := make(chan *proto.Message, 4)
	conn := &fakeConn{in: make(chan *proto.Message, 1), out: out}
	r := NewRegistry()
	r.Register(&failingTool{name: "screen_capture"})
	sess := proto.Session{Key: [32]byte{1}}
	d := NewDaemon(r, conn, sess)
	rec := &deskRecorder{}
	d.OnDesktopUse = rec.add
	go d.RunLoop(context.Background())

	callTool(t, d, out, sess, 1, "screen_capture")
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("failed call (desktop disabled) must not claim the screen is being viewed, got %v", got)
	}
}

// 旧会话的在途请求在换 key 之后才完成：不得占掉新会话的「已提示」位。
func TestDaemonDesktopUseStaleSessionDoesNotConsumeNotice(t *testing.T) {
	r := NewRegistry()
	oldSess := proto.Session{Key: [32]byte{1}}
	d := NewDaemon(r, &fakeConn{in: make(chan *proto.Message, 1), out: make(chan *proto.Message, 1)}, oldSess)
	rec := &deskRecorder{}
	d.OnDesktopUse = rec.add
	newSess := proto.Session{Key: [32]byte{2}}
	d.RotateSession(newSess)
	d.noteDesktopUse(oldSess, "input") // 旧协助端的请求晚到
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("旧会话的请求不应计入新会话，got %v", got)
	}
	d.noteDesktopUse(newSess, "input") // 新协助端第一次操控
	if got := rec.snapshot(); len(got) != 1 || got[0] != "input" {
		t.Fatalf("新会话首次操控必须提示且只提示一次，got %v", got)
	}
}
