//go:build darwin

package tools

// macOS 被控端：纯 Go（purego，无 cgo）动态调用 CoreGraphics / ApplicationServices / CoreFoundation。
// 抓屏：CGDisplayCreateImage；注入：CGEventCreate* + CGEventPost(kCGHIDEventTap)。
// 坐标约定：Bounds/Capture 返回的矩形与 Mouse 的坐标均使用 CoreGraphics 全局坐标（点，左上角为原点，
// 主显示器原点 0,0）；Capture 返回的图像是物理像素（Retina 下宽高为矩形的 2 倍）。
// 上层以 0..1 归一化坐标映射，因此两者缩放不一致不影响点击位置。
// 本实现不使用任何 purego 回调。

import (
	"fmt"
	"image"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	macLibCoreGraphics = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
	macLibAppServices  = "/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices"
	macLibCoreFound    = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"

	macHIDEventTap          = 0 // kCGHIDEventTap
	macScrollUnitLine       = 1 // kCGScrollEventUnitLine
	macFieldMouseClickState = 1 // kCGMouseEventClickState
	macMaxDisplays          = 32
)

type macPoint struct{ X, Y float64 }
type macCGRect struct{ X, Y, W, H float64 }

// macAPI 解析好的系统函数。
type macAPI struct {
	CGGetActiveDisplayList func(max uint32, ids unsafe.Pointer, count unsafe.Pointer) int32
	CGMainDisplayID        func() uint32
	CGDisplayBounds        func(id uint32) macCGRect
	CGDisplayPixelsWide    func(id uint32) uintptr
	CGDisplayPixelsHigh    func(id uint32) uintptr
	CGDisplayCreateImage   func(id uint32) uintptr

	CGImageGetWidth                 func(img uintptr) uintptr
	CGImageGetHeight                func(img uintptr) uintptr
	CGImageGetBytesPerRow           func(img uintptr) uintptr
	CGImageGetBitsPerPixel          func(img uintptr) uintptr
	CGImageGetBitmapInfo            func(img uintptr) uint32
	CGImageGetDataProvider          func(img uintptr) uintptr
	CGDataProviderCopyData          func(p uintptr) uintptr
	CFDataGetBytePtr                func(d uintptr) unsafe.Pointer
	CFDataGetLength                 func(d uintptr) int
	CFRelease                       func(ref uintptr)
	CGEventCreateMouseEvent         func(src uintptr, typ uint32, pos macPoint, btn uint32) uintptr
	CGEventCreateKeyboardEvent      func(src uintptr, key uint16, down bool) uintptr
	CGEventCreateScrollWheelEvent2  func(src uintptr, units uint32, n uint32, w1, w2, w3 int32) uintptr
	CGEventKeyboardSetUnicodeString func(ev uintptr, n uintptr, s unsafe.Pointer)
	CGEventSetFlags                 func(ev uintptr, flags uint64)
	CGEventSetIntegerValueField     func(ev uintptr, field uint32, v int64)
	CGEventPost                     func(tap uint32, ev uintptr)

	AXIsProcessTrusted func() bool

	// 10.15+ 才有；缺失时为 nil，跳过预检。
	CGPreflightScreenCaptureAccess func() bool
	CGRequestScreenCaptureAccess   func() bool
}

var (
	macOnce   sync.Once
	macLoaded *macAPI
	macErr    error
)

// macLoad 全局只加载一次；失败返回错误而不是 panic。
func macLoad() (*macAPI, error) {
	macOnce.Do(func() {
		macLoaded, macErr = macLoadLibs()
	})
	return macLoaded, macErr
}

