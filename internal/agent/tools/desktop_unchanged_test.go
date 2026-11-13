package tools

import (
	"context"
	"encoding/json"
	"image"
	"strings"
	"sync/atomic"
	"testing"
)

func noiseImage(w, h int, seed byte) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i*7+int(seed)) ^ byte(i>>8)
	}
	return img
}

func TestFrameHashStableAndSensitive(t *testing.T) {
	a, b := noiseImage(640, 360, 1), noiseImage(640, 360, 1)
	if frameHash(a) != frameHash(b) {
		t.Fatal("相同内容哈希应一致")
	}
	if frameHash(a) != frameHash(a) {
		t.Fatal("重复哈希应稳定")
	}
	base := frameHash(a)
	// 任意位置单字节变化（含第一个、最后一个、行尾、中间）都要被捕获——鼠标指针只占几个像素。
	for _, off := range []int{0, 1, 4*640 - 1, 4 * 640, len(a.Pix)/2 + 3, len(a.Pix) - 1} {
		c := noiseImage(640, 360, 1)
		c.Pix[off] ^= 0x01
		if frameHash(c) == base {
			t.Errorf("偏移 %d 的单比特变化未被检测到", off)
		}
	}
	if frameHash(noiseImage(640, 361, 1)) == base {
		t.Error("尺寸不同的图不应同哈希")
	}
}

func TestFrameHashSubImageStride(t *testing.T) {
	// stride 大于行宽（子图）时只哈希可见像素，不受行尾填充影响
	big := noiseImage(200, 100, 3)
	sub := big.SubImage(image.Rect(10, 10, 110, 60)).(*image.RGBA)
	h1 := frameHash(sub)
	big.Pix[0] ^= 0xFF // 子图之外
	if frameHash(sub) != h1 {
		t.Fatal("子图外的改动不应影响子图哈希")
	}
	big.Pix[sub.PixOffset(20, 20)] ^= 0xFF
	if frameHash(sub) == h1 {
		t.Fatal("子图内的改动应改变哈希")
	}
}

func TestFrameHashParallelMatchesSerial(t *testing.T) {
	// 行独立哈希再顺序合并，结果与并行度无关
	img := noiseImage(1920, 1080, 9)
	want := frameHash(img)
	for i := 0; i < 5; i++ {
		if frameHash(img) != want {
			t.Fatal("并行哈希结果不稳定")
		}
	}
}

// 可控画面的假后端：frame 用于在两次 Run 之间改变画面。
func newSeqTool(frame *atomic.Int32) *ScreenCaptureTool {
	ops := (&fakeOps{rect: image.Rect(0, 0, 320, 200)}).ops()
	ops.Capture = func(d int) (*image.RGBA, image.Rectangle, error) {
		return noiseImage(320, 200, byte(frame.Load())), image.Rect(0, 0, 320, 200), nil
	}
	return &ScreenCaptureTool{enabled: true, ops: ops}
}

func runCap(t *testing.T, tool *ScreenCaptureTool, args string) (ScreenCaptureResult, int) {
	t.Helper()
	out, err := tool.Run(context.Background(), json.RawMessage(args), nil)
	if err != nil {
		t.Fatal(err)
	}
	var r ScreenCaptureResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	return r, len(out)
}

func TestScreenCaptureUnchanged(t *testing.T) {
	var fr atomic.Int32
	tool := newSeqTool(&fr)
	r1, n1 := runCap(t, tool, `{}`)
	if r1.Unchanged || len(r1.Data) == 0 || r1.Seq == 0 {
		t.Fatalf("首帧必须是完整帧且带 seq: %+v", r1)
	}
	// 不带 since（旧前端）：即使画面没变也返回完整帧
	if r, _ := runCap(t, tool, `{}`); r.Unchanged || len(r.Data) == 0 || r.Seq != r1.Seq {
		t.Fatalf("不传 since 应保持旧行为: %+v", r)
	}
	// 带正确 since 且画面未变：极小的 unchanged 包，无 data，但保留屏幕尺寸等元数据
	r2, n2 := runCap(t, tool, `{"since":`+u64s(r1.Seq)+`}`)
	if !r2.Unchanged || len(r2.Data) != 0 || r2.Seq != r1.Seq || r2.ScreenWidth != 320 || r2.Displays != 2 {
		t.Fatalf("应返回 unchanged: %+v", r2)
	}
	if n2*10 > n1 {
		t.Fatalf("unchanged 包应远小于完整帧: %d vs %d", n2, n1)
	}
	// 画面变化：完整帧 + 新 seq
	fr.Store(5)
	r3, _ := runCap(t, tool, `{"since":`+u64s(r1.Seq)+`}`)
	if r3.Unchanged || len(r3.Data) == 0 || r3.Seq == r1.Seq {
		t.Fatalf("画面变化应回完整帧与新 seq: %+v", r3)
	}
	// 用新 seq 再请求：又未变化
	if r, _ := runCap(t, tool, `{"since":`+u64s(r3.Seq)+`}`); !r.Unchanged {
		t.Fatalf("新 seq 下应 unchanged: %+v", r)
	}
}

