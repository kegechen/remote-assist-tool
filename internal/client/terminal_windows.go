//go:build windows

package client

import (
	"os"

	"golang.org/x/sys/windows"
)

// consoleInfo 判断 f 是否连着控制台，并返回一个读取当前可见列数的函数（窗口可能被拖动改宽）。
func consoleInfo(f *os.File) (bool, func() int) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false, nil
	}
	return true, func() int {
		var info windows.ConsoleScreenBufferInfo
		if windows.GetConsoleScreenBufferInfo(h, &info) != nil {
			return 0
		}
		return int(info.Window.Right-info.Window.Left) + 1
	}
}
