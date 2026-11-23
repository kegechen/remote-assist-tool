//go:build linux || darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// disableEcho 关闭终端回显，返回恢复函数。f 不是终端时 ok=false。
func disableEcho(f *os.File) (restore func(), ok bool) {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, false
	}
	old := *t
	t.Lflag &^= unix.ECHO
	t.Lflag |= unix.ICANON | unix.ISIG
	t.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, t); err != nil {
		return nil, false
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, &old) }, true
}
