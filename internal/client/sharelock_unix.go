//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package client

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func shareInstanceLockPath() (string, error) {
	effectiveUID := os.Geteuid()
	currentUser, err := user.LookupId(strconv.Itoa(effectiveUID))
	if err == nil {
		if homeDir := strings.TrimSpace(currentUser.HomeDir); homeDir != "" {
			return filepath.Join(homeDir, shareLockFileName), nil
		}
	}
	// 静态二进制所在的精简系统可能没有当前 UID 的 passwd 条目。固定按 UID 落到
	// /tmp，避免退回读取热升级改写过的 HOME，也避免不同用户共用同一把锁。
	return filepath.Join("/tmp", shareLockFileName+"."+strconv.Itoa(effectiveUID)), nil
}

func lockShareFile(path string) (*shareInstanceLock, error) {
	return lockShareFileWithOperation(path, unix.LOCK_EX|unix.LOCK_NB)
}

func waitShareFile(path string) (*shareInstanceLock, error) {
	return lockShareFileWithOperation(path, unix.LOCK_EX)
}

func lockShareFileWithOperation(path string, operation int) (*shareInstanceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = unix.Flock(int(file.Fd()), operation)
	if err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrShareAlreadyRunning
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return &shareInstanceLock{file: file}, nil
}

func (l *shareInstanceLock) Close() error {
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("flock unlock: %w", unlockErr)
	}
	return closeErr
}
