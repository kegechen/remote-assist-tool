//go:build windows

package tools

import (
	"image"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestDXOutputMatches(t *testing.T) {
	od := &outputDesc{coords: windows.Rect{Left: 0, Top: 0, Right: 1920, Bottom: 1080}, attached: 1}
	if !dxOutputMatches(od, image.Rect(0, 0, 1920, 1080)) {
		t.Fatal("应匹配")
	}
	if dxOutputMatches(od, image.Rect(0, 0, 1920, 1081)) {
		t.Fatal("尺寸不同不应匹配")
	}
	od.attached = 0
	if dxOutputMatches(od, image.Rect(0, 0, 1920, 1080)) {
		t.Fatal("未接入桌面不应匹配")
	}
}

func TestDXFailedHR(t *testing.T) {
	if !failed(uintptr(uint32(hrAccessLost))) || failed(0) || failed(1) {
		t.Fatal("HRESULT 判定错误")
	}
}

// 强制失败时必须回退 GDI 出图，并且重试间隔内不再尝试 DXGI。
func TestDXGIForcedFailFallsBackToGDI(t *testing.T) {
	if _, _, err := winCapture(0); err != nil {
		t.Skip(err)
	}
	oldF, oldI := dxgiForceFail, dxgiRetryInterval
	defer func() { dxgiForceFail, dxgiRetryInterval = oldF, oldI; dxgiReleaseAll() }()
	dxgiReleaseAll()
	dxgiForceFail, dxgiRetryInterval = true, time.Hour
	for i := 0; i < 3; i++ {
		img, _, err := winCaptureAuto(0)
		if err != nil || img == nil {
			t.Fatalf("回退失败: %v", err)
		}
	}
	dxgiMu.Lock()
	d := dxgiDisplays[0]
	retry := d != nil && d.lastErr != nil && time.Until(d.retryAt) > 59*time.Minute
	dxgiMu.Unlock()
	if !retry {
		t.Fatal("应记录失败并推迟重试")
	}
}

// 真机：DXGI 与 GDI 同一时刻逐像素对比（容差内），并统计耗时。
func TestDXGIVsGDI(t *testing.T) {
	if _, _, err := winCapture(0); err != nil {
		t.Skip(err)
	}
	dxgiReleaseAll()
	rect, _ := monitorRect(0)
	var d dxgiDisplay
	if err := d.init(rect); err != nil {
		t.Skipf("DXGI 不可用: %v", err)
	}
	defer d.close()
	var a, b *image.RGBA
	var err error
	if a, err = d.grab(); err != nil {
		t.Skipf("DXGI 首帧失败: %v", err)
	}
	b, _, _ = winCapture(0)
	if a.Bounds() != b.Bounds() {
		t.Fatalf("尺寸不一致 %v %v", a.Bounds(), b.Bounds())
	}
	diff := 0
	for i := 0; i < len(a.Pix); i += 4 {
		dr := int(a.Pix[i]) - int(b.Pix[i])
		dg := int(a.Pix[i+1]) - int(b.Pix[i+1])
		db := int(a.Pix[i+2]) - int(b.Pix[i+2])
		if dr > 8 || dr < -8 || dg > 8 || dg < -8 || db > 8 || db < -8 {
			diff++
		}
	}
	total := len(a.Pix) / 4
	t.Logf("差异像素 %d/%d (%.3f%%)", diff, total, float64(diff)*100/float64(total))
	if float64(diff)/float64(total) > 0.02 {
		t.Fatalf("差异像素占比过高")
	}
}

func pct(ds []time.Duration) (mean, p95 time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	var s time.Duration
	for _, x := range ds {
		s += x
	}
	return s / time.Duration(len(ds)), ds[len(ds)*95/100]
}

func TestDXGIBenchmark(t *testing.T) {
	if _, _, err := winCapture(0); err != nil {
		t.Skip(err)
	}
	dxgiReleaseAll()
	rect, _ := monitorRect(0)
	var d dxgiDisplay
	if err := d.init(rect); err != nil {
		t.Skipf("DXGI 不可用: %v", err)
	}
	defer d.close()
	const n = 100
	var g, x []time.Duration
	for i := 0; i < n; i++ {
		s := time.Now()
		winCapture(0)
		g = append(g, time.Since(s))
	}
	for i := 0; i < n; i++ {
		s := time.Now()
		d.last = nil // 强制取新帧计算含转换的真实耗时（屏幕静止时会超时，仅统计成功帧）
		if _, err := d.grab(); err == nil {
			x = append(x, time.Since(s))
		}
	}
	gm, gp := pct(g)
	t.Logf("GDI   均值 %v P95 %v", gm, gp)
	if len(x) > 0 {
		xm, xp := pct(x)
		t.Logf("DXGI  均值 %v P95 %v (成功 %d/%d 帧)", xm, xp, len(x), n)
		k := int64(dxgiStats.dxgiFrames.Load())
		if k > 0 {
			t.Logf("DXGI 分段均值（新帧 %d 个）: 等待新帧=%.1fms 拷贝+Map=%.1fms 转RGBA=%.1fms", k,
				float64(dxgiStats.acquireNs.Load())/float64(k)/1e6, float64(dxgiStats.copyNs.Load())/float64(k)/1e6,
				float64(dxgiStats.convNs.Load())/float64(k)/1e6)
		}
	}
}

// 反复开关：COM 对象数必须回到 0。
func TestDXGIOpenCloseNoLeak(t *testing.T) {
	if _, _, err := winCapture(0); err != nil {
		t.Skip(err)
	}
	dxgiReleaseAll()
	base := dxLive.Load()
	for i := 0; i < 100; i++ {
		if _, _, err := winCaptureAuto(0); err != nil {
			t.Fatal(err)
		}
		dxgiReleaseAll()
	}
	if got := dxLive.Load(); got != base {
		t.Fatalf("COM 对象泄漏: base=%d now=%d", base, got)
	}
}

// 进程句柄数：反复开关 100 次后不应增长（允许少量运行时波动）。
func TestDXGIOpenCloseHandleCount(t *testing.T) {
	if _, _, err := winCapture(0); err != nil {
		t.Skip(err)
	}
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	count := func() uint32 {
		var n uint32
		proc.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&n)))
		return n
	}
	winCaptureAuto(0)
	dxgiReleaseAll()
	runtime.GC()
	base := count()
	for i := 0; i < 100; i++ {
		if _, _, err := winCaptureAuto(0); err != nil {
			t.Fatal(err)
		}
		dxgiReleaseAll()
	}
	runtime.GC()
	now := count()
	t.Logf("句柄数 开关前=%d 100 次后=%d", base, now)
	if now > base+5 {
		t.Fatalf("句柄泄漏: %d -> %d", base, now)
	}
}

// 稳定性：DXGI_SOAK=1 时连续抓屏 2 分钟，不得回退 GDI、不得报错。
func TestDXGISoak(t *testing.T) {
	if os.Getenv("DXGI_SOAK") != "1" {
		t.Skip("设置 DXGI_SOAK=1 运行")
	}
	dxgiReleaseAll()
	before := dxgiStats.gdiFrames.Load()
	end := time.Now().Add(2 * time.Minute)
	n := 0
	for time.Now().Before(end) {
		if _, _, err := winCaptureAuto(0); err != nil {
			t.Fatal(err)
		}
		n++
		time.Sleep(30 * time.Millisecond)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("2 分钟 %d 帧: DXGI新帧=%d 缓存帧=%d GDI回退=%d 堆=%dMB COM存活=%d", n, dxgiStats.dxgiFrames.Load(),
		dxgiStats.cachedFrames.Load(), dxgiStats.gdiFrames.Load()-before, ms.HeapAlloc>>20, dxLive.Load())
	if dxgiStats.gdiFrames.Load() != before {
		t.Fatal("soak 期间发生回退")
	}
}
