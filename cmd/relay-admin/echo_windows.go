//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// disableEcho 关闭控制台回显，返回恢复函数。f 不是控制台（比如 Git Bash 的 mintty 管道）时 ok=false。
func disableEcho(f *os.File) (restore func(), ok bool) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, false
	}
	newMode := (mode &^ windows.ENABLE_ECHO_INPUT) | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT
	if err := windows.SetConsoleMode(h, newMode); err != nil {
		return nil, false
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }, true
}
