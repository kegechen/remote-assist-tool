package tools

import (
	"image"
	"math/rand"
	"os"
	"testing"
	"time"
)

// 纯 CPU 基准（不碰屏幕）：首帧服务端的计算开销，整帧哈希 vs 块哈希，及 q70/q85 的 JPEG 编码。
func TestFirstFrameCPUCost(t *testing.T) {
	if os.Getenv("RD_BENCH") == "" {
		t.Skip("基准：设置 RD_BENCH=1 才运行（只打印耗时，不做断言）")
	}
	r := rand.New(rand.NewSource(3))
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	// 近似桌面：大面积平色 + 一些噪点文字区域
	for i := range img.Pix {
		img.Pix[i] = 240
	}
	for k := 0; k < 400000; k++ {
		img.Pix[r.Intn(len(img.Pix))] = uint8(r.Intn(256))
	}
	timeit := func(name string, n int, f func()) {
		f()
		s := time.Now()
		for i := 0; i < n; i++ {
			f()
		}
		t.Logf("%-28s %v", name, time.Since(s)/time.Duration(n))
	}
	timeit("整帧哈希 frameHash", 20, func() { frameHash(img) })
	timeit("块哈希 computeTiles", 20, func() { computeTiles(img) })
	small := scaleToWidth(img, 1280)
	timeit("缩放 1920→1280", 10, func() { scaleToWidth(img, 1280) })
	var n70, n85, n95 int
	timeit("JPEG q70", 10, func() { d, _ := encodeJPEGCapped(small, 70, desktopMaxJPEGBytes); n70 = len(d) })
	timeit("JPEG q85", 10, func() { d, _ := encodeJPEGCapped(small, 85, desktopMaxJPEGBytes); n85 = len(d) })
	timeit("JPEG q95", 10, func() { d, _ := encodeJPEGCapped(small, 95, desktopMaxJPEGBytes); n95 = len(d) })
	t.Logf("体积 q70=%dKB q85=%dKB q95=%dKB", n70/1024, n85/1024, n95/1024)
}
