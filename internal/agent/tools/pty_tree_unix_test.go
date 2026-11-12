//go:build linux

package tools

import (
	"os"
	"strings"
	"syscall"
)

func descendantsOf(root int) []int { return collectTree(root) }

func pidAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	// 僵尸进程不算活着
	if b, err := os.ReadFile("/proc/" + itoa(pid) + "/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && len(s) > i+2 && s[i+2] == 'Z' {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// treeTestCommand 在 shell 里启动一个前台 + 一个后台长进程（各自独立进程组/会话内）。
func treeTestCommand() (shell string, input string) {
	return "/bin/sh", "sleep 300 & sleep 301\n"
}