func macLoadLibs() (api *macAPI, err error) {
	defer func() {
		if r := recover(); r != nil {
			api, err = nil, macErrLoad(fmt.Errorf("panic: %v", r))
		}
	}()
	open := func(path string) (uintptr, error) {
		h, e := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if e != nil {
			return 0, fmt.Errorf("dlopen %s: %w", path, e)
		}
		return h, nil
	}
	cg, e := open(macLibCoreGraphics)
	if e != nil {
		return nil, macErrLoad(e)
	}
	as, e := open(macLibAppServices)
	if e != nil {
		return nil, macErrLoad(e)
	}
	cf, e := open(macLibCoreFound)
	if e != nil {
		return nil, macErrLoad(e)
	}
	a := &macAPI{}
	// reg 解析必需符号，缺失返回错误；optional 缺失则忽略。
	var firstErr error
	reg := func(fptr any, lib uintptr, name string, optional bool) {
		sym, e := purego.Dlsym(lib, name)
		if e != nil {
			if !optional && firstErr == nil {
				firstErr = fmt.Errorf("符号 %s 缺失: %w", name, e)
			}
			return
		}
		purego.RegisterFunc(fptr, sym)
	}
	reg(&a.CGGetActiveDisplayList, cg, "CGGetActiveDisplayList", false)
	reg(&a.CGMainDisplayID, cg, "CGMainDisplayID", false)
	reg(&a.CGDisplayBounds, cg, "CGDisplayBounds", false)
	reg(&a.CGDisplayPixelsWide, cg, "CGDisplayPixelsWide", false)
	reg(&a.CGDisplayPixelsHigh, cg, "CGDisplayPixelsHigh", false)
	reg(&a.CGDisplayCreateImage, cg, "CGDisplayCreateImage", false)
	reg(&a.CGImageGetWidth, cg, "CGImageGetWidth", false)
	reg(&a.CGImageGetHeight, cg, "CGImageGetHeight", false)
	reg(&a.CGImageGetBytesPerRow, cg, "CGImageGetBytesPerRow", false)
	reg(&a.CGImageGetBitsPerPixel, cg, "CGImageGetBitsPerPixel", false)
	reg(&a.CGImageGetBitmapInfo, cg, "CGImageGetBitmapInfo", false)
	reg(&a.CGImageGetDataProvider, cg, "CGImageGetDataProvider", false)
	reg(&a.CGDataProviderCopyData, cg, "CGDataProviderCopyData", false)
	reg(&a.CFDataGetBytePtr, cf, "CFDataGetBytePtr", false)
	reg(&a.CFDataGetLength, cf, "CFDataGetLength", false)
	reg(&a.CFRelease, cf, "CFRelease", false)
	reg(&a.CGEventCreateMouseEvent, cg, "CGEventCreateMouseEvent", false)
	reg(&a.CGEventCreateKeyboardEvent, cg, "CGEventCreateKeyboardEvent", false)
	reg(&a.CGEventCreateScrollWheelEvent2, cg, "CGEventCreateScrollWheelEvent2", false)
	reg(&a.CGEventKeyboardSetUnicodeString, cg, "CGEventKeyboardSetUnicodeString", false)
	reg(&a.CGEventSetFlags, cg, "CGEventSetFlags", false)
	reg(&a.CGEventSetIntegerValueField, cg, "CGEventSetIntegerValueField", false)
	reg(&a.CGEventPost, cg, "CGEventPost", false)
	reg(&a.AXIsProcessTrusted, as, "AXIsProcessTrusted", false)
	reg(&a.CGPreflightScreenCaptureAccess, cg, "CGPreflightScreenCaptureAccess", true)
	reg(&a.CGRequestScreenCaptureAccess, cg, "CGRequestScreenCaptureAccess", true)
	if firstErr != nil {
		return nil, macErrLoad(firstErr)
	}
	return a, nil
}

// ---------- 权限 ----------

var (
	macPermMu       sync.Mutex
	macScreenOK     bool
	macScreenAsked  bool
	macAccessibleOK bool
)

// macEnsureScreenAccess 检查“屏幕录制”权限；首次未授权时触发系统授权弹窗，并返回中文指引错误。
func macEnsureScreenAccess(a *macAPI) error {
	macPermMu.Lock()
	defer macPermMu.Unlock()
	if macScreenOK || a.CGPreflightScreenCaptureAccess == nil {
		return nil
	}
	if a.CGPreflightScreenCaptureAccess() {
		macScreenOK = true
		return nil
	}
	if !macScreenAsked && a.CGRequestScreenCaptureAccess != nil {
		macScreenAsked = true
		a.CGRequestScreenCaptureAccess()
	}
	return macErrScreenRecording()
}

