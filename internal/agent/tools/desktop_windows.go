//go:build windows

package tools

import (
	"errors"
	"fmt"
	"image"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	pEnumDisplayMonitors           = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	pGetDC                         = user32.NewProc("GetDC")
	pReleaseDC                     = user32.NewProc("ReleaseDC")
	pSetCursorPos                  = user32.NewProc("SetCursorPos")
	pSendInput                     = user32.NewProc("SendInput")
	pMapVirtualKeyW                = user32.NewProc("MapVirtualKeyW")
	pVkKeyScanW                    = user32.NewProc("VkKeyScanW")

	pCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	pSelectObject           = gdi32.NewProc("SelectObject")
	pBitBlt                 = gdi32.NewProc("BitBlt")
	pGetDIBits              = gdi32.NewProc("GetDIBits")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pDeleteDC               = gdi32.NewProc("DeleteDC")
)

const (
	srccopy        = 0x00CC0020
	captureblt     = 0x40000000
	dibRGBColors   = 0
	monitorPrimary = 1

	inputMouseType    = 0
	inputKeyboardType = 1

	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfRightDown  = 0x0008
	mouseeventfRightUp    = 0x0010
	mouseeventfMiddleDown = 0x0020
	mouseeventfMiddleUp   = 0x0040
	mouseeventfWheel      = 0x0800

	keyeventfExtended = 0x0001
	keyeventfKeyUp    = 0x0002
	keyeventfUnicode  = 0x0004
)

var dpiOnce sync.Once

// ensureDPIAware 让进程按物理像素工作，否则高 DPI 缩放下截图与坐标会被系统虚拟化。
func ensureDPIAware() {
	dpiOnce.Do(func() {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
		if pSetProcessDpiAwarenessContext.Find() == nil {
			minus4 := ^uintptr(3)
			if r, _, _ := pSetProcessDpiAwarenessContext.Call(minus4); r != 0 {
				return
			}
		}
		pSetProcessDPIAware.Call()
	})
}

type monitorInfo struct {
	cbSize    uint32
	rcMonitor windows.Rect
	rcWork    windows.Rect
	dwFlags   uint32
}

type monitorEntry struct {
	r       image.Rectangle
	primary bool
}

// 枚举回调只能创建一次：syscall.NewCallback 创建的回调永不释放，进程内上限约 2000 个，
// 超出会 fatal error: too many callback functions 直接让进程退出。每帧抓屏/每次输入都要
// 枚举显示器，若每次都新建回调，几分钟就会触顶（被控端会莫名崩溃）。
// 回调把结果追加到 monitorCollector，由 monitorMu 串行化各次枚举。
var (
	monitorMu        sync.Mutex
	monitorCollector []monitorEntry
	monitorEnumCB    = syscall.NewCallback(func(hMon, _, _, _ uintptr) uintptr {
		mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := pGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); r != 0 {
			monitorCollector = append(monitorCollector, monitorEntry{
				r:       image.Rect(int(mi.rcMonitor.Left), int(mi.rcMonitor.Top), int(mi.rcMonitor.Right), int(mi.rcMonitor.Bottom)),
				primary: mi.dwFlags&monitorPrimary != 0,
			})
		}
		return 1
	})
)

// listMonitors 返回显示器矩形，主显示器排第 0，其余按左上角坐标排序。
func listMonitors() ([]image.Rectangle, error) {
	ensureDPIAware()
	monitorMu.Lock()
	monitorCollector = monitorCollector[:0]
	r, _, e := pEnumDisplayMonitors.Call(0, 0, monitorEnumCB, 0)
	mons := append([]monitorEntry(nil), monitorCollector...)
	monitorMu.Unlock()
	if r == 0 {
		return nil, fmt.Errorf("EnumDisplayMonitors: %v", e)
	}
	if len(mons) == 0 {
		return nil, errors.New("no display found")
	}
	sort.SliceStable(mons, func(i, j int) bool {
		if mons[i].primary != mons[j].primary {
			return mons[i].primary
		}
		if mons[i].r.Min.X != mons[j].r.Min.X {
			return mons[i].r.Min.X < mons[j].r.Min.X
		}
		return mons[i].r.Min.Y < mons[j].r.Min.Y
	})
	out := make([]image.Rectangle, len(mons))
	for i, m := range mons {
		out[i] = m.r
	}
	return out, nil
}

