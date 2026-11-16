//go:build linux && (amd64 || arm64)

package tools

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
)

// MIT-SHM 抓屏：X server 直接把像素写进与我们共享的内存，省掉 GetImage 经 socket 拷贝 4MB/帧的开销。
// 任何一步失败（无扩展、远程 X、shmget/shmat 失败）都返回错误，由调用方回退核心协议 GetImage。
type x11Shm struct {
	seg  shm.Seg
	addr uintptr
	size int
	ok   bool
}

const (
	sysIPCPrivate = 0
	sysIPCCreat   = 01000
	sysIPCRmid    = 0
)

func (b *x11Backend) shmInit() error {
	if b.shmTried {
		if b.shm.ok {
			return nil
		}
		return fmt.Errorf("shm unavailable")
	}
	b.shmTried = true
	if err := shm.Init(b.conn); err != nil {
		return err
	}
	v, err := shm.QueryVersion(b.conn).Reply()
	if err != nil {
		return err
	}
	_ = v
	b.shm.ok = true
	return nil
}

// shmEnsure 保证共享段至少 size 字节；不够时重建（旧段先 Detach 并解除映射）。
func (b *x11Backend) shmEnsure(size int) error {
	if b.shm.size >= size && b.shm.addr != 0 {
		return nil
	}
	b.shmRelease()
	id, _, e := syscall.Syscall(syscall.SYS_SHMGET, sysIPCPrivate, uintptr(size), sysIPCCreat|0600)
	if e != 0 {
		return fmt.Errorf("shmget: %v", e)
	}
	addr, _, e := syscall.Syscall(syscall.SYS_SHMAT, id, 0, 0)
	if e != 0 {
		syscall.Syscall(syscall.SYS_SHMCTL, id, sysIPCRmid, 0)
		return fmt.Errorf("shmat: %v", e)
	}
	seg, err := shm.NewSegId(b.conn)
	if err != nil {
		syscall.Syscall(syscall.SYS_SHMDT, addr, 0, 0)
		syscall.Syscall(syscall.SYS_SHMCTL, id, sysIPCRmid, 0)
		return err
	}
	// 同步 attach，失败（如 X server 在另一台机器上无法访问该段）立即报错
	if err := shm.AttachChecked(b.conn, seg, uint32(id), false).Check(); err != nil {
		syscall.Syscall(syscall.SYS_SHMDT, addr, 0, 0)
		syscall.Syscall(syscall.SYS_SHMCTL, id, sysIPCRmid, 0)
		return err
	}
	// 标记删除：所有人解除映射后系统自动回收，进程崩溃也不会泄漏
	syscall.Syscall(syscall.SYS_SHMCTL, id, sysIPCRmid, 0)
	b.shm.seg, b.shm.addr, b.shm.size = seg, addr, size
	return nil
}

func (b *x11Backend) shmRelease() {
	if b.shm.addr == 0 {
		return
	}
	if b.conn != nil {
		_ = shm.Detach(b.conn, b.shm.seg).Check()
	}
	syscall.Syscall(syscall.SYS_SHMDT, b.shm.addr, 0, 0)
	b.shm.addr, b.shm.size = 0, 0
}

// shmGetImage 抓取根窗口矩形，返回指向共享内存的只读视图（下一次抓取前有效，调用方须在持锁期间拷走）。
func (b *x11Backend) shmGetImage(x, y int16, w, h uint16) ([]byte, error) {
	if err := b.shmInit(); err != nil {
		return nil, err
	}
	need := int(w) * int(h) * 4
	if err := b.shmEnsure(need); err != nil {
		b.shm.ok = false
		return nil, err
	}
	rep, err := shm.GetImage(b.conn, xproto.Drawable(b.root), x, y, w, h, 0xffffffff, xproto.ImageFormatZPixmap, b.shm.seg, 0).Reply()
	if err != nil {
		b.shm.ok = false
		return nil, err
	}
	if int(rep.Size) < need {
		return nil, fmt.Errorf("shm GetImage 返回数据不足: %d < %d", rep.Size, need)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(b.shm.addr)), need), nil
}
