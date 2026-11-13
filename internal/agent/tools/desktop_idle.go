package tools

import (
	"runtime/debug"
	"sync"
	"time"
)

// 远程桌面后端的惰性初始化与空闲回收：
//   - 后端（platformDesktopOps，Windows 下会改进程 DPI 感知）只在启用且首次被调用时构造；
//   - 每次调用后用一个一次性 time.AfterFunc 计时，空闲 desktopIdleTimeout 后释放缓存资源
//     （Windows 内存 DC+DIB、Linux X 连接）；空闲期间没有任何常驻 goroutine（定时器触发才临时起一个）。

// desktopIdleTimeout 空闲多久后释放抓屏/注入的缓存资源；测试可改。
var desktopIdleTimeout = 30 * time.Second

var (
	sharedOpsOnce sync.Once
	sharedOps     *DesktopOps

	idleMu    sync.Mutex
	idleTimer *time.Timer
)

// lazyDesktopOps 返回进程内共享的平台后端，首次调用时才构造。
func lazyDesktopOps() *DesktopOps {
	sharedOpsOnce.Do(func() { sharedOps = platformDesktopOps() })
	return sharedOps
}

// touchDesktopIdle 记录一次使用并（重新）安排空闲回收。
func touchDesktopIdle() {
	idleMu.Lock()
	defer idleMu.Unlock()
	if idleTimer == nil {
		idleTimer = time.AfterFunc(desktopIdleTimeout, releaseDesktopIdle)
		return
	}
	idleTimer.Reset(desktopIdleTimeout)
}

func releaseDesktopIdle() {
	platformReleaseIdle()
	// 抓屏/编码期间堆会涨到几十 MB，空闲后主动把空闲页还给系统，让常驻内存回落到使用前水平。
	debug.FreeOSMemory()
}
