//go:build linux

package tools

import (
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestKeysymForKey(t *testing.T) {
	cases := map[string]xproto.Keysym{
		"Enter": 0xff0d, "Backspace": 0xff08, "ArrowLeft": 0xff51, "Escape": 0xff1b, "Control": 0xffe3,
		"F1": 0xffbe, "F5": 0xffc2, "F12": 0xffc9, "a": 'a', "A": 'A', "5": '5', " ": 0x20, "Delete": 0xffff,
		"é": 0xe9, "中": 0x01000000 + 0x4e2d,
	}
	for k, want := range cases {
		if got, ok := keysymForKey(k); !ok || got != want {
			t.Errorf("keysymForKey(%q)=%#x,%v 期望 %#x", k, got, ok, want)
		}
	}
	if _, ok := keysymForKey("NoSuchKey"); ok {
		t.Error("未知键名应返回 false")
	}
	if _, ok := keysymForKey("F99"); ok {
		t.Error("F99 不应识别")
	}
}

func TestKeysymForRune(t *testing.T) {
	cases := map[rune]xproto.Keysym{'a': 'a', '~': '~', '\n': 0xff0d, '\t': 0xff09, 'ü': 0xfc, '你': 0x01000000 + 0x4f60, '😀': 0x01000000 + 0x1f600}
	for r, want := range cases {
		if got := keysymForRune(r); got != want {
			t.Errorf("keysymForRune(%q)=%#x 期望 %#x", r, got, want)
		}
	}
}

func TestFindKeycode(t *testing.T) {
	// min=8，每键码 4 列：kc8 = a/A，kc9 = 1/!，kc10 空（spare），kc11 = b/B
	ks := []xproto.Keysym{'a', 'A', 0, 0, '1', '!', 0, 0, 0, 0, 0, 0, 'b', 'B', 0, 0}
	if kc, col, ok := findKeycode(ks, 4, 8, 'a', 0); !ok || kc != 8 || col != 0 {
		t.Errorf("a: %d %d %v", kc, col, ok)
	}
	if kc, col, ok := findKeycode(ks, 4, 8, 'B', 0); !ok || kc != 11 || col != 1 {
		t.Errorf("B: %d %d %v", kc, col, ok)
	}
	if _, _, ok := findKeycode(ks, 4, 8, 'z', 0); ok {
		t.Error("不存在的键不应找到")
	}
	if _, _, ok := findKeycode(ks, 4, 8, 'a', 8); ok { // 跳过被借用的键码
		t.Error("skip 的键码不应被返回")
	}
	if _, _, ok := findKeycode(nil, 0, 8, 'a', 0); ok {
		t.Error("空映射不应找到")
	}
}

func TestButtonCode(t *testing.T) {
	if buttonCode("left") != 1 || buttonCode("middle") != 2 || buttonCode("right") != 3 || buttonCode("") != 1 {
		t.Error("按钮映射错误")
	}
}

// 回归：连接断开(conn==nil)后 fake/tap/restoreSpare 不能 panic。
func TestX11NoConnDoesNotPanic(t *testing.T) {
	b := &x11Backend{minKC: 8, maxKC: 255}
	if err := b.fake(xKeyPress, 38, 0, 0); err == nil {
		t.Error("无连接时 fake 应返回错误")
	}
	if err := b.tap(38, true, []xproto.Keysym{'a', 'A'}, 2); err == nil {
		t.Error("无连接时 tap 应返回错误而不是 panic")
	}
	b.dirtySpare = true
	if err := b.restoreSpare(); err != nil {
		t.Errorf("无连接时 restoreSpare 应安静返回: %v", err)
	}
}

func TestClampInt16(t *testing.T) {
	if clampInt16(40000) != 32767 || clampInt16(-40000) != -32768 || clampInt16(123) != 123 {
		t.Error("clampInt16 错误")
	}
}
