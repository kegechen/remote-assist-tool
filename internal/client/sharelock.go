package client

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const shareLockFileName = ".remote_assist_share.lock"

var ErrShareAlreadyRunning = errors.New("已有 share 正在运行，请先退出该进程或使用 --new-instance")

type shareInstanceLock struct {
	file *os.File
}

// ShareInstanceLockResult 是热升级异步接管默认实例锁的结果。
type ShareInstanceLockResult struct {
	Lock io.Closer
	Err  error
}

// AcquireShareInstanceLock 获取默认 share 的用户级单实例锁。调用方必须在退出时 Close；
// 进程异常结束时操作系统也会自动释放文件锁。
func AcquireShareInstanceLock() (io.Closer, error) {
	path, err := shareInstanceLockPath()
	if err != nil {
		return nil, err
	}
	lock, err := acquireShareInstanceLock(path)
	if err != nil {
		if errors.Is(err, ErrShareAlreadyRunning) {
			return nil, fmt.Errorf("%w（锁文件: %s）", err, path)
		}
		return nil, fmt.Errorf("建立 share 单实例锁失败(%s): %w", path, err)
	}
	return lock, nil
}

// BeginShareInstanceLockHandover 供热升级接管进程使用。它先同步持有交接门锁，再异步
// 等待 old 释放主锁；普通启动必须先经过门锁，因此接管者排队和取得主锁之间没有窗口。
func BeginShareInstanceLockHandover() (<-chan ShareInstanceLockResult, error) {
	path, err := shareInstanceLockPath()
	if err != nil {
		return nil, err
	}
	result, err := beginShareInstanceLockHandover(path)
	if err != nil {
		return nil, fmt.Errorf("开始接管 share 单实例锁失败(%s): %w", path, err)
	}
	return result, nil
}

func acquireShareInstanceLock(path string) (*shareInstanceLock, error) {
	gate, err := lockShareFile(path + ".handover")
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	return lockShareFile(path)
}

func beginShareInstanceLockHandover(path string) (<-chan ShareInstanceLockResult, error) {
	gate, err := lockShareFile(path + ".handover")
	if err != nil {
		return nil, err
	}
	result := make(chan ShareInstanceLockResult, 1)
	go func() {
		lock, lockErr := waitShareFile(path)
		_ = gate.Close()
		result <- ShareInstanceLockResult{Lock: lock, Err: lockErr}
	}()
	return result, nil
}
