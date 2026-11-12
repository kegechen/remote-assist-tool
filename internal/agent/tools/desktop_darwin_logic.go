package tools

// macOS 远程桌面后端中与平台 API 无关的纯逻辑：键名→虚拟键码、像素格式转换、
// 鼠标事件类型选择、文本分段、连击计数、错误文案。
// 本文件不带 build tag，便于在 Windows/Linux 上编译与单测；仅 desktop_darwin.go 调用系统 API。
// 所有标识符统一加 mac 前缀，避免与其他平台文件重名。

import (
	"errors"
	"fmt"
	"image"
	"strings"
	"time"
	"unicode/utf16"
)

// ---------- 错误文案 ----------

const macPermHint = "系统设置 → 隐私与安全性 → %s，勾选运行 remote 的终端（或 remote 可执行文件），然后完全退出并重新打开该终端、重新运行 remote share 后再试"

// macErrScreenRecording 无“屏幕录制”权限。
func macErrScreenRecording() error {
	return fmt.Errorf("macos_permission_screen_recording: 缺少“屏幕录制”权限，无法抓取屏幕（否则只会得到壁纸/黑屏）。请打开 "+macPermHint, "屏幕录制")
}

// macErrAccessibility 无“辅助功能”权限。
func macErrAccessibility() error {
	return fmt.Errorf("macos_permission_accessibility: 缺少“辅助功能”权限，无法注入鼠标/键盘事件。请打开 "+macPermHint, "辅助功能")
}

// macErrLoad 动态库/符号加载失败。
func macErrLoad(err error) error {
	return fmt.Errorf("macos_load_failed: 加载 macOS 系统库失败: %w", err)
}

// ---------- 像素格式转换 ----------

// CGBitmapInfo 掩码。
const (
	macAlphaInfoMask     = 0x1F
	macByteOrderMask     = 0x7000
	macByteOrder32Little = 2 << 12
	macByteOrder32Big    = 4 << 12
)

// CGImageAlphaInfo：First 类 alpha 在最高位（ARGB），Last 类在最低位（RGBA）。
func macAlphaFirst(info uint32) (first bool, ok bool) {
	switch info & macAlphaInfoMask {
	case 2, 4, 6: // PremultipliedFirst / First / NoneSkipFirst
		return true, true
	case 1, 3, 5: // PremultipliedLast / Last / NoneSkipLast
		return false, true
	}
	return false, false
}

// macConvertPixels 把 CGImage 的原始像素（32bpp，带每行字节对齐 stride）转成不透明的 RGBA 图像。
// 屏幕图像 alpha 视为 255，因此无需处理预乘。
func macConvertPixels(src []byte, w, h, stride, bpp int, bitmapInfo uint32) (*image.RGBA, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid image size %dx%d", w, h)
	}
	if bpp != 32 {
		return nil, fmt.Errorf("unsupported bits per pixel %d (need 32)", bpp)
	}
	if stride < w*4 {
		return nil, fmt.Errorf("invalid bytes per row %d for width %d", stride, w)
	}
	if need := (h-1)*stride + w*4; len(src) < need {
		return nil, fmt.Errorf("pixel data too short: %d < %d", len(src), need)
	}
	first, ok := macAlphaFirst(bitmapInfo)
	if !ok {
		return nil, fmt.Errorf("unsupported bitmap alpha info %#x", bitmapInfo)
	}
	little := false
	switch bitmapInfo & macByteOrderMask {
	case macByteOrder32Little:
		little = true
	case 0, macByteOrder32Big:
	default:
		return nil, fmt.Errorf("unsupported byte order %#x", bitmapInfo&macByteOrderMask)
	}
	// 以 32 位整数读：first=ARGB，否则 RGBA。little 时内存字节序与整数相反。
	// 求出 r/g/b 三个分量在内存 4 字节中的下标。
	var ri, gi, bi int
	switch {
	case first && little: // 内存 B G R A
		ri, gi, bi = 2, 1, 0
	case first && !little: // 内存 A R G B
		ri, gi, bi = 1, 2, 3
	case !first && little: // 内存 A B G R
		ri, gi, bi = 3, 2, 1
	default: // 内存 R G B A
		ri, gi, bi = 0, 1, 2
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		s := src[y*stride : y*stride+w*4]
		d := dst.Pix[y*dst.Stride : y*dst.Stride+w*4]
		for x := 0; x < w*4; x += 4 {
			d[x], d[x+1], d[x+2], d[x+3] = s[x+ri], s[x+gi], s[x+bi], 255
		}
	}
	return dst, nil
}

// ---------- 鼠标 ----------

