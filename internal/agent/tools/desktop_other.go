//go:build !windows

package tools

// platformDesktopOps 非 Windows 平台暂无实现（Linux X11 后续补充）。
func platformDesktopOps() *DesktopOps { return nil }
