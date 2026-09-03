//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup 让子进程自成一个进程组，并把 CommandContext 的取消动作从
// 「杀直接子进程」换成「杀整个进程组」。
//
// exec.CommandContext 默认的 Cancel 是 cmd.Process.Kill()，只对直接子进程发信号。
// 而 exec 工具最常见的用法恰恰是 `bash -lc "..."` / `npm run xxx` 这类壳层——SIGKILL
// 打在壳层上，它派生的编译器、测试进程、dev server 全都活着变成孤儿：既继续吃 CPU，
// 又攥着 stdout 管道不放，于是 cmd.Wait 在超时之后仍然读不到 EOF，永远回不来。
// Setpgid 让子进程的 pgid 等于它自己的 pid，kill(-pid) 就能一次覆盖整棵子树。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// 负号 = 整个进程组。ESRCH 表示组里已经没人了，按「进程已结束」上报，
		// 否则 cmd.Wait 会把它当成取消失败原样抛给调用方。
		switch err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err {
		case nil:
			return nil
		case syscall.ESRCH:
			return os.ErrProcessDone
		default:
			// Setpgid 万一没生效（极少见），至少退回到杀直接子进程。
			return cmd.Process.Kill()
		}
	}
}
