//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package directory

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFile 不等待地尝试取得排他锁（exclusive）或共享锁；锁被别人占着时返回 (false, nil)。
// 用 flock 而不是 fcntl：flock 的锁属于打开的文件描述，同一进程里两次 open 也互斥；
// fcntl 锁按进程计，同进程内不互斥，而且关闭任何一个描述符都会释放。
func tryLockFile(f *os.File, exclusive bool) (bool, error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return false, err
}

func unlockFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// renameBusy：rename 不受别的进程开着文件影响，失败就是真失败，不重试。
func renameBusy(error) bool { return false }
