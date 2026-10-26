//go:build windows

package config

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isTransientOpenError 报告打开失败是否只是写入方仍持有独占句柄（写入尚未结束）。
func isTransientOpenError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
