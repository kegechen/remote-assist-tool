package tools

import (
	"bytes"
	"image"
	"image/jpeg"
	"math/rand"
	"testing"
)

// 合成带渐变+噪声+文字样条纹的测试图。
func synthImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := y*img.Stride + x*4
			v := uint8(rnd.Intn(40))
			if (x/7+y/5)%3 == 0 {
				v += 180
			}
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = uint8(x*255/w)+v, uint8(y*255/h)+v/2, v, 255
		}
	}
	return img
}

// 优化前的单线程参考实现。
func scaleRef(src *image.RGBA, maxW int) *image.RGBA {
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
	for dy := 0; dy < dh; dy++ {
		y0, y1 := dy*sh/dh, (dy+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			x0, x1 := dx*sw/dw, (dx+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, cnt uint32
			for y := y0; y < y1; y++ {
				off := y*src.Stride + x0*4
				for x := x0; x < x1; x++ {
					r += uint32(src.Pix[off])
					g += uint32(src.Pix[off+1])
					b += uint32(src.Pix[off+2])
					off += 4
					cnt++
				}
			}
			o := dy*dst.Stride + dx*4
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/cnt), uint8(g/cnt), uint8(b/cnt), 255
		}
	}
	return dst
}

func TestScaleParallelMatchesReference(t *testing.T) {
	for _, c := range [][3]int{{1920, 1080, 1280}, {1366, 768, 800}, {2560, 1440, 1000}, {300, 200, 160}, {1920, 1080, 1919}} {
		src := synthImage(c[0], c[1])
		got, want := scaleToWidth(src, c[2]), scaleRef(src, c[2])
		if got.Bounds() != want.Bounds() || !bytes.Equal(got.Pix, want.Pix) {
			t.Fatalf("%v: 并行缩放与参考实现不一致", c)
		}
	}
}

func TestStripedJPEGDecodesLikeSingle(t *testing.T) {
	for _, sz := range [][2]int{{1280, 720}, {1000, 563}, {333, 777}, {64, 700}, {1280, 130}} {
		img := synthImage(sz[0], sz[1])
		single, err := encodeJPEGSingle(img, 60)
		if err != nil {
			t.Fatal(err)
		}
		striped, err := encodeJPEG(img, 60)
		if err != nil {
			t.Fatal(err)
		}
		a, err := jpeg.Decode(bytes.NewReader(single))
		if err != nil {
			t.Fatal(err)
		}
		b, err := jpeg.Decode(bytes.NewReader(striped))
		if err != nil {
			t.Fatalf("%v 条带 JPEG 无法解码: %v", sz, err)
		}
		if a.Bounds() != b.Bounds() {
			t.Fatalf("%v 尺寸不同", sz)
		}
		ya, yb := a.(*image.YCbCr), b.(*image.YCbCr)
		if !bytes.Equal(ya.Y, yb.Y) || !bytes.Equal(ya.Cb, yb.Cb) || !bytes.Equal(ya.Cr, yb.Cr) {
			t.Fatalf("%v 条带 JPEG 解码结果与单线程不一致", sz)
		}
	}
}

func BenchmarkScale1080To1280(b *testing.B) {
	src := synthImage(1920, 1080)
	b.Run("ref", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scaleRef(src, 1280)
		}
	})
	b.Run("parallel", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scaleToWidth(src, 1280)
		}
	})
}

func BenchmarkJPEG1280x720(b *testing.B) {
	img := scaleToWidth(synthImage(1920, 1080), 1280)
	b.Run("single", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			encodeJPEGSingle(img, 60)
		}
	})
	b.Run("striped", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			encodeJPEG(img, 60)
		}
	})
}
