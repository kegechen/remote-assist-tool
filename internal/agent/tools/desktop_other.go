//go:build !windows && !linux && !darwin

package tools

// platformDesktopOps 既非 Windows 也非 Linux 的平台暂无实现。
func platformDesktopOps() *DesktopOps { return nil }
