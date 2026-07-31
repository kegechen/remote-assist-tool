//go:build windows

package client

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func shareInstanceLockPath() (string, error) {
	currentUser, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("无法获取 Windows 用户，不能建立 share 单实例锁: %w", err)
	}
	homeDir := strings.TrimSpace(currentUser.HomeDir)
	if homeDir == "" {
		return "", errors.New("Windows 用户目录为空，不能建立 share 单实例锁")
	}
	// Windows 的 HomeDir 来自当前进程令牌，不读取可被升级隔离的 HOME/USERPROFILE。
	return filepath.Join(homeDir, shareLockFileName), nil
}

func lockShareFile(path string) (*shareInstanceLock, error) {
	return lockShareFileWithFlags(path, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY)
}

func waitShareFile(path string) (*shareInstanceLock, error) {
	return lockShareFileWithFlags(path, windows.LOCKFILE_EXCLUSIVE_LOCK)
}

func lockShareFileWithFlags(path string, flags uint32) (*shareInstanceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	overlapped := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, overlapped)
	if err != nil {
		file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrShareAlreadyRunning
		}
		return nil, fmt.Errorf("LockFileEx: %w", err)
	}
	return &shareInstanceLock{file: file}, nil
}

func (l *shareInstanceLock) Close() error {
	overlapped := new(windows.Overlapped)
	unlockErr := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, overlapped)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("UnlockFileEx: %w", unlockErr)
	}
	return closeErr
}