func TestScreenCaptureSinceMismatchReturnsFull(t *testing.T) {
	var fr atomic.Int32
	tool := newSeqTool(&fr)
	r1, _ := runCap(t, tool, `{}`)
	for _, since := range []uint64{1, r1.Seq - 1, r1.Seq + 1, r1.Seq + 100000} {
		if r, _ := runCap(t, tool, `{"since":`+u64s(since)+`}`); r.Unchanged || len(r.Data) == 0 {
			t.Errorf("since=%d 与当前 seq=%d 不匹配，应回完整帧", since, r1.Seq)
		}
	}
}

func TestScreenCaptureParamChangeReturnsFull(t *testing.T) {
	var fr atomic.Int32
	tool := newSeqTool(&fr)
	r1, _ := runCap(t, tool, `{"max_width":320,"quality":60}`)
	// 画面没变，但宽度/质量变了：对端旧帧不能代替新参数的帧
	for _, args := range []string{`{"max_width":200,"quality":60,"since":%d}`, `{"max_width":320,"quality":30,"since":%d}`} {
		r, _ := runCap(t, tool, strings.Replace(args, "%d", u64s(r1.Seq), 1))
		if r.Unchanged || len(r.Data) == 0 {
			t.Errorf("参数变化应回完整帧: %s", args)
		}
	}
}

func TestScreenCaptureSeqPerDisplayAndRestart(t *testing.T) {
	var fr atomic.Int32
	tool := newSeqTool(&fr)
	a, _ := runCap(t, tool, `{"display":0}`)
	b, _ := runCap(t, tool, `{"display":1}`)
	if a.Seq == b.Seq {
		t.Fatal("不同显示器的 seq 应不同")
	}
	// 用显示器 0 的 seq 请求显示器 1：不应命中
	if r, _ := runCap(t, tool, `{"display":1,"since":`+u64s(a.Seq)+`}`); r.Unchanged {
		t.Fatal("跨显示器的 since 不应命中")
	}
	// “重启”：新工具实例的 seq 不会等于旧实例的 seq，旧 since 不会误判未变化
	tool2 := newSeqTool(&fr)
	if r, _ := runCap(t, tool2, `{"display":0,"since":`+u64s(a.Seq)+`}`); r.Unchanged {
		t.Fatal("新实例不应认旧 seq")
	}
}

// 契约兼容：旧前端只读 format/width/height/screen_*/displays/data，新增字段不影响；
// 新前端遇到旧被控端（无 seq/unchanged/proc_ms）按完整帧处理。
func TestScreenCaptureContractCompat(t *testing.T) {
	var fr atomic.Int32
	tool := newSeqTool(&fr)
	out, err := tool.Run(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"format", "width", "height", "screen_width", "screen_height", "displays", "data", "seq", "proc_ms"} {
		if _, ok := m[k]; !ok {
			t.Errorf("完整帧缺少字段 %s", k)
		}
	}
	if _, ok := m["unchanged"]; ok {
		t.Error("完整帧不应带 unchanged")
	}
	// 旧结构体（无新字段）能解析新响应
	var old struct {
		Format string `json:"format"`
		Width  int    `json:"width"`
		Data   []byte `json:"data"`
	}
	if err := json.Unmarshal(out, &old); err != nil || old.Format != "jpeg" || len(old.Data) == 0 {
		t.Fatalf("旧调用方解析失败: %v %+v", err, old)
	}
}

func u64s(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func BenchmarkFrameHash1080p(b *testing.B) {
	img := noiseImage(1920, 1080, 1)
	b.SetBytes(int64(len(img.Pix)))
	for i := 0; i < b.N; i++ {
		frameHash(img)
	}
}

// swizzleToRGBA 与逐像素参考实现一致（含 stride 大于行宽、多种分量顺序、行数不能被并行块整除）。
func TestSwizzleToRGBAMatchesReference(t *testing.T) {
	const w, h, stride = 37, 203, 37*4 + 12
	src := make([]byte, h*stride)
	for i := range src {
		src[i] = byte(i*31 + i>>7)
	}
	for _, o := range [][3]int{{2, 1, 0}, {0, 1, 2}, {1, 2, 3}, {3, 2, 1}} {
		got := image.NewRGBA(image.Rect(0, 0, w, h))
		swizzleToRGBA(got, src, stride, o[0], o[1], o[2])
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				s := src[y*stride+x*4:]
				d := got.Pix[y*got.Stride+x*4:]
				if d[0] != s[o[0]] || d[1] != s[o[1]] || d[2] != s[o[2]] || d[3] != 255 {
					t.Fatalf("order %v (%d,%d): got %v want [%d %d %d 255]", o, x, y, d[:4], s[o[0]], s[o[1]], s[o[2]])
				}
			}
		}
	}
}

func BenchmarkSwizzle1080p(b *testing.B) {
	src := make([]byte, 1920*1080*4)
	dst := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	b.SetBytes(int64(len(src)))
	for i := 0; i < b.N; i++ {
		swizzleToRGBA(dst, src, 1920*4, 2, 1, 0)
	}
}

// 改造前的逐像素串行实现，仅作对比基线。
func BenchmarkSwizzleSerial1080p(b *testing.B) {
	src := make([]byte, 1920*1080*4)
	dst := image.NewRGBA(image.Rect(0, 0, 1920, 1080)).Pix
	b.SetBytes(int64(len(src)))
	for i := 0; i < b.N; i++ {
		for j := 0; j+3 < len(src); j += 4 {
			dst[j], dst[j+1], dst[j+2], dst[j+3] = src[j+2], src[j+1], src[j], 255
		}
	}
}
