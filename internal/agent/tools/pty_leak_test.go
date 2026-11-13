//go:build windows || linux

package tools

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// TestPTYSessionReleasesGoroutines 反复开关真实终端会话，断言 goroutine 数回落到基线（无泄漏、无常驻 goroutine）。
func TestPTYSessionReleasesGoroutines(t *testing.T) {
	shell, _ := treeTestCommand()
	m := NewPTYManager(false)
	open, closeT := &ptyOpenTool{m}, &ptyCloseTool{m}
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		sink := &ptyCaptureSink{}
		args, _ := json.Marshal(map[string]any{"session": "leak", "shell": shell})
		done := make(chan struct{})
		go func() { open.Run(context.Background(), args, sink); close(done) }()
		waitFor(t, func() bool { return len(sink.String()) > 0 }, "shell 有输出")
		closeT.Run(context.Background(), json.RawMessage(`{"session":"leak"}`), nil)
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Fatal("pty_open 未返回")
		}
	}
	var n int
	waitFor(t, func() bool { n = runtime.NumGoroutine(); return n <= base+1 }, "goroutine 回落")
	t.Logf("基线 %d，5 次会话后 %d", base, n)
}