func monitorRect(display int) (image.Rectangle, error) {
	ms, err := listMonitors()
	if err != nil {
		return image.Rectangle{}, err
	}
	if display < 0 || display >= len(ms) {
		return image.Rectangle{}, fmt.Errorf("display %d out of range (have %d)", display, len(ms))
	}
	return ms[display], nil
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

func winCapture(display int) (*image.RGBA, image.Rectangle, error) {
	rect, err := monitorRect(display)
	if err != nil {
		return nil, rect, err
	}
	w, h := rect.Dx(), rect.Dy()
	hdcScreen, _, e := pGetDC.Call(0)
	if hdcScreen == 0 {
		return nil, rect, fmt.Errorf("GetDC: %v", e)
	}
	defer pReleaseDC.Call(0, hdcScreen)
	hdcMem, _, e := pCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return nil, rect, fmt.Errorf("CreateCompatibleDC: %v", e)
	}
	defer pDeleteDC.Call(hdcMem)
	hBmp, _, e := pCreateCompatibleBitmap.Call(hdcScreen, uintptr(w), uintptr(h))
	if hBmp == 0 {
		return nil, rect, fmt.Errorf("CreateCompatibleBitmap: %v", e)
	}
	defer pDeleteObject.Call(hBmp)
	old, _, _ := pSelectObject.Call(hdcMem, hBmp)
	defer pSelectObject.Call(hdcMem, old)
	if r, _, e := pBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h), hdcScreen,
		uintptr(rect.Min.X), uintptr(rect.Min.Y), srccopy|captureblt); r == 0 {
		return nil, rect, fmt.Errorf("BitBlt: %v（可能处于锁屏/断开的远程会话）", e)
	}
	bih := bitmapInfoHeader{biSize: uint32(unsafe.Sizeof(bitmapInfoHeader{})), biWidth: int32(w),
		biHeight: -int32(h), biPlanes: 1, biBitCount: 32}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if r, _, e := pGetDIBits.Call(hdcMem, hBmp, 0, uintptr(h), uintptr(unsafe.Pointer(&img.Pix[0])),
		uintptr(unsafe.Pointer(&bih)), dibRGBColors); r == 0 {
		return nil, rect, fmt.Errorf("GetDIBits: %v", e)
	}
	// GDI 给的是 BGRA，且 alpha 不可靠：换成 RGBA 并强制不透明。
	p := img.Pix
	for i := 0; i+3 < len(p); i += 4 {
		p[i], p[i+2] = p[i+2], p[i]
		p[i+3] = 255
	}
	return img, rect, nil
}

// ---- SendInput ----
// 64 位下 INPUT = type(4) + 对齐填充(4) + union(32)；32 位无填充。两种输入共用同一尺寸。
type mouseInput struct {
	dx, dy      int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

type keybdInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
	_           [8]byte // 补齐到 mouseInput 的大小
}

type inputMouse struct {
	typ uint32
	_   [unsafe.Sizeof(uintptr(0)) - 4]byte
	mi  mouseInput
}

type inputKey struct {
	typ uint32
	_   [unsafe.Sizeof(uintptr(0)) - 4]byte
	ki  keybdInput
}

func sendMouse(flags uint32, data int32) error {
	in := inputMouse{typ: inputMouseType, mi: mouseInput{dwFlags: flags, mouseData: uint32(data)}}
	return sendRaw(unsafe.Pointer(&in), unsafe.Sizeof(in))
}

func sendKeyRaw(vk, scan uint16, flags uint32) error {
	in := inputKey{typ: inputKeyboardType, ki: keybdInput{wVk: vk, wScan: scan, dwFlags: flags}}
	return sendRaw(unsafe.Pointer(&in), unsafe.Sizeof(in))
}

func sendRaw(p unsafe.Pointer, size uintptr) error {
	r, _, e := pSendInput.Call(1, uintptr(p), size)
	if r != 1 {
		return fmt.Errorf("SendInput: %v（目标窗口可能是管理员权限/UAC 安全桌面）", e)
	}
	return nil
}

