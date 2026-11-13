//go:build windows

package tools

import (
	"errors"
	"fmt"
	"image"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DXGI Desktop Duplication 抓屏后端（纯 Go，无 cgo，手写最小 COM vtable 调用）。
//
// 流程：CreateDXGIFactory1 → 枚举 adapter/output（按显示器矩形匹配）→ 在该 output 所属 adapter 上
// D3D11CreateDevice → IDXGIOutput1::DuplicateOutput → 每帧 AcquireNextFrame → CopyResource 到 CPU 可读
// staging texture → Map 读 BGRA → 转 RGBA（复用 parallelRows）→ Unmap/ReleaseFrame。
//
// 与 GDI 的关系：DXGI 为首选；任何失败（不支持、E_ACCESSDENIED、锁屏/RDP/会话 0、设备丢失）都返回 ok=false，
// 由调用方回退 GDI，且失败后 dxgiRetryInterval 内不再尝试 DXGI。屏幕无变化（AcquireNextFrame 超时或仅光标移动）
// 时直接返回上一帧缓存。全部 COM 对象在空闲回收（dxgiReleaseAll）时 Release。
//
// 不使用 syscall.NewCallback（本文件没有任何回调）。所有传给 COM 的指针目标都放在包级 dxScratch 里，
// 因为 uintptr 参数不会被 GC/栈拷贝跟踪，必须保证目标地址稳定；dxgiMu 串行化对其的使用。

var (
	d3d11dll = windows.NewLazySystemDLL("d3d11.dll")
	dxgidll  = windows.NewLazySystemDLL("dxgi.dll")

	pD3D11CreateDevice  = d3d11dll.NewProc("D3D11CreateDevice")
	pCreateDXGIFactory1 = dxgidll.NewProc("CreateDXGIFactory1")
)

const (
	hrWaitTimeout  = 0x887A0027 // DXGI_ERROR_WAIT_TIMEOUT
	hrAccessLost   = 0x887A0026 // DXGI_ERROR_ACCESS_LOST
	hrNotFound     = 0x887A0002 // DXGI_ERROR_NOT_FOUND（枚举结束）
	dxgiFormatBGRA = 87         // DXGI_FORMAT_B8G8R8A8_UNORM

	dxAcquireTimeoutMs = 20 // 无变化时最多等这么久再返回缓存帧
)

// 可调参数（测试可改）。
var (
	dxgiRetryInterval = 30 * time.Second
	// dxgiMode: "" 默认、"gdi" 强制 GDI、"dxgi" 强制尝试 DXGI；环境变量 REMOTE_DESKTOP_CAPTURE。
	dxgiMode = os.Getenv("REMOTE_DESKTOP_CAPTURE")
	// 测试钩子：REMOTE_DESKTOP_DXGI_FORCE_FAIL=1 时初始化必然失败，用于验证回退。
	dxgiForceFail = os.Getenv("REMOTE_DESKTOP_DXGI_FORCE_FAIL") == "1"
	// 默认是否启用 DXGI（环境变量未指定时）。
	dxgiDefaultOn = true
)

var (
	iidDXGIFactory1 = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidDXGIOutput1  = windows.GUID{Data1: 0x00cddea8, Data2: 0x939b, Data3: 0x4b83, Data4: [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidTexture2D    = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

// comObj 是指向 COM 接口指针（其首字为 vtable 指针）的薄封装。
type comObj struct{ p unsafe.Pointer }

func (o comObj) nil() bool { return o.p == nil }

// call 调用 vtable[idx]，返回 HRESULT（或原始返回值）。
func (o comObj) call(idx int, args ...uintptr) uintptr {
	vt := *(*unsafe.Pointer)(o.p)
	fn := *(*uintptr)(unsafe.Add(vt, idx*int(unsafe.Sizeof(uintptr(0)))))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(o.p))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

// dxLive 统计尚未 Release 的 COM 对象数，用于泄漏测试。
var dxLive atomic.Int64

func (o comObj) release() {
	if o.p != nil {
		o.call(2)
		dxLive.Add(-1)
	}
}

func dxTrack(p unsafe.Pointer) comObj {
	if p != nil {
		dxLive.Add(1)
	}
	return comObj{p}
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

type texDesc struct {
	width, height, mipLevels, arraySize, format uint32
	sampleCount, sampleQuality                  uint32
	usage, bindFlags, cpuAccess, miscFlags      uint32
}

type outputDesc struct {
	deviceName [32]uint16
	coords     windows.Rect
	attached   int32
	rotation   uint32
	monitor    uintptr
}

type frameInfo struct {
	lastPresent, lastMouse int64
	accumulated            uint32
	rectsCoalesced         int32
	protectedMasked        int32
	ptrX, ptrY             int32
	ptrVisible             int32
	metaSize, shapeSize    uint32
}

type mappedRes struct {
	pData      unsafe.Pointer
	rowPitch   uint32
	depthPitch uint32
}

// dxScratch：COM 调用的出参/入参存放处（包级变量地址稳定），由 dxgiMu 保护。
var dxScratch struct {
	out1, out2 unsafe.Pointer
	featLevel  uint32
	od         outputDesc
	td         texDesc
	fi         frameInfo
	mapped     mappedRes
}

type dxgiDisplay struct {
	device, ctx, dup, staging comObj
	rect                      image.Rectangle
	last                      *image.RGBA
	retryAt                   time.Time
	lastErr                   error
}

var (
	dxgiMu       sync.Mutex
	dxgiDisplays = map[int]*dxgiDisplay{}
	dxgiStats    struct {
		dxgiFrames, cachedFrames, gdiFrames atomic.Int64
		// 取到新帧时各段累计纳秒：等待新帧(Acquire)、拷贝+Map、BGRA→RGBA 转换
		acquireNs, copyNs, convNs atomic.Int64
	}
)

func dxgiEnabled() bool {
	switch dxgiMode {
	case "gdi":
		return false
	case "dxgi":
		return true
	}
	return dxgiDefaultOn
}

// winCaptureAuto 是 DesktopOps.Capture 的实现：优先 DXGI，失败回退 GDI。
func winCaptureAuto(display int) (*image.RGBA, image.Rectangle, error) {
	if !dxgiEnabled() {
		return winCapture(display)
	}
	rect, err := monitorRect(display)
	if err != nil {
		return nil, rect, err
	}
	if img, ok := dxgiCapture(display, rect); ok {
		return img, rect, nil
	}
	dxgiStats.gdiFrames.Add(1)
	img, r, err := winCapture(display)
	if err == nil {
		dxgiSeed(display, rect, img)
	}
	return img, r, err
}

// dxgiSeed 在 duplication 已建立但还没有桌面帧（屏幕静止）时，用 GDI 帧作为缓存，
// 这样后续 AcquireNextFrame 超时能返回正确画面。
func dxgiSeed(display int, rect image.Rectangle, img *image.RGBA) {
	dxgiMu.Lock()
	defer dxgiMu.Unlock()
	if d := dxgiDisplays[display]; d != nil && !d.dup.nil() && d.last == nil && d.rect == rect {
		d.last = img
	}
}

func dxgiCapture(display int, rect image.Rectangle) (*image.RGBA, bool) {
	dxgiMu.Lock()
	defer dxgiMu.Unlock()
	d := dxgiDisplays[display]
	if d == nil {
		d = &dxgiDisplay{}
		dxgiDisplays[display] = d
	}
	if !d.dup.nil() && d.rect != rect { // 分辨率/布局变化
		d.close()
		d.last = nil
	}
	if d.dup.nil() {
		if time.Now().Before(d.retryAt) {
			return nil, false
		}
		if err := d.init(rect); err != nil {
			d.fail(err)
			return nil, false
		}
	}
	img, err := d.grab()
	if errors.Is(err, errDXTimeoutNoCache) {
		return nil, false // 首帧即超时：本次用 GDI，不算失败
	}
	if errors.Is(err, errDXAccessLost) { // 分辨率切换/UAC/锁屏：立即重建一次
		d.close()
		if e := d.init(rect); e == nil {
			img, err = d.grab()
		} else {
			err = e
		}
	}
	if err != nil {
		if errors.Is(err, errDXTimeoutNoCache) {
			return nil, false
		}
		d.fail(err)
		return nil, false
	}
	return img, true
}

// fail 释放资源并安排 dxgiRetryInterval 之后才重试。
func (d *dxgiDisplay) fail(err error) {
	d.close()
	d.last = nil
	d.lastErr = err
	d.retryAt = time.Now().Add(dxgiRetryInterval)
	if desktopTraceOn {
		fmt.Fprintf(os.Stderr, "[desktop-dxgi] 回退 GDI（%v 后重试）: %v\n", dxgiRetryInterval, err)
	}
}

func (d *dxgiDisplay) close() {
	d.staging.release()
	d.dup.release()
	d.ctx.release()
	d.device.release()
	d.staging, d.dup, d.ctx, d.device = comObj{}, comObj{}, comObj{}, comObj{}
}

// dxgiReleaseAll 释放所有显示器的 COM 对象与缓存帧（空闲回收调用）。
func dxgiReleaseAll() {
	dxgiMu.Lock()
	defer dxgiMu.Unlock()
	for k, d := range dxgiDisplays {
		d.close()
		delete(dxgiDisplays, k)
	}
}

var (
	errDXAccessLost     = errors.New("DXGI_ERROR_ACCESS_LOST")
	errDXTimeoutNoCache = errors.New("首帧等待超时")
)

func hrErr(name string, hr uintptr) error {
	return fmt.Errorf("%s: HRESULT 0x%08X", name, uint32(hr))
}

// init 为 rect 对应的 output 创建 device、duplication 和 staging texture。
func (d *dxgiDisplay) init(rect image.Rectangle) (err error) {
	if dxgiForceFail {
		return errors.New("测试钩子强制 DXGI 失败")
	}
	ensureDPIAware()
	if err := pCreateDXGIFactory1.Find(); err != nil {
		return err
	}
	if err := pD3D11CreateDevice.Find(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			d.close()
		}
	}()
	sc := &dxScratch
	sc.out1 = nil
	if hr, _, _ := pCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidDXGIFactory1)), uintptr(unsafe.Pointer(&sc.out1))); failed(hr) {
		return hrErr("CreateDXGIFactory1", hr)
	}
	factory := dxTrack(sc.out1)
	defer factory.release()

	var adapter, output comObj
	found := false
	for ai := 0; !found; ai++ {
		sc.out1 = nil
		if hr := factory.call(12, uintptr(ai), uintptr(unsafe.Pointer(&sc.out1))); failed(hr) { // EnumAdapters1
			break
		}
		ad := dxTrack(sc.out1)
		for oi := 0; ; oi++ {
			sc.out1 = nil
			if hr := ad.call(7, uintptr(oi), uintptr(unsafe.Pointer(&sc.out1))); failed(hr) { // EnumOutputs
				break
			}
			out := dxTrack(sc.out1)
			if hr := out.call(7, uintptr(unsafe.Pointer(&sc.od))); !failed(hr) && dxOutputMatches(&sc.od, rect) { // GetDesc
				adapter, output, found = ad, out, true
				break
			}
			out.release()
		}
		if !found {
			ad.release()
		}
	}
	if !found {
		return fmt.Errorf("未找到与显示器 %v 匹配的 DXGI output", rect)
	}
	defer adapter.release()
	defer output.release()
	if sc.od.rotation > 1 {
		return fmt.Errorf("显示器旋转 (%d)，DXGI 路径暂不处理", sc.od.rotation)
	}

	// 设备必须建在 output 所属 adapter 上（Optimus/双显卡下 output 通常挂在核显）。
	const createBGRA = 0x20
	sc.out1, sc.out2 = nil, nil
	hr, _, _ := pD3D11CreateDevice.Call(uintptr(adapter.p), 0 /*UNKNOWN*/, 0, createBGRA, 0, 0, 7,
		uintptr(unsafe.Pointer(&sc.out1)), uintptr(unsafe.Pointer(&sc.featLevel)), uintptr(unsafe.Pointer(&sc.out2)))
	if failed(hr) {
		return hrErr("D3D11CreateDevice", hr)
	}
	d.device, d.ctx = dxTrack(sc.out1), dxTrack(sc.out2)

	sc.out1 = nil
	if hr := output.call(0, uintptr(unsafe.Pointer(&iidDXGIOutput1)), uintptr(unsafe.Pointer(&sc.out1))); failed(hr) {
		return hrErr("QueryInterface(IDXGIOutput1)", hr)
	}
	out1 := dxTrack(sc.out1)
	defer out1.release()
	sc.out1 = nil
	if hr := out1.call(22, uintptr(d.device.p), uintptr(unsafe.Pointer(&sc.out1))); failed(hr) { // DuplicateOutput
		return hrErr("DuplicateOutput", hr)
	}
	d.dup = dxTrack(sc.out1)

	// staging texture：CPU 可读，与桌面同尺寸、BGRA。
	sc.td = texDesc{width: uint32(rect.Dx()), height: uint32(rect.Dy()), mipLevels: 1, arraySize: 1,
		format: dxgiFormatBGRA, sampleCount: 1, usage: 3 /*STAGING*/, cpuAccess: 0x20000 /*READ*/}
	sc.out1 = nil
	if hr := d.device.call(5, uintptr(unsafe.Pointer(&sc.td)), 0, uintptr(unsafe.Pointer(&sc.out1))); failed(hr) { // CreateTexture2D
		return hrErr("CreateTexture2D(staging)", hr)
	}
	d.staging = dxTrack(sc.out1)
	d.rect = rect
	d.last = nil
	d.lastErr = nil
	return nil
}

func dxOutputMatches(od *outputDesc, r image.Rectangle) bool {
	c := od.coords
	return od.attached != 0 && int(c.Left) == r.Min.X && int(c.Top) == r.Min.Y &&
		int(c.Right) == r.Max.X && int(c.Bottom) == r.Max.Y
}

// grab 取一帧。屏幕无变化返回缓存帧（可能与上次是同一个 *image.RGBA，调用方只读）。
func (d *dxgiDisplay) grab() (*image.RGBA, error) {
	// 首帧前 DXGI 可能先交付仅光标更新（lastPresent==0）的空纹理，需要继续等真正的桌面帧。
	deadline := time.Now().Add(dxFirstFrameBudget)
	for {
		img, err, again := d.grabOnce(deadline)
		if !again {
			return img, err
		}
	}
}

const dxFirstFrameBudget = 200 * time.Millisecond

// grabOnce 取一次；again=true 表示拿到的是无画面的光标更新且还没有缓存，应重试。
func (d *dxgiDisplay) grabOnce(deadline time.Time) (img *image.RGBA, err error, again bool) {
	sc := &dxScratch
	sc.out1 = nil
	t0 := time.Now()
	hr := d.dup.call(8, dxAcquireTimeoutMs, uintptr(unsafe.Pointer(&sc.fi)), uintptr(unsafe.Pointer(&sc.out1))) // AcquireNextFrame
	switch {
	case uint32(hr) == hrWaitTimeout:
		if d.last != nil {
			dxgiStats.cachedFrames.Add(1)
			return d.last, nil, false
		}
		return nil, errDXTimeoutNoCache, false
	case uint32(hr) == hrAccessLost:
		return nil, errDXAccessLost, false
	case failed(hr):
		return nil, hrErr("AcquireNextFrame", hr), false
	}
	if sc.fi.lastPresent == 0 {
		dxTrack(sc.out1).release()
		d.dup.call(14)
		if d.last != nil {
			dxgiStats.cachedFrames.Add(1)
			return d.last, nil, false
		}
		if time.Now().After(deadline) {
			return nil, errDXTimeoutNoCache, false
		}
		return nil, nil, true
	}
	res := dxTrack(sc.out1)
	frameHeld := true
	defer func() {
		if frameHeld {
			d.dup.call(14) // ReleaseFrame
		}
	}()
	defer res.release()

	sc.out1 = nil
	if hr := res.call(0, uintptr(unsafe.Pointer(&iidTexture2D)), uintptr(unsafe.Pointer(&sc.out1))); failed(hr) {
		return nil, hrErr("QueryInterface(ID3D11Texture2D)", hr), false
	}
	tex := dxTrack(sc.out1)
	defer tex.release()
	tex.call(10, uintptr(unsafe.Pointer(&sc.td))) // GetDesc
	w, h := d.rect.Dx(), d.rect.Dy()
	if int(sc.td.width) != w || int(sc.td.height) != h || sc.td.format != dxgiFormatBGRA {
		return nil, fmt.Errorf("桌面纹理 %dx%d 格式 %d 与期望 %dx%d BGRA 不符（HDR/分辨率切换？）",
			sc.td.width, sc.td.height, sc.td.format, w, h), false
	}
	t1 := time.Now()
	d.ctx.call(47, uintptr(d.staging.p), uintptr(tex.p)) // CopyResource
	d.dup.call(14)                                       // ReleaseFrame（拷贝已入队）
	frameHeld = false

	if hr := d.ctx.call(14, uintptr(d.staging.p), 0, 1 /*MAP_READ*/, 0, uintptr(unsafe.Pointer(&sc.mapped))); failed(hr) {
		return nil, hrErr("Map(staging)", hr), false
	}
	defer d.ctx.call(15, uintptr(d.staging.p), 0) // Unmap
	pitch := int(sc.mapped.rowPitch)
	if pitch < w*4 {
		return nil, fmt.Errorf("RowPitch %d 小于 %d", pitch, w*4), false
	}
	src := unsafe.Slice((*byte)(sc.mapped.pData), pitch*(h-1)+w*4)
	t2 := time.Now()
	img = image.NewRGBA(image.Rect(0, 0, w, h))
	parallelRows(h, 64, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			// 按 32 位字整体换 R/B：BGRA(小端字 0xAARRGGBB) → RGBA(字 0xAABBGGRR)，alpha 置 255。
			s := unsafe.Slice((*uint32)(unsafe.Pointer(&src[y*pitch])), w)
			dd := unsafe.Slice((*uint32)(unsafe.Pointer(&img.Pix[y*w*4])), w)
			for i, v := range s {
				dd[i] = v&0x0000FF00 | (v&0xFF)<<16 | (v>>16)&0xFF | 0xFF000000
			}
		}
	})
	runtime.KeepAlive(src)
	t3 := time.Now()
	dxgiStats.acquireNs.Add(int64(t1.Sub(t0)))
	dxgiStats.copyNs.Add(int64(t2.Sub(t1)))
	dxgiStats.convNs.Add(int64(t3.Sub(t2)))
	d.last = img
	dxgiStats.dxgiFrames.Add(1)
	return img, nil, false
}
