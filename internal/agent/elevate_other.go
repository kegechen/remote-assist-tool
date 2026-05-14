//go:build !windows

package agent

import "fmt"

// RelaunchElevated 非 Windows 上是 no-op + 错误
func RelaunchElevated() error {
	return fmt.Errorf("--elevate is only supported on Windows")
}

// IsElevated 非 Windows 保守返回 false
func IsElevated() bool {
	return false
}
