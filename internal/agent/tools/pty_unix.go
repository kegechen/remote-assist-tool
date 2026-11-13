//go:build !windows

package tools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	cpty "github.com/creack/pty"
)

// resolveShell 选 shell：显式指定 > $SHELL > bash > sh；以登录交互方式启动（-l，交互由 pty 决定）。
func resolveShell(want string) (string, []string, error) {
	cands := []string{}
	if want != "" {
		cands = append(cands, want)
	} else {
		if sh := os.Getenv("SHELL"); sh != "" {
			cands = append(cands, sh)
		}
		cands = append(cands, "bash", "sh")
	}
	for _, c := range cands {
		if p, err := exec.LookPath(c); err == nil {
			return p, []string{"-l"}, nil
		}
	}
	return "", nil, fmt.Errorf("internal_error: 找不到可用的 shell（试过 %s）", strings.Join(cands, ", "))
}

type unixPTY struct {
	cmd      *exec.Cmd
	f        *os.File
	pid      int
	killOnce sync.Once
	closeMu  sync.Once
	exited   chan struct{}
	code     int
}

// 子进程会继承父进程里“被忽略”的信号。被控端若是被 nohup/后台任务/非交互 shell 启动的，
// SIGINT、SIGHUP 在启动时就是忽略状态，shell 及其子进程（tail -f 等）继承后 Ctrl+C 会
// 回显 ^C 却杀不掉进程。Go 的规则是：启动时被忽略的信号一旦 Notify 就不再忽略；exec 时
// 被“处理”的信号会恢复为默认动作。所以 fork 前临时 Notify，fork 后立即 Stop，
// 既让终端里的子进程拿到默认信号动作，又不改变被控端自身的信号行为。
var sigResetMu sync.Mutex

func withDefaultSignals(start func() error) error {
	sigResetMu.Lock()
	defer sigResetMu.Unlock()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)
	defer signal.Stop(ch)
	return start()
}

func startPTY(spec ptySpec) (ptyProc, error) {
	cmd := exec.Command(spec.Shell, spec.Args...)
	cmd.Dir = spec.Cwd
	env := os.Environ()
	// 终端是 xterm.js，统一声明 TERM（追加在后，覆盖继承值）。
	env = append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	// Setsid 让 shell 成为新会话领头进程，Setctty 使 pty 成为其控制终端（Ctrl+C 才能变 SIGINT）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	var f *os.File
	err := withDefaultSignals(func() (e error) {
		f, e = cpty.StartWithSize(cmd, &cpty.Winsize{Cols: spec.Cols, Rows: spec.Rows})
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("internal_error: 启动 shell 失败: %w", err)
	}
	p := &unixPTY{cmd: cmd, f: f, pid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.code = 0
		if cmd.ProcessState != nil {
			p.code = cmd.ProcessState.ExitCode()
		} else if err != nil {
			p.code = -1
		}
		close(p.exited)
	}()
	return p, nil
}

func (p *unixPTY) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	if err != nil && n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (p *unixPTY) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *unixPTY) Resize(cols, rows uint16) error {
	return cpty.Setsize(p.f, &cpty.Winsize{Cols: cols, Rows: rows})
}

func (p *unixPTY) Wait() int {
	<-p.exited
	return p.code
}

// Kill 杀掉整个会话：进程组 + 会话内所有进程 + 后代。交互 shell 的前台作业各自是独立进程组，
// 光杀 -pid 不够，所以按「会话 id 相同」与「父子关系」再扫一遍；扫两轮防止竞态中新 fork 的漏网。
func (p *unixPTY) Kill() {
	p.killOnce.Do(func() {
		for i := 0; i < 3; i++ {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			victims := collectTree(p.pid)
			for _, v := range victims {
				_ = syscall.Kill(v, syscall.SIGKILL)
			}
			if len(victims) == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	})
}

func (p *unixPTY) Finish() {}

// Interrupt 绕过可能被卡住的输入写入：取 PTY 的前台进程组（TIOCGPGRP），直接递送控制字符对应的信号。
// 用 SyscallConn 而非 Fd()，避免把 master 切成阻塞模式而影响 Read/Close 行为。
func (p *unixPTY) Interrupt(b byte) bool {
	var sig syscall.Signal
	switch b {
	case 0x03:
		sig = syscall.SIGINT
	case 0x1a:
		sig = syscall.SIGTSTP
	case 0x1c:
		sig = syscall.SIGQUIT
	default:
		return false
	}
	rc, err := p.f.SyscallConn()
	if err != nil {
		return false
	}
	var pgrp int32
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp)))
	}); err != nil || errno != 0 || pgrp <= 0 {
		return false
	}
	return syscall.Kill(-int(pgrp), sig) == nil
}

func (p *unixPTY) Close() {
	p.Kill()
	p.closeMu.Do(func() { _ = p.f.Close() })
}

// Pid 返回 shell 进程号（测试与诊断用）。
func (p *unixPTY) Pid() int { return p.pid }