// macEnsureAccessibility 检查“辅助功能”权限（注入事件必需）。
func macEnsureAccessibility(a *macAPI) error {
	macPermMu.Lock()
	defer macPermMu.Unlock()
	if macAccessibleOK {
		return nil
	}
	if !a.AXIsProcessTrusted() {
		return macErrAccessibility()
	}
	macAccessibleOK = true
	return nil
}

// ---------- 显示器 / 抓屏 ----------

func macDisplayIDs(a *macAPI) ([]uint32, error) {
	var ids [macMaxDisplays]uint32
	var n uint32
	if r := a.CGGetActiveDisplayList(macMaxDisplays, unsafe.Pointer(&ids[0]), unsafe.Pointer(&n)); r != 0 {
		return nil, fmt.Errorf("CGGetActiveDisplayList 失败: %d", r)
	}
	if n > macMaxDisplays {
		n = macMaxDisplays
	}
	return macDisplayOrder(ids[:n], a.CGMainDisplayID()), nil
}

func macBounds(a *macAPI, id uint32) image.Rectangle {
	b := a.CGDisplayBounds(id)
	if r, ok := macRectFromCG(b.X, b.Y, b.W, b.H); ok {
		return r
	}
	// 兜底：结构体返回异常时用像素宽高，原点按 0,0。
	return image.Rect(0, 0, int(a.CGDisplayPixelsWide(id)), int(a.CGDisplayPixelsHigh(id)))
}

func macResolve(display int) (*macAPI, uint32, error) {
	a, err := macLoad()
	if err != nil {
		return nil, 0, err
	}
	ids, err := macDisplayIDs(a)
	if err != nil {
		return nil, 0, err
	}
	id, err := macPickDisplay(ids, display)
	return a, id, err
}

func macDisplays() (int, error) {
	a, err := macLoad()
	if err != nil {
		return 0, err
	}
	ids, err := macDisplayIDs(a)
	return len(ids), err
}

func macBoundsOf(display int) (image.Rectangle, error) {
	a, id, err := macResolve(display)
	if err != nil {
		return image.Rectangle{}, err
	}
	return macBounds(a, id), nil
}

func macCapture(display int) (*image.RGBA, image.Rectangle, error) {
	a, id, err := macResolve(display)
	if err != nil {
		return nil, image.Rectangle{}, err
	}
	if err := macEnsureScreenAccess(a); err != nil {
		return nil, image.Rectangle{}, err
	}
	rect := macBounds(a, id)
	img := a.CGDisplayCreateImage(id)
	if img == 0 {
		return nil, image.Rectangle{}, fmt.Errorf("CGDisplayCreateImage 返回空（显示器 %d）。若刚授权“屏幕录制”，请重启运行 remote 的终端后重试；%w", display, macErrScreenRecording())
	}
	defer a.CFRelease(img)
	w, h := int(a.CGImageGetWidth(img)), int(a.CGImageGetHeight(img))
	stride, bpp := int(a.CGImageGetBytesPerRow(img)), int(a.CGImageGetBitsPerPixel(img))
	info := a.CGImageGetBitmapInfo(img)
	provider := a.CGImageGetDataProvider(img) // 非拥有，不释放
	if provider == 0 {
		return nil, image.Rectangle{}, fmt.Errorf("CGImageGetDataProvider 返回空")
	}
	data := a.CGDataProviderCopyData(provider)
	if data == 0 {
		return nil, image.Rectangle{}, fmt.Errorf("CGDataProviderCopyData 返回空")
	}
	defer a.CFRelease(data)
	ptr, n := a.CFDataGetBytePtr(data), a.CFDataGetLength(data)
	if ptr == nil || n <= 0 {
		return nil, image.Rectangle{}, fmt.Errorf("屏幕像素数据为空")
	}
	// 转换会拷贝到新的 RGBA，之后即可释放 CFData。
	out, err := macConvertPixels(unsafe.Slice((*byte)(ptr), n), w, h, stride, bpp, info)
	if err != nil {
		return nil, image.Rectangle{}, fmt.Errorf("解析屏幕像素失败: %w", err)
	}
	return out, rect, nil
}

// ---------- 输入 ----------

