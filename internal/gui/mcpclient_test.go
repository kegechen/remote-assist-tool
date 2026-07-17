package gui

import (
	"sync"
	"testing"
)

// readLoop 与 Close 会并发到达 done 的关闭点。曾经两边都用
// `select { case <-done: default: close(done) }`——这不是原子操作，两个 goroutine
// 可能都走进 default 分支，第二个 close 就 panic 掉整个 GUI 进程
// （readLoop 是裸 goroutine，panic 不会被 http 的 recover 兜住）。
func TestMarkDoneIsIdempotentUnderConcurrency(t *testing.T) {
	for round := 0; round < 500; round++ {
		c := &MCPClient{done: make(chan struct{})}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.markDone() // 并发关闭：必须幂等，不能 double-close panic
			}()
		}
		wg.Wait()
		select {
		case <-c.done:
		default:
			t.Fatal("markDone 之后 done 应已关闭")
		}
	}
}

// Close 声称幂等，且没有子进程时也不该阻塞或 panic。
func TestCloseIdempotentWithoutProcess(t *testing.T) {
	c := &MCPClient{done: make(chan struct{})}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Close() }()
	}
	wg.Wait()
	if c.IsAlive() {
		t.Error("Close 之后 IsAlive 应为 false")
	}
}

// 远端可能是 Windows 也可能是 Linux，下载文件名不能依赖本机的路径分隔符。
func TestBaseNameHandlesBothSeparators(t *testing.T) {
	cases := map[string]string{
		`C:\Users\me\crash.dmp`: "crash.dmp",
		"/var/log/app.log":      "app.log",
		`C:/mixed\sep/x.txt`:    "x.txt",
		"noslash.txt":           "noslash.txt",
	}
	for in, want := range cases {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q，期望 %q", in, got, want)
		}
	}
}
