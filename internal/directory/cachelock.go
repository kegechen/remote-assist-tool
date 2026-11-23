package directory

import (
	"fmt"
	"os"
	"time"
)

// lockTimeout 是等别的进程释放缓存锁的上限。持锁期间只做读文件、验签、写文件，正常远不到一秒。
const lockTimeout = 10 * time.Second

// lockCache 取得 path 的跨进程锁：exclusive 为 true 时是写锁（排他），否则是读锁（与其他读锁共存）。
// 锁的是旁边的 path+".lock"，不是缓存文件本身：缓存文件会被 rename 替换，锁在它上面会随旧文件一起失效。
// 锁文件留在原地，不删除——删除会让「刚打开旧锁文件的进程」和「新建锁文件的进程」各自拿到一把锁。
// 同一个进程里也不能嵌套加锁（锁属于打开的文件，第二次加锁会等第一次释放）。
func lockCache(path string, exclusive bool) (unlock func(), err error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		ok, err := tryLockFile(f, exclusive)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("directory: lock %s: %w", path, err)
		}
		if ok {
			return func() {
				unlockFile(f)
				f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("directory: %s is locked by another process for more than %s", path, lockTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
