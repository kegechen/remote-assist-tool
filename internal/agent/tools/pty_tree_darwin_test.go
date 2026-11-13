//go:build darwin

package tools

import (
	"strconv"
	"syscall"
)

func descendantsOf(root int) []int { return collectTree(root) }

// darwin 没有 /proc，用信号 0 探测；僵尸进程在被回收前也会返回成功，这里接受这一近似。
func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func itoa(n int) string { return strconv.Itoa(n) }

// treeTestCommand 在 shell 里启动一个前台 + 一个后台长进程。
func treeTestCommand() (shell string, input string) {
	return "/bin/sh", "sleep 300 & sleep 301\n"
}