// macInput 输入状态：按住的鼠标键、已按下的修饰键、最近一次点击记录。
type macInput struct {
	mu      sync.Mutex
	pressed string
	mods    uint64
	last    macClickRecord
	count   int // 当前按下期间的 clickState
}

var macIn macInput

// post 发送并释放事件；ev 为 0 视为创建失败。
func (a *macAPI) post(ev uintptr, flags uint64) error {
	if ev == 0 {
		return fmt.Errorf("创建 CGEvent 失败")
	}
	a.CGEventSetFlags(ev, flags)
	a.CGEventPost(macHIDEventTap, ev)
	a.CFRelease(ev)
	return nil
}

func macMouse(kind string, x, y int, button string, delta int) error {
	a, err := macLoad()
	if err != nil {
		return err
	}
	if err := macEnsureAccessibility(a); err != nil {
		return err
	}
	in := &macIn
	in.mu.Lock()
	defer in.mu.Unlock()
	if kind == "wheel" {
		ev := a.CGEventCreateScrollWheelEvent2(0, macScrollUnitLine, 1, macScrollLines(delta), 0, 0)
		return a.post(ev, in.mods)
	}
	typ, btn, err := macMouseEvent(kind, button, in.pressed)
	if err != nil {
		return err
	}
	ev := a.CGEventCreateMouseEvent(0, typ, macPoint{float64(x), float64(y)}, btn)
	if ev == 0 {
		return fmt.Errorf("创建鼠标事件失败")
	}
	switch kind {
	case "down":
		in.count = macNextClickCount(in.last, time.Now(), x, y, button)
		in.last = macClickRecord{t: time.Now(), x: x, y: y, button: button, count: in.count}
		a.CGEventSetIntegerValueField(ev, macFieldMouseClickState, int64(in.count))
		in.pressed = button
	case "up":
		if in.count < 1 {
			in.count = 1
		}
		a.CGEventSetIntegerValueField(ev, macFieldMouseClickState, int64(in.count))
		in.pressed = ""
	}
	return a.post(ev, in.mods)
}

func macKey(key string, up bool) error {
	a, err := macLoad()
	if err != nil {
		return err
	}
	if err := macEnsureAccessibility(a); err != nil {
		return err
	}
	code, needShift, ok := macKeyCode(key)
	if !ok {
		// 非映射字符（如 é、中）：按下时直接输入 Unicode，抬起忽略。
		if r := []rune(key); len(r) == 1 {
			if up {
				return nil
			}
			return macText(key)
		}
		return fmt.Errorf("unknown key %q", key)
	}
	in := &macIn
	in.mu.Lock()
	defer in.mu.Unlock()
	flags := in.mods
	if f := macModifierFlag(code); f != 0 {
		if up {
			in.mods &^= f
		} else {
			in.mods |= f
		}
		flags = in.mods
	} else if needShift {
		flags |= macFlagShift
	}
	return a.post(a.CGEventCreateKeyboardEvent(0, code, !up), flags)
}

func macText(s string) error {
	a, err := macLoad()
	if err != nil {
		return err
	}
	if err := macEnsureAccessibility(a); err != nil {
		return err
	}
	in := &macIn
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, seg := range macSplitText(s) {
		if seg.Key >= 0 {
			for _, down := range []bool{true, false} {
				if err := a.post(a.CGEventCreateKeyboardEvent(0, uint16(seg.Key), down), 0); err != nil {
					return err
				}
			}
			continue
		}
		for _, down := range []bool{true, false} {
			ev := a.CGEventCreateKeyboardEvent(0, 0, down)
			if ev == 0 {
				return fmt.Errorf("创建键盘事件失败")
			}
			a.CGEventKeyboardSetUnicodeString(ev, uintptr(len(seg.Units)), unsafe.Pointer(&seg.Units[0]))
			if err := a.post(ev, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// platformDesktopOps macOS 后端。
func platformDesktopOps() *DesktopOps {
	return &DesktopOps{
		Displays: macDisplays,
		Capture:  macCapture,
		Bounds:   macBoundsOf,
		Mouse:    macMouse,
		Key:      macKey,
		Text:     macText,
	}
}

// platformReleaseIdle macOS 后端无需空闲回收的缓存（库句柄按需加载，常驻占用很小）。
func platformReleaseIdle() {}
