//go:build linux

package tools

import (
	"errors"
	"fmt"
	"image"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// Linux（X11）后端：纯 Go 的 X11 协议（xgb），不依赖 cgo 与 libX11 头文件。
// 抓屏用核心协议 GetImage，注入用 XTest 扩展，显示器布局用 RandR。
// Wayland 会话下没有 X 显示（或只有 XWayland，抓不到别的窗口），给出明确错误。

type x11Backend struct {
	mu      sync.Mutex
	conn    *xgb.Conn
	root    xproto.Window
	width   int // 根窗口尺寸
	height  int
	minKC   xproto.Keycode
	maxKC   xproto.Keycode
	hasTest bool
	hasRR   bool
	// 临时借用来输入任意 Unicode 的键码；dirtySpare 为真表示映射还没还原。
	spareKC    xproto.Keycode
	dirtySpare bool
}

var x11 = &x11Backend{}

// ensureConn 建立或复用 X 连接；调用方持有 mu。
func (b *x11Backend) ensureConn() error {
	if b.conn != nil {
		return nil
	}
	if os.Getenv("DISPLAY") == "" {
		// ssh 等非图形会话启动时没有 DISPLAY，按常见的本机主显示器兜底。
		os.Setenv("DISPLAY", ":0")
	}
	c, err := xgb.NewConn()
	if err != nil {
		hint := ""
		if strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") {
			hint = "（当前是 Wayland 会话，暂不支持，请切换到 X11 会话）"
		}
		return fmt.Errorf("连接 X 显示 %s 失败: %w%s；请在被控用户的图形会话里启动 share，必要时设置 DISPLAY/XAUTHORITY", os.Getenv("DISPLAY"), err, hint)
	}
	setup := xproto.Setup(c)
	scr := setup.DefaultScreen(c)
	b.conn, b.root = c, scr.Root
	b.width, b.height = int(scr.WidthInPixels), int(scr.HeightInPixels)
	b.minKC, b.maxKC = setup.MinKeycode, setup.MaxKeycode
	b.hasTest = xtest.Init(c) == nil
	b.hasRR = randr.Init(c) == nil
	if b.hasRR {
		if _, err := randr.QueryVersion(c, 1, 3).Reply(); err != nil {
			b.hasRR = false
		}
	}
	if !b.hasTest {
		c.Close()
		b.conn = nil
		return errors.New("X 服务器不支持 XTest 扩展，无法注入输入")
	}
	if err := b.checkImageFormat(setup, scr); err != nil {
		c.Close()
		b.conn = nil
		return err
	}
	return nil
}

// checkImageFormat 只支持常见的 32bpp 小端 BGRX，其余格式明确报错而不是出花屏。
func (b *x11Backend) checkImageFormat(setup *xproto.SetupInfo, scr *xproto.ScreenInfo) error {
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		return errors.New("不支持大端字节序的 X 服务器")
	}
	for _, f := range setup.PixmapFormats {
		if f.Depth == scr.RootDepth {
			if f.BitsPerPixel != 32 {
				return fmt.Errorf("不支持的像素格式: depth=%d bpp=%d（仅支持 32bpp）", f.Depth, f.BitsPerPixel)
			}
			return nil
		}
	}
	return fmt.Errorf("找不到 depth=%d 的像素格式", scr.RootDepth)
}

// drop 丢弃出错的连接，下次调用重连（X 服务器重启/注销后自动恢复）。
func (b *x11Backend) drop() {
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
}

// monitors 返回显示器矩形，主显示器第 0；无 RandR 时退化为整个根窗口。调用方持有 mu。
func (b *x11Backend) monitors() []image.Rectangle {
	whole := []image.Rectangle{image.Rect(0, 0, b.width, b.height)}
	if !b.hasRR {
		return whole
	}
	res, err := randr.GetScreenResourcesCurrent(b.conn, b.root).Reply()
	if err != nil {
		return whole
	}
	var primaryCrtc randr.Crtc
	if po, err := randr.GetOutputPrimary(b.conn, b.root).Reply(); err == nil && po.Output != 0 {
		if oi, err := randr.GetOutputInfo(b.conn, po.Output, res.ConfigTimestamp).Reply(); err == nil {
			primaryCrtc = oi.Crtc
		}
	}
	type mon struct {
		r       image.Rectangle
		primary bool
	}
	var ms []mon
	for _, crtc := range res.Crtcs {
		ci, err := randr.GetCrtcInfo(b.conn, crtc, res.ConfigTimestamp).Reply()
		if err != nil || ci.Width == 0 || ci.Height == 0 || ci.Mode == 0 {
			continue
		}
		ms = append(ms, mon{image.Rect(int(ci.X), int(ci.Y), int(ci.X)+int(ci.Width), int(ci.Y)+int(ci.Height)), crtc == primaryCrtc})
	}
	if len(ms) == 0 {
		return whole
	}
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].primary != ms[j].primary {
			return ms[i].primary
		}
		if ms[i].r.Min.X != ms[j].r.Min.X {
			return ms[i].r.Min.X < ms[j].r.Min.X
		}
		return ms[i].r.Min.Y < ms[j].r.Min.Y
	})
	out := make([]image.Rectangle, len(ms))
	for i, m := range ms {
		out[i] = m.r
	}
	return out
}