// CGEventType / CGMouseButton 常量。
const (
	macEvLeftDown     = 1
	macEvLeftUp       = 2
	macEvRightDown    = 3
	macEvRightUp      = 4
	macEvMoved        = 5
	macEvLeftDragged  = 6
	macEvRightDragged = 7
	macEvOtherDown    = 25
	macEvOtherUp      = 26
	macEvOtherDragged = 27

	macBtnLeft   = 0
	macBtnRight  = 1
	macBtnCenter = 2
)

// macMouseEvent 选择 CGEvent 类型与按键。pressed 为当前按住的键（""=无）：
// 按住期间的 move 必须发 Dragged 事件，否则拖拽不生效。
func macMouseEvent(kind, button, pressed string) (evType uint32, btn uint32, err error) {
	btnOf := func(b string) (down, up, drag uint32, id uint32, err error) {
		switch b {
		case "", "left":
			return macEvLeftDown, macEvLeftUp, macEvLeftDragged, macBtnLeft, nil
		case "right":
			return macEvRightDown, macEvRightUp, macEvRightDragged, macBtnRight, nil
		case "middle":
			return macEvOtherDown, macEvOtherUp, macEvOtherDragged, macBtnCenter, nil
		}
		return 0, 0, 0, 0, fmt.Errorf("unknown button %q", b)
	}
	switch kind {
	case "move":
		if pressed == "" {
			return macEvMoved, macBtnLeft, nil
		}
		_, _, drag, id, e := btnOf(pressed)
		return drag, id, e
	case "down":
		down, _, _, id, e := btnOf(button)
		return down, id, e
	case "up":
		_, up, _, id, e := btnOf(button)
		return up, id, e
	}
	return 0, 0, fmt.Errorf("unknown mouse kind %q", kind)
}

// macScrollLines 把 wheel delta（>0 向下，约 120 一格）换算成 CGEvent 的行数。
// CGEvent 滚轮正值=向上，所以取反；非 0 的 delta 至少滚 1 行。
func macScrollLines(delta int) int32 {
	if delta == 0 {
		return 0
	}
	lines := delta / 120
	if lines == 0 {
		if delta > 0 {
			lines = 1
		} else {
			lines = -1
		}
	}
	if lines > 100 {
		lines = 100
	} else if lines < -100 {
		lines = -100
	}
	return int32(-lines)
}

// 连击判定阈值。
const (
	macClickInterval = 500 * time.Millisecond
	macClickRadius   = 6
)

type macClickRecord struct {
	t      time.Time
	x, y   int
	button string
	count  int
}

// macNextClickCount 计算本次 down 的连击序号（1=单击，2=双击…）；
// 同键、间隔与位移都在阈值内则累加，否则重置为 1。
func macNextClickCount(prev macClickRecord, now time.Time, x, y int, button string) int {
	if prev.count > 0 && prev.button == button && now.Sub(prev.t) <= macClickInterval &&
		macAbs(prev.x-x) <= macClickRadius && macAbs(prev.y-y) <= macClickRadius {
		return prev.count + 1
	}
	return 1
}

func macAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---------- 键盘 ----------

// CGEventFlags 修饰键位。
const (
	macFlagCapsLock = 0x00010000
	macFlagShift    = 0x00020000
	macFlagControl  = 0x00040000
	macFlagAlt      = 0x00080000
	macFlagCommand  = 0x00100000
)

// macKeyReturn / macKeyTab 供文本分段使用。
const (
	macKeyReturn = 36
	macKeyTab    = 48
)

// macKeyCodes 浏览器 key 名（及无需 Shift 的字符）→ kVK_* 虚拟键码（ANSI/US 物理位置）。
var macKeyCodes = map[string]uint16{
	"a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9,
	"b": 11, "q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17,
	"1": 18, "2": 19, "3": 20, "4": 21, "6": 22, "5": 23, "=": 24, "9": 25, "7": 26,
	"-": 27, "8": 28, "0": 29, "]": 30, "o": 31, "u": 32, "[": 33, "i": 34, "p": 35,
	"l": 37, "j": 38, "'": 39, "k": 40, ";": 41, "\\": 42, ",": 43, "/": 44,
	"n": 45, "m": 46, ".": 47, "`": 50,

	"Enter": macKeyReturn, "Tab": macKeyTab, " ": 49, "Spacebar": 49,
	"Backspace": 51, "Escape": 53, "Esc": 53,
	"Delete": 117, // 浏览器 Delete = 向前删除 = kVK_ForwardDelete
	"Home":   115, "End": 119, "PageUp": 116, "PageDown": 121,
	"ArrowLeft": 123, "ArrowRight": 124, "ArrowDown": 125, "ArrowUp": 126,
	"Left": 123, "Right": 124, "Down": 125, "Up": 126,

	"Meta": 55, "Command": 55, "OS": 55, "Super": 55,
	"Shift": 56, "CapsLock": 57, "Alt": 58, "Option": 58, "Control": 59, "Ctrl": 59,
	"Fn": 63,

	"F1": 122, "F2": 120, "F3": 99, "F4": 118, "F5": 96, "F6": 97, "F7": 98, "F8": 100,
	"F9": 101, "F10": 109, "F11": 103, "F12": 111,
}

