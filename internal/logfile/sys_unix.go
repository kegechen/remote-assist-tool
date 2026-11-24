//go:build !windows

package logfile

import (
	"errors"
	"os"
	"strconv"
	"syscall"
)

// userTag 是 auto 目录名里区分用户的部分：uid。
func userTag() string { return strconv.Itoa(os.Getuid()) }

// fileOwner 报告 fi 的属主是不是当前用户。
func fileOwner(fi os.FileInfo) (owned, known bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return int(st.Uid) == os.Getuid(), true
}

// processAlive 报告 pid 的进程是否还在运行（发 0 号信号；没有权限发也说明进程在）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
