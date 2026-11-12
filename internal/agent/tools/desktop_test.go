package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// fakeOps 记录所有注入调用，Capture 返回纯色图。
type fakeOps struct {
	calls []string
	rect  image.Rectangle
	fail  string // 非空时对应 kind 的调用返回错误
}

func (f *fakeOps) ops() *DesktopOps {
	return &DesktopOps{
		Displays: func() (int, error) { return 2, nil },
		Capture: func(d int) (*image.RGBA, image.Rectangle, error) {
			img := image.NewRGBA(image.Rect(0, 0, f.rect.Dx(), f.rect.Dy()))
			for i := 0; i < len(img.Pix); i += 4 {
				img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 200, 30, 30, 255
			}
			return img, f.rect, nil
		},
		Bounds: func(d int) (image.Rectangle, error) {
			if d > 1 {
				return image.Rectangle{}, errors.New("no such display")
			}
			return f.rect, nil
		},
		Mouse: func(kind string, x, y int, b string, delta int) error {
			if f.fail == kind {
				return errors.New("boom")
			}
			f.calls = append(f.calls, fmt.Sprintf("mouse:%s:%d,%d:%s:%d", kind, x, y, b, delta))
			return nil
		},
		Key: func(k string, up bool) error {
			f.calls = append(f.calls, fmt.Sprintf("key:%s:%v", k, up))
			return nil
		},
		Text: func(s string) error { f.calls = append(f.calls, "text:"+s); return nil },
	}
}

func newInputTool(f *fakeOps, enabled bool) *InputTool {
	return &InputTool{enabled: enabled, ops: f.ops()}
}

func runInput(t *testing.T, tool *InputTool, args string) (InputResult, error) {
	t.Helper()
	out, err := tool.Run(context.Background(), json.RawMessage(args), nil)
	if err != nil {
		return InputResult{}, err
	}
	var r InputResult
	if e := json.Unmarshal(out, &r); e != nil {
		t.Fatal(e)
	}
	return r, nil
}

