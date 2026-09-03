//go:build windows

package tools

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// taskkillTimeout 杀树命令自己的超时。taskkill 正常几十毫秒返回；给个上限只是不让
// 取消路径被一个卡住的系统工具反过来拖住。
const taskkillTimeout = 5 * time.Second

// configureProcessGroup 把 CommandContext 的取消动作从「杀直接子进程」换成「杀整棵
// 进程树」。
//
// exec.CommandContext 默认的 Cancel 是 cmd.Process.Kill()，只终止直接子进程。而 exec
// 工具最常见的用法恰恰是 `cmd /c ...` / `npm run xxx` 这类壳层——壳层被杀掉之后，它派生
// 的编译器、测试进程、dev server 全都活着变成孤儿：既继续吃 CPU，又攥着 stdout 管道
// 不放，于是 cmd.Wait 在超时之后仍然读不到 EOF，永远回不来。
//
// Windows 上没有 Unix 那样的进程组信号语义。理论上最干净的做法是 Job Object +
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE，但把进程放进 Job 必须在它启动之后、派生孙子
// 进程之前完成，而 os/exec 不暴露 CREATE_SUSPENDED 所需的线程句柄，这个竞态关不掉。
// taskkill /T 在杀的那一刻现场遍历父子关系，覆盖面对本场景足够，且不引入句柄生命周期
// 问题。代价是极少数情形会漏：孙子进程的父进程 PID 已被系统回收再分配给别人时，它不再
// 被算进这棵树。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
		defer cancel()
		kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := kill.Run(); err != nil {
			// taskkill 找不到进程（已经退了）也会返回非零，此时 Process.Kill 会给出
			// os.ErrProcessDone，正是 cmd.Wait 期望的「无需报错」信号。
			return cmd.Process.Kill()
		}
		return nil
	}
}