func (b *x11Backend) displays() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureConn(); err != nil {
		return 0, err
	}
	return len(b.monitors()), nil
}

func (b *x11Backend) bounds(display int) (image.Rectangle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureConn(); err != nil {
		return image.Rectangle{}, err
	}
	ms := b.monitors()
	if display < 0 || display >= len(ms) {
		return image.Rectangle{}, fmt.Errorf("display %d out of range (have %d)", display, len(ms))
	}
	return ms[display], nil
}

func (b *x11Backend) capture(display int) (*image.RGBA, image.Rectangle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureConn(); err != nil {
		return nil, image.Rectangle{}, err
	}
	ms := b.monitors()
	if display < 0 || display >= len(ms) {
		return nil, image.Rectangle{}, fmt.Errorf("display %d out of range (have %d)", display, len(ms))
	}
	r := ms[display]
	w, h := r.Dx(), r.Dy()
	reply, err := xproto.GetImage(b.conn, xproto.ImageFormatZPixmap, xproto.Drawable(b.root),
		int16(r.Min.X), int16(r.Min.Y), uint16(w), uint16(h), 0xffffffff).Reply()
	if err != nil {
		b.drop()
		return nil, r, fmt.Errorf("GetImage: %w", err)
	}
	if len(reply.Data) < w*h*4 {
		return nil, r, fmt.Errorf("GetImage 返回数据不足: %d < %d", len(reply.Data), w*h*4)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	src, dst := reply.Data, img.Pix
	for i := 0; i+3 < w*h*4; i += 4 { // BGRX → RGBA，强制不透明
		dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 255
	}
	return img, r, nil
}

// XTest FakeInput 事件类型
const (
	xKeyPress      = 2
	xKeyRelease    = 3
	xButtonPress   = 4
	xButtonRelease = 5
	xMotionNotify  = 6
)

// fake 发一个 XTest 事件并同步等待服务器处理完。调用方持有 mu。
func (b *x11Backend) fake(typ, detail byte, x, y int16) error {
	if b.conn == nil {
		return errors.New("X 连接已断开")
	}
	if err := xtest.FakeInputChecked(b.conn, typ, detail, 0, b.root, x, y, 0).Check(); err != nil {
		b.drop()
		return fmt.Errorf("XTest FakeInput: %w", err)
	}
	return nil
}

func (b *x11Backend) mouse(kind string, x, y int, button string, delta int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureConn(); err != nil {
		return err
	}
	switch kind {
	case "move":
		return b.fake(xMotionNotify, 0, clampInt16(x), clampInt16(y))
	case "down", "up":
		btn := buttonCode(button)
		t := byte(xButtonPress)
		if kind == "up" {
			t = xButtonRelease
		}
		return b.fake(t, btn, 0, 0)
	case "wheel":
		// 浏览器 dy>0 向下；X11 里按钮 4=上滚、5=下滚，每次按下抬起算一格（约 120 单位）
		btn := byte(5)
		if delta < 0 {
			btn, delta = 4, -delta
		}
		clicks := (delta + 60) / 120
		if clicks < 1 {
			clicks = 1
		}
		if clicks > 10 {
			clicks = 10
		}
		for i := 0; i < clicks; i++ {
			if err := b.fake(xButtonPress, btn, 0, 0); err != nil {
				return err
			}
			if err := b.fake(xButtonRelease, btn, 0, 0); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown mouse kind %q", kind)
}

func clampInt16(v int) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

func buttonCode(b string) byte {
	switch b {
	case "middle":
		return 2
	case "right":
		return 3
	}
	return 1
}

// keymap 取当前键盘映射：keysyms[(kc-min)*per + col]。调用方持有 mu。
func (b *x11Backend) keymap() ([]xproto.Keysym, int, error) {
	n := int(b.maxKC) - int(b.minKC) + 1
	r, err := xproto.GetKeyboardMapping(b.conn, b.minKC, byte(n)).Reply()
	if err != nil {
		b.drop()
		return nil, 0, fmt.Errorf("GetKeyboardMapping: %w", err)
	}
	return r.Keysyms, int(r.KeysymsPerKeycode), nil
}

// findKeycode 在映射里找 keysym 对应键码及其列（0=无修饰，1=Shift）。跳过借用的 spare 键码。
func findKeycode(ks []xproto.Keysym, per int, min xproto.Keycode, want xproto.Keysym, skip xproto.Keycode) (kc xproto.Keycode, col int, ok bool) {
	if per <= 0 {
		return 0, 0, false
	}
	best, bestCol := xproto.Keycode(0), 99
	for i := 0; i+per <= len(ks); i += per {
		code := xproto.Keycode(i/per) + min
		if code == skip {
			continue
		}
		for c := 0; c < per && c < 2; c++ { // 只认前两列：无修饰与 Shift
			if ks[i+c] == want && c < bestCol {
				best, bestCol = code, c
			}
		}
	}
	if bestCol == 99 {
		return 0, 0, false
	}
	return best, bestCol, true
}

func (b *x11Backend) key(name string, up bool) error {
	sym, ok := keysymForKey(name)
	if !ok {
		return fmt.Errorf("unknown key %q", name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureConn(); err != nil {
		return err
	}
	ks, per, err := b.keymap()
	if err != nil {
		return err
	}
	kc, _, found := findKeycode(ks, per, b.minKC, sym, b.spareKC)
	if !found && sym >= 'A' && sym <= 'Z' { // 大写字母按键码（Shift 由调用方单独发）
		kc, _, found = findKeycode(ks, per, b.minKC, sym+('a'-'A'), b.spareKC)
	}
	if !found {
		return fmt.Errorf("当前键盘布局没有键 %q", name)
	}
	t := byte(xKeyPress)
	if up {
		t = xKeyRelease
	}
	return b.fake(t, byte(kc), 0, 0)
}

func (b *x11Backend) text(s string) (err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err = b.ensureConn(); err != nil {
		return err
	}
	// 无论中途是否出错都要还原借用的键码映射，否则它会一直绑在某个中文字符上
	defer func() {
		if e := b.restoreSpare(); err == nil {
			err = e
		}
	}()
	for _, r := range s {
		if err = b.typeRune(r); err != nil {
			return err
		}
	}
	return nil
}

// typeRune 输入单个字符：键盘映射里有就直接按（需要时带 Shift），否则借用空闲键码临时映射。
func (b *x11Backend) typeRune(r rune) error {
	sym := keysymForRune(r)
	ks, per, err := b.keymap()
	if err != nil {
		return err
	}
	if kc, col, ok := findKeycode(ks, per, b.minKC, sym, b.spareKC); ok {
		return b.tap(kc, col == 1, ks, per)
	}
	return b.typeViaSpare(sym)
}

// tap 按下并抬起一个键，shift 为真时包一层 Shift。
func (b *x11Backend) tap(kc xproto.Keycode, shift bool, ks []xproto.Keysym, per int) error {
	var shiftKC xproto.Keycode
	if shift {
		var ok bool
		if shiftKC, _, ok = findKeycode(ks, per, b.minKC, 0xffe1, 0); !ok { // Shift_L
			return errors.New("找不到 Shift 键")
		}
		if err := b.fake(xKeyPress, byte(shiftKC), 0, 0); err != nil {
			return err
		}
	}
	err := b.fake(xKeyPress, byte(kc), 0, 0)
	if err == nil {
		err = b.fake(xKeyRelease, byte(kc), 0, 0)
	}
	if shift && b.conn != nil { // 连接已断开时服务器会自行释放，不必也无法再发
		if e := b.fake(xKeyRelease, byte(shiftKC), 0, 0); err == nil {
			err = e
		}
	}
	return err
}

// x11MappingSettle 改键盘映射/按键后等应用刷新映射的时间。太短时负载高的机器上（如虚拟机）
// Qt/GTK 应用还没处理完 MappingNotify，下一个字就按下，会丢字；实测 15ms 偶发丢字。
const x11MappingSettle = 40 * time.Millisecond

// typeViaSpare 把 sym 临时绑定到一个空闲键码上再按它（xdotool 的做法），用于映射里没有的字符（中文等）。
func (b *x11Backend) typeViaSpare(sym xproto.Keysym) error {
	if b.spareKC == 0 {
		kc, err := b.pickSpare()
		if err != nil {
			return err
		}
		b.spareKC = kc
	}
	// 写两列（无修饰/Shift 同一 keysym）：单列时 Xlib 会按大小写对解释，Á 之类会被输成小写
	if err := xproto.ChangeKeyboardMappingChecked(b.conn, 1, b.spareKC, 2, []xproto.Keysym{sym, sym}).Check(); err != nil {
		b.drop()
		return fmt.Errorf("ChangeKeyboardMapping: %w", err)
	}
	b.dirtySpare = true
	// 等服务器把 MappingNotify 发给各客户端，应用重新读取映射后再按键
	if _, err := xproto.GetInputFocus(b.conn).Reply(); err != nil {
		return err
	}
	time.Sleep(x11MappingSettle)
	if err := b.fake(xKeyPress, byte(b.spareKC), 0, 0); err != nil {
		return err
	}
	if err := b.fake(xKeyRelease, byte(b.spareKC), 0, 0); err != nil {
		return err
	}
	time.Sleep(x11MappingSettle) // 让应用先处理按键，再换回映射
	return nil
}

// pickSpare 选一个没有任何键盘符号的键码；找不到就报错（不能借用真实按键，还原时会把它清空）。
func (b *x11Backend) pickSpare() (xproto.Keycode, error) {
	ks, per, err := b.keymap()
	if err != nil {
		return 0, err
	}
	if per > 0 {
		for i := len(ks) - per; i >= 0; i -= per {
			empty := true
			for c := 0; c < per; c++ {
				if ks[i+c] != 0 {
					empty = false
					break
				}
			}
			if empty {
				return xproto.Keycode(i/per) + b.minKC, nil
			}
		}
	}
	return 0, errors.New("键盘映射里没有空闲键码，无法输入映射外的字符（如中文）")
}

func (b *x11Backend) restoreSpare() error {
	if !b.dirtySpare || b.conn == nil {
		return nil
	}
	b.dirtySpare = false
	if err := xproto.ChangeKeyboardMappingChecked(b.conn, 1, b.spareKC, 2, []xproto.Keysym{0, 0}).Check(); err != nil {
		b.drop()
		return fmt.Errorf("还原键盘映射失败: %w", err)
	}
	return nil
}

// keysymForRune 字符 → X keysym：Latin-1 范围与码点相同，其余用 0x01000000+码点。
func keysymForRune(r rune) xproto.Keysym {
	switch {
	case r == '\n' || r == '\r':
		return 0xff0d
	case r == '\t':
		return 0xff09
	case (r >= 0x20 && r <= 0x7e) || (r >= 0xa0 && r <= 0xff):
		return xproto.Keysym(r)
	}
	return xproto.Keysym(0x01000000 + int(r))
}

var x11NamedKeys = map[string]xproto.Keysym{
	"backspace": 0xff08, "tab": 0xff09, "enter": 0xff0d, "escape": 0xff1b, "esc": 0xff1b,
	"delete": 0xffff, "insert": 0xff63, "home": 0xff50, "end": 0xff57, "pageup": 0xff55, "pagedown": 0xff56,
	"arrowleft": 0xff51, "arrowup": 0xff52, "arrowright": 0xff53, "arrowdown": 0xff54,
	"shift": 0xffe1, "control": 0xffe3, "alt": 0xffe9, "meta": 0xffeb, "os": 0xffeb, "capslock": 0xffe5,
	"contextmenu": 0xff67, "pause": 0xff13, "printscreen": 0xff61, "space": 0x20, " ": 0x20,
}

// keysymForKey 浏览器 KeyboardEvent.key → X keysym。
func keysymForKey(key string) (xproto.Keysym, bool) {
	lk := strings.ToLower(key)
	if s, ok := x11NamedKeys[lk]; ok {
		return s, true
	}
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
			return xproto.Keysym(0xffbe + n - 1), true
		}
	}
	if rs := []rune(key); len(rs) == 1 {
		return keysymForRune(rs[0]), true
	}
	return 0, false
}

func platformDesktopOps() *DesktopOps {
	return &DesktopOps{
		Displays: x11.displays,
		Capture:  x11.capture,
		Bounds:   x11.bounds,
		Mouse:    x11.mouse,
		Key:      x11.key,
		Text:     x11.text,
	}
}

// platformReleaseIdle 空闲时关闭 X 连接（先还原借用的键码映射），下次调用自动重连。
func platformReleaseIdle() {
	x11.mu.Lock()
	defer x11.mu.Unlock()
	if x11.conn == nil {
		return
	}
	_ = x11.restoreSpare()
	x11.drop()
}
