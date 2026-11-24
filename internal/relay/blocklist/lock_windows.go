//go:build windows

package blocklist

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// fileLock 是进程级独占锁，防止两个 relay 实例同时写同一个黑名单文件。
// 用 LockFileEx 加排他锁，但不等待：锁被占用立即失败。
type fileLock struct{ file *os.File }

func acquireFileLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var ov windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ov); err != nil {
		f.Close()
		return nil, fmt.Errorf("blocklist lock %s is held by another process: %w", path, err)
	}
	return &fileLock{file: f}, nil
}

func (l *fileLock) Close() error {
	var ov windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &ov)
	return l.file.Close()
}
