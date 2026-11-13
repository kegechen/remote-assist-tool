//go:build windows

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"testing"
	"unsafe"

)

var pGetCursorPos = user32.NewProc("GetCursorPos")

type point struct{ X, Y int32 }

func cursorPos(t *testing.T) (int, int) {
	t.Helper()
	var p point
	if r, _, e := pGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); r == 0 {
		t.Fatalf("GetCursorPos: %v", e)
	}
	return int(p.X), int(p.Y)
}

// 真实抓屏：要求在有交互桌面的会话中运行（锁屏/服务态会失败，此时跳过）。
func TestWindowsRealCapture(t *testing.T) {
	sc := NewScreenCapture(true)
	out, err := sc.Run(context.Background(), json.RawMessage(`{"max_width":800}`), nil)
	if err != nil {
		t.Skipf("当前会话无法抓屏（锁屏/非交互会话？）: %v", err)
	}
	var r ScreenCaptureResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	if r.ScreenWidth <= 0 || r.ScreenHeight <= 0 || r.Width <= 0 || r.Width > 800 {
		t.Fatalf("尺寸异常: %+v", r)
	}
	if _, err := jpeg.Decode(bytes.NewReader(r.Data)); err != nil {
		t.Fatalf("JPEG 非法: %v", err)
	}
	t.Logf("抓屏 OK: 屏幕 %dx%d → 返回 %dx%d, %d 字节, 显示器数 %d", r.ScreenWidth, r.ScreenHeight, r.Width, r.Height, len(r.Data), r.Displays)
}

// 真实注入：把光标移到主屏 (0.25,0.5) 处，核对系统光标位置，随后恢复原位。
func TestWindowsRealMouseMove(t *testing.T) {
	ox, oy := cursorPos(t)
	defer winMouse("move", ox, oy, "", 0)

	rect, err := monitorRect(0)
	if err != nil {
		t.Fatal(err)
	}
	in := NewInput(true)
	if _, err := in.Run(context.Background(), json.RawMessage(`{"events":[{"type":"move","x":0.25,"y":0.5}]}`), nil); err != nil {
		t.Fatal(err)
	}
	gx, gy := cursorPos(t)
	wx, wy := toPixel(rect, 0.25, 0.5)
	if gx != wx || gy != wy {
		t.Fatalf("光标位置 (%d,%d)，期望 (%d,%d)", gx, gy, wx, wy)
	}
}

func TestWindowsResolveVK(t *testing.T) {
	cases := map[string]uint16{"Enter": 0x0D, "ArrowLeft": 0x25, "F5": 0x74, "F12": 0x7B, "Escape": 0x1B, "a": 'A', "A": 'A', "5": '5'}
	for k, want := range cases {
		if got, ok := resolveVK(k); !ok || got != want {
			t.Errorf("resolveVK(%q)=%#x,%v 期望 %#x", k, got, ok, want)
		}
	}
	if _, ok := resolveVK("NoSuchKey"); ok {
		t.Error("未知键名应返回 false")
	}
}

// 回归：listMonitors 曾每次调用都 syscall.NewCallback，Go 限制进程内回调约 2000 个，
// 5fps 抓屏+输入几分钟后就会 fatal 崩溃（被控端进程直接退出）。反复枚举远超上限次数必须不崩。
func TestWindowsListMonitorsManyCallsNoCallbackLeak(t *testing.T) {
	for i := 0; i < 5000; i++ {
		if _, err := listMonitors(); err != nil {
			t.Fatalf("第 %d 次枚举失败: %v", i, err)
		}
	}
}
