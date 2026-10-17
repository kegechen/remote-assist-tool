//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package client

import (
	"os"

	"golang.org/x/sys/unix"
)

// consoleInfo 判断 f 是否连着终端，并返回一个读取当前列数的函数（窗口可能被拉宽）。
func consoleInfo(f *os.File) (bool, func() int) {
	fd := int(f.Fd())
	if _, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err != nil {
		return false, nil
	}
	return true, func() int {
		ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		if err != nil {
			return 0
		}
		return int(ws.Col)
	}
}
