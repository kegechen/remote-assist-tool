//go:build !windows

package config

// isTransientOpenError 在类 Unix 系统上打开文件不会因写入方持有句柄而失败。
func isTransientOpenError(error) bool { return false }
