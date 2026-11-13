package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestPTYKillsWholeProcessTree 起真实 shell 并派生子进程，关闭会话后断言子孙进程全部消失。
func TestPTYKillsWholeProcessTree(t *testing.T) {
	shell, input := treeTestCommand()
	m := NewPTYManager(false)
	open, write, closeT := &ptyOpenTool{m}, &ptyWriteTool{m}, &ptyCloseTool{m}
	sink := &ptyCaptureSink{}
	reqArgs, _ := json.Marshal(map[string]any{"session": "tree1", "shell": shell, "cols": 100, "rows": 30})
	done := make(chan json.RawMessage, 1)
	go func() {
		res, err := open.Run(context.Background(), reqArgs, sink)
		if err != nil {
			t.Errorf("open: %v", err)
		}
		t.Logf("open 返回: %s sink=%q", res, sink.String())
		done <- res
	}()
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")
	m.mu.Lock()
	s := m.sessions["tree1"]
	m.mu.Unlock()
	<-s.ready
	pp, ok := s.proc.(interface{ Pid() int })
	if !ok {
		t.Skip("后端不提供 Pid")
	}
	shellPid := pp.Pid()
	time.Sleep(800 * time.Millisecond) // 等 shell 起来
	wa, _ := json.Marshal(map[string]any{"session": "tree1", "data": []byte(input)})
	if _, err := write.Run(context.Background(), wa, nil); err != nil {
		t.Fatal(err)
	}
	var kids []int
	waitFor(t, func() bool {
		kids = descendantsOf(shellPid)
		// windows: cmd 下有 ping（及 conhost 之外）；unix: 两个 sleep
		return len(kids) >= 1
	}, "出现子孙进程")
	time.Sleep(300 * time.Millisecond)
	kids = descendantsOf(shellPid)
	all := append([]int{shellPid}, kids...)
	t.Logf("shell=%d 子孙=%v", shellPid, kids)

	if _, err := closeT.Run(context.Background(), json.RawMessage(`{"session":"tree1"}`), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pty_open 未返回")
	}
	waitFor(t, func() bool {
		for _, p := range all {
			if pidAlive(p) {
				return false
			}
		}
		return true
	}, "shell 及子孙进程全部退出")
	for _, p := range all {
		if pidAlive(p) {
			t.Errorf("进程 %d 残留", p)
		}
	}
}

// TestPTYRealShellOutput 真实 shell 能收到提示符输出（冒烟）。
func TestPTYRealShellOutput(t *testing.T) {
	shell, _ := treeTestCommand()
	m := NewPTYManager(false)
	open, closeT := &ptyOpenTool{m}, &ptyCloseTool{m}
	sink := &ptyCaptureSink{}
	args, _ := json.Marshal(map[string]any{"session": "smoke", "shell": shell})
	go open.Run(context.Background(), args, sink)
	waitFor(t, func() bool { return len(sink.String()) > 0 }, "shell 有输出")
	closeT.Run(context.Background(), json.RawMessage(`{"session":"smoke"}`), nil)
	waitFor(t, func() bool { return m.Count() == 0 }, "会话清理")
}
