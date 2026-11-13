//go:build windows

package tools

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// 真实 ConPTY：不经输入管道，Interrupt(0x03) 经助手子进程向 shell 的控制台发 CTRL_C_EVENT，
// 前台的 ping -t 被中断并打印统计信息；同时本（share）进程自身不受影响。
func TestWinPTYInterruptStopsForegroundPing(t *testing.T) {
	p, err := startPTY(ptySpec{Shell: "cmd.exe", Cols: 120, Rows: 30})
	if err != nil {
		t.Skipf("无法启动 ConPTY: %v", err)
	}
	defer p.Close()
	in, ok := p.(ptyInterrupter)
	if !ok {
		t.Fatal("winPTY 应实现 ptyInterrupter")
	}

	var mu sync.Mutex
	var sb strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			mu.Lock()
			sb.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	has := func(s string) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(sb.String(), s)
	}
	waitFor := func(s string, d time.Duration) bool {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if has(s) {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}

	if _, err := p.Write([]byte("ping -t 127.0.0.1\r")); err != nil {
		t.Fatal(err)
	}
	if !waitFor("TTL=", 10*time.Second) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("ping 未开始: %q", sb.String())
	}
	if !in.Interrupt(0x03) {
		t.Fatal("Interrupt(0x03) 返回 false")
	}
	// 语言无关的判据：ping 被中断后 shell 才会执行排队的 echo。输入回显里是 ALI^VE，输出才是 ALIVE。
	if _, err := p.Write([]byte("echo ALI^VE\r")); err != nil {
		t.Fatal(err)
	}
	if !waitFor("ALIVE", 5*time.Second) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("ping 未被 Ctrl+C 中断: %q", sb.String())
	}
	// Ctrl+Z 在 Windows 没有等价信号。
	if in.Interrupt(0x1a) {
		t.Fatal("Interrupt(0x1a) 应返回 false")
	}
}
