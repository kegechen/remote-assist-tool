//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 回归：被控端若以“SIGINT/SIGHUP 被忽略”的状态启动（nohup、后台任务、非交互 shell），
// 终端里的子进程不能继承这个忽略状态，否则 Ctrl+C 只回显 ^C 却杀不掉进程。
// 做法：用 sh 的 trap "" INT HUP 让测试进程以忽略状态启动，再在其中跑真实 PTY。
func TestPTYCtrlCWithInheritedIgnoredSigint(t *testing.T) {
	if os.Getenv("PTY_SIGINT_HELPER") == "1" {
		return // 辅助子进程里由 TestPTYSigintHelper 负责
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("无 sh")
	}
	cmd := exec.Command(sh, "-c", `trap "" INT HUP; exec "$0" -test.run '^TestPTYSigintHelper$' -test.v`, os.Args[0])
	cmd.Env = append(os.Environ(), "PTY_SIGINT_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") || strings.Contains(string(out), "FAIL") {
		t.Fatalf("SIGINT 被忽略时终端内 Ctrl+C 未能中断子进程: err=%v\n%s", err, out)
	}
}

func TestPTYSigintHelper(t *testing.T) {
	if os.Getenv("PTY_SIGINT_HELPER") != "1" {
		t.Skip("仅作为子进程运行")
	}
	p, err := startPTY(ptySpec{Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	time.Sleep(300 * time.Millisecond)
	if _, err := p.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Ctrl+C 后 sleep 仍在运行：子进程继承了被忽略的 SIGINT")
	}
}
