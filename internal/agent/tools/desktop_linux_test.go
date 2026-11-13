//go:build linux

package tools

import (
	"os"
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
	b.spareKCs = []xproto.Keycode{200}
	if err := b.restoreSpare(); err != nil {
		t.Errorf("无连接时 restoreSpare 应安静返回: %v", err)
	}
}

func TestClampInt16(t *testing.T) {
	if clampInt16(40000) != 32767 || clampInt16(-40000) != -32768 || clampInt16(123) != 123 {
		t.Error("clampInt16 错误")
	}
}

// 一段文本里需要借用键码的字符，必须分配到各不相同的空闲键码：对同一个键码反复改绑，
// Qt/GTK 应用在高负载下可能还没刷新缓存的映射就收到下一个字（实测会丢字）。
func TestPlanSpareAssignments(t *testing.T) {
	// min=8，每键码 4 列；kc8=a/A，kc9=b/B，kc10~12 为空（可借）
	ks := []xproto.Keysym{
		'a', 'A', 0, 0,
		'b', 'B', 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
	}
	text := "a远程b远" // a、b 映射里已有；远、程需要借；“远”出现两次应复用同一键码
	plan, err := planSpare(ks, 4, 8, []rune(text), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("应只为 远/程 两个字符借键码，得到 %d: %v", len(plan), plan)
	}
	kcOf := map[rune]xproto.Keycode{}
	seen := map[xproto.Keycode]bool{}
	for _, a := range plan {
		if seen[a.kc] {
			t.Fatalf("不同字符不应共用键码 %d: %v", a.kc, plan)
		}
		seen[a.kc] = true
		kcOf[a.r] = a.kc
		if a.kc < 10 {
			t.Fatalf("借用了非空闲键码 %d（真实按键 a/b 不能动）", a.kc)
		}
	}
	if _, ok := kcOf['远']; !ok {
		t.Fatal("远 应被分配")
	}
	if _, ok := kcOf['程']; !ok {
		t.Fatal("程 应被分配")
	}
}

func TestPlanSpareNoFreeKeycode(t *testing.T) {
	ks := []xproto.Keysym{'a', 'A', 0, 0, 'b', 'B', 0, 0} // 没有空闲键码
	if _, err := planSpare(ks, 4, 8, []rune("远"), 0); err == nil {
		t.Fatal("没有空闲键码时应报错而不是破坏真实按键")
	}
	// 文本里全是映射里已有的字符：不需要借，不应报错
	if plan, err := planSpare(ks, 4, 8, []rune("ab"), 0); err != nil || len(plan) != 0 {
		t.Fatalf("无需借键码时应返回空计划: %v %v", plan, err)
	}
}

func TestX11DisplayName(t *testing.T) {
	t.Setenv("DISPLAY", "")
	if got := x11DisplayName(); got != ":0" {
		t.Errorf("DISPLAY 为空时应兜底 :0，得到 %q", got)
	}
	t.Setenv("DISPLAY", ":7")
	if got := x11DisplayName(); got != ":7" {
		t.Errorf("应沿用环境变量，得到 %q", got)
	}
}

// ensureConn 不得修改进程环境，否则之后启动的子进程（pty 里的 shell、exec 命令）会继承被改写的 DISPLAY。
func TestEnsureConnDoesNotSetenv(t *testing.T) {
	t.Setenv("DISPLAY", "")
	b := &x11Backend{}
	_ = b.ensureConn() // 有无 X 服务器都行，只关心环境
	defer b.drop()
	if v, ok := os.LookupEnv("DISPLAY"); ok && v != "" {
		t.Errorf("ensureConn 污染了进程环境 DISPLAY=%q", v)
	}
}
