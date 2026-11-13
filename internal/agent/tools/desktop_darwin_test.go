//go:build darwin

package tools

import (
	"image"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestMacKeyCode(t *testing.T) {
	cases := []struct {
		key   string
		code  uint16
		shift bool
	}{
		{"a", 0, false}, {"A", 0, true}, {"z", 6, false}, {"5", 23, false}, {"!", 18, true},
		{"Enter", 36, false}, {"Tab", 48, false}, {" ", 49, false}, {"Backspace", 51, false},
		{"Escape", 53, false}, {"Delete", 117, false}, {"Home", 115, false}, {"End", 119, false},
		{"PageUp", 116, false}, {"PageDown", 121, false},
		{"ArrowLeft", 123, false}, {"ArrowRight", 124, false}, {"ArrowDown", 125, false}, {"ArrowUp", 126, false},
		{"F1", 122, false}, {"F5", 96, false}, {"F12", 111, false},
		{"Shift", 56, false}, {"Control", 59, false}, {"Alt", 58, false}, {"Meta", 55, false}, {"CapsLock", 57, false},
		{"?", 44, true}, {"~", 50, true}, {"=", 24, false},
	}
	for _, c := range cases {
		code, shift, ok := macKeyCode(c.key)
		if !ok || code != c.code || shift != c.shift {
			t.Errorf("macKeyCode(%q)=%d,%v,%v 期望 %d,%v", c.key, code, shift, ok, c.code, c.shift)
		}
	}
	for _, k := range []string{"NoSuchKey", "F99", "中", ""} {
		if _, _, ok := macKeyCode(k); ok {
			t.Errorf("%q 不应识别", k)
		}
	}
}

func TestMacKeyCodesUniqueExceptAliases(t *testing.T) {
	// 每个 F1-F12、字母数字都必须存在。
	for _, k := range strings.Split("abcdefghijklmnopqrstuvwxyz0123456789", "") {
		if _, _, ok := macKeyCode(k); !ok {
			t.Errorf("缺少 %q", k)
		}
	}
}

func TestMacModifierFlag(t *testing.T) {
	if macModifierFlag(56) != macFlagShift || macModifierFlag(59) != macFlagControl ||
		macModifierFlag(58) != macFlagAlt || macModifierFlag(55) != macFlagCommand || macModifierFlag(0) != 0 {
		t.Error("修饰键标志位不对")
	}
}

func TestMacConvertPixels(t *testing.T) {
	// 2x1 图，stride 12（每行 4 字节对齐填充）；两个像素 (R,G,B)=(10,20,30)、(40,50,60)
	cases := []struct {
		name string
		info uint32
		px   []byte
	}{
		{"BGRA little premulFirst", 2<<12 | 2, []byte{30, 20, 10, 255, 60, 50, 40, 255, 0, 0, 0, 0}},
		{"RGBA big premulLast", 4<<12 | 1, []byte{10, 20, 30, 255, 40, 50, 60, 255, 0, 0, 0, 0}},
		{"ARGB big first", 4<<12 | 2, []byte{255, 10, 20, 30, 255, 40, 50, 60, 0, 0, 0, 0}},
		{"ABGR little last", 2<<12 | 1, []byte{255, 30, 20, 10, 255, 60, 50, 40, 0, 0, 0, 0}},
		{"default order noneSkipLast", 5, []byte{10, 20, 30, 0, 40, 50, 60, 0, 0, 0, 0, 0}},
	}
	for _, c := range cases {
		img, err := macConvertPixels(c.px, 2, 1, 12, 32, c.info)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := []byte{10, 20, 30, 255, 40, 50, 60, 255}
		if !reflect.DeepEqual(img.Pix[:8], want) {
			t.Errorf("%s: %v 期望 %v", c.name, img.Pix[:8], want)
		}
	}
	// stride 大于 w*4 且多行：第二行不应读到填充
	src := make([]byte, 16*2)
	src[16], src[17], src[18], src[19] = 3, 2, 1, 0 // 第二行首像素 BGRA
	img, err := macConvertPixels(src, 1, 2, 16, 32, 2<<12|2)
	if err != nil || img.Pix[img.Stride+0] != 1 || img.Pix[img.Stride+1] != 2 || img.Pix[img.Stride+2] != 3 {
		t.Errorf("多行 stride 转换错误: %v %v", err, img)
	}
	// 错误路径
	if _, err := macConvertPixels(src, 2, 1, 4, 32, 2); err == nil {
		t.Error("stride 过小应报错")
	}
	if _, err := macConvertPixels(src[:4], 2, 1, 8, 32, 2); err == nil {
		t.Error("数据过短应报错")
	}
	if _, err := macConvertPixels(src, 1, 1, 4, 16, 2); err == nil {
		t.Error("非 32bpp 应报错")
	}
	if _, err := macConvertPixels(src, 1, 1, 4, 32, 0); err == nil {
		t.Error("无 alpha 信息应报错")
	}
}

func TestMacMouseEvent(t *testing.T) {
	type r struct{ typ, btn uint32 }
	cases := []struct {
		kind, button, pressed string
		want                  r
	}{
		{"move", "", "", r{macEvMoved, 0}},
		{"move", "", "left", r{macEvLeftDragged, macBtnLeft}},
		{"move", "", "right", r{macEvRightDragged, macBtnRight}},
		{"move", "", "middle", r{macEvOtherDragged, macBtnCenter}},
		{"down", "left", "", r{macEvLeftDown, macBtnLeft}},
		{"up", "left", "left", r{macEvLeftUp, macBtnLeft}},
		{"down", "right", "", r{macEvRightDown, macBtnRight}},
		{"up", "right", "", r{macEvRightUp, macBtnRight}},
		{"down", "middle", "", r{macEvOtherDown, macBtnCenter}},
		{"up", "middle", "", r{macEvOtherUp, macBtnCenter}},
	}
	for _, c := range cases {
		typ, btn, err := macMouseEvent(c.kind, c.button, c.pressed)
		if err != nil || typ != c.want.typ || btn != c.want.btn {
			t.Errorf("%+v => %d,%d,%v", c, typ, btn, err)
		}
	}
	if _, _, err := macMouseEvent("down", "x", ""); err == nil {
		t.Error("未知按键应报错")
	}
	if _, _, err := macMouseEvent("zoom", "", ""); err == nil {
		t.Error("未知 kind 应报错")
	}
}

func TestMacScrollLines(t *testing.T) {
	cases := map[int]int32{0: 0, 120: -1, -120: 1, 240: -2, 30: -1, -30: 1, 1000000: -100}
	for d, want := range cases {
		if got := macScrollLines(d); got != want {
			t.Errorf("macScrollLines(%d)=%d 期望 %d", d, got, want)
		}
	}
}

func TestMacNextClickCount(t *testing.T) {
	now := time.Now()
	prev := macClickRecord{t: now, x: 10, y: 10, button: "left", count: 1}
	if macNextClickCount(prev, now.Add(100*time.Millisecond), 12, 11, "left") != 2 {
		t.Error("应为双击")
	}
	if macNextClickCount(prev, now.Add(time.Second), 10, 10, "left") != 1 {
		t.Error("超时应重置")
	}
	if macNextClickCount(prev, now.Add(100*time.Millisecond), 100, 10, "left") != 1 {
		t.Error("位移过大应重置")
	}
	if macNextClickCount(prev, now.Add(100*time.Millisecond), 10, 10, "right") != 1 {
		t.Error("换键应重置")
	}
	if macNextClickCount(macClickRecord{}, now, 0, 0, "left") != 1 {
		t.Error("首次为单击")
	}
}

func TestMacSplitText(t *testing.T) {
	segs := macSplitText("ab\r\nc\td")
	if len(segs) != 5 || segs[1].Key != macKeyReturn || segs[3].Key != macKeyTab ||
		string(utf16.Decode(segs[0].Units)) != "ab" || string(utf16.Decode(segs[2].Units)) != "c" {
		t.Errorf("分段错误: %+v", segs)
	}
	// 长文本分段不超 20 单元且不拆代理对，拼回等于原文
	long := strings.Repeat("你好😀a", 20)
	var sb strings.Builder
	for _, s := range macSplitText(long) {
		if s.Key >= 0 || len(s.Units) > macMaxUnicodeUnits || len(s.Units) == 0 {
			t.Fatalf("非法分段 %+v", s)
		}
		sb.WriteString(string(utf16.Decode(s.Units)))
	}
	if sb.String() != long {
		t.Error("拆分后拼接不等于原文（可能拆开了代理对）")
	}
	if len(macSplitText("")) != 0 {
		t.Error("空串应无分段")
	}
}

func TestMacDisplay(t *testing.T) {
	got := macDisplayOrder([]uint32{5, 7, 3}, 7)
	if !reflect.DeepEqual(got, []uint32{7, 5, 3}) {
		t.Errorf("主显示器应排第 0: %v", got)
	}
	if id, err := macPickDisplay(got, 1); err != nil || id != 5 {
		t.Errorf("pick: %d %v", id, err)
	}
	if _, err := macPickDisplay(got, 3); err == nil {
		t.Error("越界应报错")
	}
	if _, err := macPickDisplay(nil, 0); err == nil {
		t.Error("无显示器应报错")
	}
	r, ok := macRectFromCG(-1440, 0.4, 1440, 900)
	if !ok || r != image.Rect(-1440, 0, 0, 900) {
		t.Errorf("rect: %v %v", r, ok)
	}
	if _, ok := macRectFromCG(0, 0, 0, 0); ok {
		t.Error("零尺寸应失败")
	}
}

func TestMacErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want []string
	}{
		{macErrScreenRecording(), []string{"屏幕录制", "隐私与安全性", "macos_permission_screen_recording"}},
		{macErrAccessibility(), []string{"辅助功能", "隐私与安全性", "macos_permission_accessibility"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.err.Error(), w) {
				t.Errorf("错误文案缺少 %q: %v", w, c.err)
			}
		}
	}
}

// 在非 Mac 真机（如 CI）之外也应能构造后端；不调用系统 API。
func TestMacPlatformOps(t *testing.T) {
	o := platformDesktopOps()
	if o == nil || o.Capture == nil || o.Mouse == nil || o.Key == nil || o.Text == nil || o.Bounds == nil || o.Displays == nil {
		t.Fatal("后端函数不完整")
	}
}