func TestDesktopDisabled(t *testing.T) {
	f := &fakeOps{rect: image.Rect(0, 0, 100, 100)}
	if _, err := newInputTool(f, false).Run(context.Background(), json.RawMessage(`{"events":[{"type":"move"}]}`), nil); !errors.Is(err, ErrDesktopDisabled) {
		t.Fatalf("input 未启用应返回 ErrDesktopDisabled，得到 %v", err)
	}
	sc := &ScreenCaptureTool{enabled: false, ops: f.ops()}
	if _, err := sc.Run(context.Background(), nil, nil); !errors.Is(err, ErrDesktopDisabled) {
		t.Fatalf("screen_capture 未启用应返回 ErrDesktopDisabled，得到 %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("禁用时不应有任何注入: %v", f.calls)
	}
}

func TestDesktopUnsupportedWhenNoOps(t *testing.T) {
	// 惰性后端默认会取真实平台实现，这里强制为“无后端”，避免测试真的去注入输入。
	sharedOpsOnce = sync.Once{}
	sharedOpsOnce.Do(func() {})
	sharedOps = nil
	t.Cleanup(func() { sharedOpsOnce = sync.Once{} })
	if _, err := (&InputTool{enabled: true}).Run(context.Background(), json.RawMessage(`{"events":[{"type":"text","text":"a"}]}`), nil); !errors.Is(err, ErrDesktopUnsupported) {
		t.Fatalf("got %v", err)
	}
	if _, err := (&ScreenCaptureTool{enabled: true}).Run(context.Background(), nil, nil); !errors.Is(err, ErrDesktopUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestInputCoordinateMapping(t *testing.T) {
	// 副屏：虚拟桌面起点 (1920,0)，1000x500
	f := &fakeOps{rect: image.Rect(1920, 0, 2920, 500)}
	r, err := runInput(t, newInputTool(f, true), `{"events":[{"type":"click","x":0.5,"y":0.5},{"type":"move","x":1,"y":1},{"type":"move","x":0,"y":0}]}`)
	if err != nil || r.Applied != 3 {
		t.Fatalf("applied=%d err=%v", r.Applied, err)
	}
	want := []string{
		"mouse:move:2420,250::0", "mouse:down:2420,250:left:0", "mouse:up:2420,250:left:0",
		"mouse:move:2919,499::0", // 右下角夹取到最后一个像素，不越界
		"mouse:move:1920,0::0",
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("调用序列不符:\n got %v\nwant %v", f.calls, want)
	}
}

func TestInputEventKinds(t *testing.T) {
	f := &fakeOps{rect: image.Rect(0, 0, 100, 100)}
	_, err := runInput(t, newInputTool(f, true), `{"events":[
		{"type":"dblclick","x":0.1,"y":0.1,"button":"right"},
		{"type":"wheel","x":0.5,"y":0.5,"dy":120},
		{"type":"key_down","key":"Control"},{"type":"key_down","key":"c"},{"type":"key_up","key":"c"},{"type":"key_up","key":"Control"},
		{"type":"text","text":"你好"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, "|")
	for _, w := range []string{
		"mouse:down:10,10:right:0|mouse:up:10,10:right:0|mouse:down:10,10:right:0", // 双击 = 两次按下抬起
		"mouse:wheel:50,50::120", "key:Control:false", "key:c:true", "text:你好",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("缺少 %q\n实际: %s", w, got)
		}
	}
}

func TestInputValidation(t *testing.T) {
	f := &fakeOps{rect: image.Rect(0, 0, 100, 100)}
	tool := newInputTool(f, true)
	big := `{"events":[` + strings.Repeat(`{"type":"key_down","key":"a"},`, desktopMaxEvents) + `{"type":"key_up","key":"a"}]}`
	cases := map[string]string{
		"空事件":    `{"events":[]}`,
		"非法JSON": `{`,
		"未知类型":   `{"events":[{"type":"explode"}]}`,
		"越界坐标":   `{"events":[{"type":"move","x":1.5,"y":0}]}`,
		"负坐标":    `{"events":[{"type":"click","x":-0.1,"y":0}]}`,
		"未知按钮":   `{"events":[{"type":"click","x":0,"y":0,"button":"side"}]}`,
		"缺key":   `{"events":[{"type":"key_down"}]}`,
		"缺text":  `{"events":[{"type":"text"}]}`,
		"超长文本":   `{"events":[{"type":"text","text":"` + strings.Repeat("a", desktopMaxTextLen+1) + `"}]}`,
		"事件过多":   big,
		"负显示器":   `{"display":-1,"events":[{"type":"move"}]}`,
		"显示器不存在": `{"display":5,"events":[{"type":"move","x":0,"y":0}]}`,
	}
	for name, args := range cases {
		if _, err := runInput(t, tool, args); err == nil {
			t.Errorf("%s: 应返回错误", name)
		}
	}
	// 校验失败必须发生在任何注入之前：后面事件非法时，前面的合法事件也不能执行
	if _, err := runInput(t, tool, `{"events":[{"type":"text","text":"x"},{"type":"explode"}]}`); err == nil {
		t.Fatal("应整体拒绝")
	}
	if len(f.calls) != 0 {
		t.Fatalf("校验失败后不应有注入: %v", f.calls)
	}
}

func TestInputPartialFailureReported(t *testing.T) {
	f := &fakeOps{rect: image.Rect(0, 0, 100, 100), fail: "down"}
	_, err := runInput(t, newInputTool(f, true), `{"events":[{"type":"key_down","key":"a"},{"type":"click","x":0.5,"y":0.5}]}`)
	if err == nil || !strings.Contains(err.Error(), "after 1 applied") {
		t.Fatalf("应报告已执行 1 个后失败，得到 %v", err)
	}
}

func TestScreenCaptureScaleAndJPEG(t *testing.T) {
	f := &fakeOps{rect: image.Rect(0, 0, 2560, 1440)}
	sc := &ScreenCaptureTool{enabled: true, ops: f.ops()}
	out, err := sc.Run(context.Background(), json.RawMessage(`{"max_width":640,"quality":50}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var r ScreenCaptureResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	if r.Width != 640 || r.Height != 360 || r.ScreenWidth != 2560 || r.ScreenHeight != 1440 || r.Displays != 2 || r.Format != "jpeg" {
		t.Fatalf("结果字段不符: %+v", r)
	}
	img, err := jpeg.Decode(bytes.NewReader(r.Data))
	if err != nil {
		t.Fatalf("返回数据不是合法 JPEG: %v", err)
	}
	if img.Bounds().Dx() != 640 || img.Bounds().Dy() != 360 {
		t.Fatalf("JPEG 尺寸 %v", img.Bounds())
	}
	cr, cg, cb, _ := img.At(320, 180).RGBA()
	if cr>>8 < 180 || cg>>8 > 60 || cb>>8 > 60 { // 原图 (200,30,30)，容许 JPEG 误差
		t.Fatalf("颜色失真: %d %d %d", cr>>8, cg>>8, cb>>8)
	}
}

func TestScreenCaptureNoUpscaleAndArgs(t *testing.T) {
	f := &fakeOps{rect: image.Rect(0, 0, 800, 600)}
	sc := &ScreenCaptureTool{enabled: true, ops: f.ops()}
	out, err := sc.Run(context.Background(), nil, nil) // 默认参数，且允许空 args
	if err != nil {
		t.Fatal(err)
	}
	var r ScreenCaptureResult
	json.Unmarshal(out, &r)
	if r.Width != 800 || r.Height != 600 {
		t.Fatalf("小于 max_width 时不应缩放: %dx%d", r.Width, r.Height)
	}
	for _, bad := range []string{`{"quality":101}`, `{"quality":-1}`, `{"display":-2}`, `{`} {
		if _, err := sc.Run(context.Background(), json.RawMessage(bad), nil); err == nil {
			t.Errorf("%s 应报错", bad)
		}
	}
}

func TestEncodeJPEGCapped(t *testing.T) {
	// 随机噪声难压缩：高质量必超限，应自动降质量；极小 limit 则报错
	img := image.NewRGBA(image.Rect(0, 0, 400, 400))
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Set((i/4)%400, (i/4)/400, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
	}
	hi, _ := encodeJPEGCapped(img, 95, 1<<30)
	capped, err := encodeJPEGCapped(img, 95, len(hi)/2)
	if err != nil {
		t.Fatalf("降质量后应能满足: %v", err)
	}
	if len(capped) > len(hi)/2 {
		t.Fatalf("未满足上限: %d > %d", len(capped), len(hi)/2)
	}
	if _, err := encodeJPEGCapped(img, 95, 100); err == nil {
		t.Fatal("limit 过小应报错")
	}
}

func TestScaleToWidthAverages(t *testing.T) {
	// 2x2 黑白棋盘缩成 1x1，面积平均应为中灰
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	set := func(x, y int, v uint8) { o := y*src.Stride + x*4; src.Pix[o], src.Pix[o+1], src.Pix[o+2], src.Pix[o+3] = v, v, v, 255 }
	set(0, 0, 0)
	set(1, 0, 255)
	set(0, 1, 255)
	set(1, 1, 0)
	dst := scaleToWidth(src, 1)
	if dst.Bounds().Dx() != 1 || dst.Pix[0] < 120 || dst.Pix[0] > 135 {
		t.Fatalf("面积平均错误: %v %v", dst.Bounds(), dst.Pix[:4])
	}
}

// 未启用时构造工具不得创建后端（不调 platformDesktopOps、不改 DPI）。
func TestDesktopToolsLazyBackend(t *testing.T) {
	sc := NewScreenCapture(false)
	in := NewInput(true)
	if sc.ops != nil || in.ops != nil {
		t.Fatal("构造时不应创建后端")
	}
	if _, err := sc.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "desktop_disabled") {
		t.Fatalf("want desktop_disabled, got %v", err)
	}
}
