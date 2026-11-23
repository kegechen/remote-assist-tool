//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package directory

import "os"

// 其他平台不在发布范围内，不加锁：多进程同时更新时最坏是丢掉一次更新，下次拉取会补回。
func tryLockFile(*os.File, bool) (bool, error) { return true, nil }

func unlockFile(*os.File) {}

func renameBusy(error) bool { return false }
