//go:build windows

package directory

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile 不等待地尝试取得排他锁（exclusive）或共享锁；锁被别的句柄占着时返回 (false, nil)。
// LockFileEx 的锁属于文件句柄，同一进程里两个句柄也互斥。
func tryLockFile(f *os.File, exclusive bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	var ol windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func unlockFile(f *os.File) {
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

// renameBusy 报告 rename 失败是否可能只是文件正被别的句柄短暂占用：杀毒软件、索引服务扫描时打开文件不带
// FILE_SHARE_DELETE，这期间 MoveFileEx 返回拒绝访问或共享冲突。真的没有权限也是拒绝访问，重试几次后照样报错。
func renameBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