// macShiftedKeys 需按住 Shift 才能打出的符号 → 底层键对应的无 Shift 字符。
var macShiftedKeys = map[string]string{
	"!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6", "&": "7", "*": "8",
	"(": "9", ")": "0", "_": "-", "+": "=", "{": "[", "}": "]", "|": "\\", ":": ";",
	"\"": "'", "<": ",", ">": ".", "?": "/", "~": "`",
}

// macKeyCode 把浏览器 key 名转为 kVK 键码。needShift 表示该字符本身需要 Shift
// （大写字母、上档符号），即使调用方没单独按下 Shift 也补上，保证输出字符正确。
func macKeyCode(key string) (code uint16, needShift bool, ok bool) {
	if c, found := macKeyCodes[key]; found {
		return c, false, true
	}
	if base, found := macShiftedKeys[key]; found {
		return macKeyCodes[base], true, true
	}
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		return macKeyCodes[strings.ToLower(key)], true, true
	}
	return 0, false, false
}

// macModifierFlag 返回修饰键键码对应的 CGEventFlags 位，非修饰键返回 0。
func macModifierFlag(code uint16) uint64 {
	switch code {
	case 54, 55:
		return macFlagCommand
	case 56, 60:
		return macFlagShift
	case 58, 61:
		return macFlagAlt
	case 59, 62:
		return macFlagControl
	}
	return 0
}

// ---------- 文本 ----------

// CGEventKeyboardSetUnicodeString 单次最多 20 个 UTF-16 单元。
const macMaxUnicodeUnits = 20

// macTextSeg 文本分段：Key>=0 表示按一次该键码（换行/制表），否则发送 Units 对应的 Unicode。
type macTextSeg struct {
	Key   int
	Units []uint16
}

// macSplitText 把文本拆成可发送的片段：\n \r 转 Return、\t 转 Tab，
// 其余按每段不超过 20 个 UTF-16 单元切分且不拆代理对。\r\n 只算一次换行。
func macSplitText(s string) []macTextSeg {
	var segs []macTextSeg
	var cur []uint16
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, macTextSeg{Key: -1, Units: cur})
			cur = nil
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '\r', '\n':
			if r == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			flush()
			segs = append(segs, macTextSeg{Key: macKeyReturn})
			continue
		case '\t':
			flush()
			segs = append(segs, macTextSeg{Key: macKeyTab})
			continue
		}
		u := utf16.Encode([]rune{r})
		if len(cur)+len(u) > macMaxUnicodeUnits {
			flush()
		}
		cur = append(cur, u...)
	}
	flush()
	return segs
}

// ---------- 显示器 ----------

// macDisplayOrder 把 CGGetActiveDisplayList 的结果重排成主显示器在前，其余保持原顺序。
func macDisplayOrder(ids []uint32, main uint32) []uint32 {
	out := make([]uint32, 0, len(ids))
	for _, id := range ids {
		if id == main {
			out = append(out, id)
		}
	}
	for _, id := range ids {
		if id != main {
			out = append(out, id)
		}
	}
	return out
}

// macPickDisplay 按序号取显示器 ID。
func macPickDisplay(ids []uint32, display int) (uint32, error) {
	if len(ids) == 0 {
		return 0, errors.New("no active display")
	}
	if display < 0 || display >= len(ids) {
		return 0, fmt.Errorf("display %d out of range (0..%d)", display, len(ids)-1)
	}
	return ids[display], nil
}

// macRectFromCG 把 CGRect（点坐标，float64）四舍五入为整数矩形。
// 宽高非正/NaN 时返回 ok=false，调用方应改用 CGDisplayPixelsWide/High 兜底。
func macRectFromCG(x, y, w, h float64) (image.Rectangle, bool) {
	if !(w > 0) || !(h > 0) || w > 1e6 || h > 1e6 { // !(w>0) 同时排除 NaN
		return image.Rectangle{}, false
	}
	round := func(v float64) int {
		if v < 0 {
			return int(v - 0.5)
		}
		return int(v + 0.5)
	}
	x0, y0 := round(x), round(y)
	return image.Rect(x0, y0, x0+round(w), y0+round(h)), true
}