func winMouse(kind string, x, y int, button string, delta int) error {
	switch kind {
	case "move":
		if r, _, e := pSetCursorPos.Call(uintptr(x), uintptr(y)); r == 0 {
			return fmt.Errorf("SetCursorPos: %v", e)
		}
		return nil
	case "down", "up":
		var f uint32
		switch {
		case button == "right" && kind == "down":
			f = mouseeventfRightDown
		case button == "right":
			f = mouseeventfRightUp
		case button == "middle" && kind == "down":
			f = mouseeventfMiddleDown
		case button == "middle":
			f = mouseeventfMiddleUp
		case kind == "down":
			f = mouseeventfLeftDown
		default:
			f = mouseeventfLeftUp
		}
		return sendMouse(f, 0)
	case "wheel":
		// 浏览器 deltaY>0 表示向下，Windows 向下滚是负值；每 120 为一格。
		return sendMouse(mouseeventfWheel, int32(-delta))
	}
	return fmt.Errorf("unknown mouse kind %q", kind)
}

// namedKeys 浏览器 KeyboardEvent.key（小写）→ Windows 虚拟键码。
var namedKeys = map[string]uint16{
	"backspace": 0x08, "tab": 0x09, "enter": 0x0D, "shift": 0x10, "control": 0x11, "alt": 0x12,
	"pause": 0x13, "capslock": 0x14, "escape": 0x1B, "esc": 0x1B, " ": 0x20, "space": 0x20,
	"pageup": 0x21, "pagedown": 0x22, "end": 0x23, "home": 0x24,
	"arrowleft": 0x25, "arrowup": 0x26, "arrowright": 0x27, "arrowdown": 0x28,
	"printscreen": 0x2C, "insert": 0x2D, "delete": 0x2E,
	"meta": 0x5B, "os": 0x5B, "contextmenu": 0x5D,
}

var extendedVK = map[uint16]bool{
	0x21: true, 0x22: true, 0x23: true, 0x24: true, 0x25: true, 0x26: true, 0x27: true, 0x28: true,
	0x2C: true, 0x2D: true, 0x2E: true, 0x5B: true, 0x5D: true,
}

func resolveVK(key string) (uint16, bool) {
	lk := strings.ToLower(key)
	if vk, ok := namedKeys[lk]; ok {
		return vk, true
	}
	// F1..F24
	if len(lk) >= 2 && lk[0] == 'f' {
		n := 0
		for _, c := range lk[1:] {
			if c < '0' || c > '9' {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n >= 1 && n <= 24 {
			return uint16(0x6F + n), true
		}
	}
	// 单个字符：用 VkKeyScanW 取其虚拟键码（忽略 Shift 状态位，由调用方单独发 Shift）
	rs := []rune(key)
	if len(rs) == 1 && rs[0] < 0x10000 {
		r, _, _ := pVkKeyScanW.Call(uintptr(rs[0]))
		if int16(r) != -1 {
			return uint16(r & 0xFF), true
		}
	}
	return 0, false
}

func winKey(key string, up bool) error {
	vk, ok := resolveVK(key)
	if !ok {
		return fmt.Errorf("unknown key %q", key)
	}
	scan, _, _ := pMapVirtualKeyW.Call(uintptr(vk), 0)
	var f uint32
	if extendedVK[vk] {
		f |= keyeventfExtended
	}
	if up {
		f |= keyeventfKeyUp
	}
	return sendKeyRaw(vk, uint16(scan), f)
}

func winText(s string) error {
	for _, u := range utf16.Encode([]rune(s)) {
		if err := sendKeyRaw(0, u, keyeventfUnicode); err != nil {
			return err
		}
		if err := sendKeyRaw(0, u, keyeventfUnicode|keyeventfKeyUp); err != nil {
			return err
		}
	}
	return nil
}

func platformDesktopOps() *DesktopOps {
	ensureDPIAware()
	return &DesktopOps{
		Displays: func() (int, error) {
			ms, err := listMonitors()
			return len(ms), err
		},
		Capture: winCapture,
		Bounds:  monitorRect,
		Mouse:   winMouse,
		Key:     winKey,
		Text:    winText,
	}
}
