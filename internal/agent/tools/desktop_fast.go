package tools

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 本文件放远程桌面每帧热路径的并行实现：缩放按行分块并行、JPEG 按条带并行编码后用 RST 重启标记拼接。
// 输出与单线程实现逐像素一致（缩放）或解码后逐像素一致（JPEG）。

// parallelRows 把 [0,n) 切成若干连续块，并行执行 fn(lo,hi)。n 很小时直接串行。
func parallelRows(n, minPerWorker int, fn func(lo, hi int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if m := n / minPerWorker; m < workers {
		workers = m
	}
	if workers <= 1 {
		fn(0, n)
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		lo, hi := n*i/workers, n*(i+1)/workers
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(lo, hi)
		}()
	}
	wg.Wait()
}

// scaleToWidth 用面积平均缩小到不超过 maxW 宽；不放大。面积平均比最近邻更利于文字可读。
// 行块并行；列边界预先算好，避免内层重复整数除法。
func scaleToWidth(src *image.RGBA, maxW int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= maxW || sw == 0 {
		return src
	}
	dw := maxW
	dh := sh * dw / sw
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xb := make([]int, dw+1) // 目标列 dx 对应源列 [xb[dx], xb[dx+1])
	for dx := 0; dx <= dw; dx++ {
		xb[dx] = dx * sw / dw
	}
	parallelRows(dh, 16, func(lo, hi int) {
		for dy := lo; dy < hi; dy++ {
			y0, y1 := dy*sh/dh, (dy+1)*sh/dh
			if y1 <= y0 {
				y1 = y0 + 1
			}
			o := dy * dst.Stride
			for dx := 0; dx < dw; dx++ {
				x0, x1 := xb[dx], xb[dx+1]
				if x1 <= x0 {
					x1 = x0 + 1
				}
				var r, g, b uint32
				for y := y0; y < y1; y++ {
					p := src.Pix[y*src.Stride+x0*4 : y*src.Stride+x1*4]
					for i := 0; i+3 < len(p); i += 4 {
						r += uint32(p[i])
						g += uint32(p[i+1])
						b += uint32(p[i+2])
					}
				}
				cnt := uint32((y1 - y0) * (x1 - x0))
				dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/cnt), uint8(g/cnt), uint8(b/cnt), 255
				o += 4
			}
		}
	})
	return dst
}

// 条带并行 JPEG：每条带高度为 16 的倍数（4:2:0 的 MCU 高度），各自独立编码；
// 每个条带的熵编码数据以 DC 预测值清零开始、字节对齐结束，恰好等价于一个重启区间，
// 因此把条带数据用 RSTn 标记串起来，再在头部加 DRI，就是一个合法的单张基线 JPEG。
const jpegStripMinHeight = 64

func encodeJPEG(img *image.RGBA, quality int) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	strips := runtime.GOMAXPROCS(0)
	if strips > 8 {
		strips = 8
	}
	if m := h / jpegStripMinHeight; m < strips {
		strips = m
	}
	if strips <= 1 || w < 16 {
		return encodeJPEGSingle(img, quality)
	}
	// 重启区间必须等长，所以除最后一条外每条 MCU 行数相同。
	rows := (h + 15) / 16 // MCU 行数
	per := (rows + strips - 1) / strips
	strips = (rows + per - 1) / per
	bounds := make([]int, strips+1)
	for i := 0; i <= strips; i++ {
		bounds[i] = i * per * 16
		if bounds[i] > h {
			bounds[i] = h
		}
	}
	parts := make([][]byte, strips)
	errs := make([]error, strips)
	var wg sync.WaitGroup
	for i := 0; i < strips; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub := img.SubImage(image.Rect(b.Min.X, b.Min.Y+bounds[i], b.Max.X, b.Min.Y+bounds[i+1])).(*image.RGBA)
			parts[i], errs[i] = encodeJPEGSingle(sub, quality)
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	interval := ((w + 15) / 16) * ((bounds[1] - bounds[0]) / 16)
	return stitchJPEGStrips(parts, h, interval)
}

func encodeJPEGSingle(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("jpeg encode: %w", err)
	}
	return buf.Bytes(), nil
}

// splitJPEG 返回 SOS 段起点（含标记）、熵编码数据起点，以及数据终点（不含 EOI）。
func splitJPEG(p []byte) (sos, data, end int, err error) {
	if len(p) < 4 || p[0] != 0xFF || p[1] != 0xD8 {
		return 0, 0, 0, errors.New("jpeg: bad SOI")
	}
	i := 2
	for i+4 <= len(p) {
		if p[i] != 0xFF {
			return 0, 0, 0, errors.New("jpeg: bad marker")
		}
		m := p[i+1]
		l := int(p[i+2])<<8 | int(p[i+3])
		if m == 0xDA {
			if len(p) < 2 || p[len(p)-2] != 0xFF || p[len(p)-1] != 0xD9 {
				return 0, 0, 0, errors.New("jpeg: bad EOI")
			}
			return i, i + 2 + l, len(p) - 2, nil
		}
		i += 2 + l
	}
	return 0, 0, 0, errors.New("jpeg: no SOS")
}

func stitchJPEGStrips(parts [][]byte, totalH, interval int) ([]byte, error) {
	sos, data, end, err := splitJPEG(parts[0])
	if err != nil {
		return nil, err
	}
	head := append([]byte(nil), parts[0][:sos]...)
	// 回填 SOF0/SOF1 的高度
	patched := false
	for i := 2; i+9 <= len(head); {
		m := head[i+1]
		l := int(head[i+2])<<8 | int(head[i+3])
		if m == 0xC0 || m == 0xC1 {
			head[i+5], head[i+6] = byte(totalH>>8), byte(totalH)
			patched = true
			break
		}
		i += 2 + l
	}
	if !patched {
		return nil, errors.New("jpeg: no SOF")
	}
	size := len(head) + 6 + len(parts[0][sos:data]) + 2
	for _, p := range parts {
		size += len(p) + 2
	}
	out := make([]byte, 0, size)
	out = append(out, head...)
	out = append(out, 0xFF, 0xDD, 0x00, 0x04, byte(interval>>8), byte(interval))
	out = append(out, parts[0][sos:data]...)
	out = append(out, parts[0][data:end]...)
	for i := 1; i < len(parts); i++ {
		_, d, e, err := splitJPEG(parts[i])
		if err != nil {
			return nil, err
		}
		out = append(out, 0xFF, byte(0xD0+(i-1)%8))
		out = append(out, parts[i][d:e]...)
	}
	out = append(out, 0xFF, 0xD9)
	return out, nil
}

// 每帧分段计时：仅当环境变量 REMOTE_DESKTOP_TRACE=1 时输出到 stderr，用于定位瓶颈，默认零开销。
var desktopTraceOn = os.Getenv("REMOTE_DESKTOP_TRACE") == "1"

type desktopTrace struct {
	last  time.Time
	start time.Time
	segs  []string
}

func newDesktopTrace() *desktopTrace {
	if !desktopTraceOn {
		return nil
	}
	now := time.Now()
	return &desktopTrace{last: now, start: now}
}

func (t *desktopTrace) mark(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.segs = append(t.segs, fmt.Sprintf("%s=%.1fms", name, float64(now.Sub(t.last).Microseconds())/1000))
	t.last = now
}

func (t *desktopTrace) flush(bytes int) {
	if t == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "[desktop-trace] %s total=%.1fms json=%dB\n", strings.Join(t.segs, " "),
		float64(time.Since(t.start).Microseconds())/1000, bytes)
}
