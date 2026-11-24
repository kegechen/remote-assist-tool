//go:build !windows

package blocklist

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// fileLock 是进程级独占锁，防止两个 relay 实例同时写同一个黑名单文件。
// 用 flock 加排他锁，但不等待：锁被占用立即失败。
type fileLock struct{ file *os.File }

func acquireFileLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateFile(path); err != nil {
		f.Close()
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("blocklist lock %s is held by another process: %w", path, err)
	}
	return &fileLock{file: f}, nil
}

func (l *fileLock) Close() error {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}
