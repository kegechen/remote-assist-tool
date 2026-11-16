package tools

import (
	"bytes"
	"image"
	"math/rand"
	"testing"
)

func randImg(r *rand.Rand, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(r.Intn(256))
	}
	return img
}

func cropRGBA(src *image.RGBA, r image.Rectangle) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		copy(dst.Pix[y*dst.Stride:y*dst.Stride+r.Dx()*4], src.Pix[(r.Min.Y+y)*src.Stride+r.Min.X*4:])
	}
	return dst
}

// 区域缩放必须与整幅缩放后裁剪逐像素一致，否则增量块拼回画布会出现接缝。
func TestScaleRectMatchesFullScale(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, c := range [][3]int{{300, 200, 150}, {301, 199, 97}, {1920, 1080, 1280}, {500, 300, 499}, {64, 64, 7}} {
		src := randImg(r, c[0], c[1])
		dw, dh := scaledSize(c[0], c[1], c[2])
		full := scaleRect(src, dw, dh, image.Rect(0, 0, dw, dh))
		if !bytes.Equal(full.Pix, scaleToWidth(src, c[2]).Pix) && c[0] > c[2] {
			t.Fatalf("scaleToWidth 与整幅 scaleRect 不一致 %v", c)
		}
		for i := 0; i < 30; i++ {
			x0, y0 := r.Intn(dw), r.Intn(dh)
			rc := image.Rect(x0, y0, x0+1+r.Intn(dw-x0), y0+1+r.Intn(dh-y0))
			if !bytes.Equal(scaleRect(src, dw, dh, rc).Pix, cropRGBA(full, rc).Pix) {
				t.Fatalf("区域缩放与整幅裁剪不一致 %v %v", c, rc)
			}
		}
	}
}

func TestSrcRectToDstCovers(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 2000; i++ {
		sw, sh := 100+r.Intn(1900), 100+r.Intn(1100)
		dw := 10 + r.Intn(sw)
		dh := sh * dw / sw
		if dh < 1 {
			dh = 1
		}
		x0, y0 := r.Intn(sw), r.Intn(sh)
		sr := image.Rect(x0, y0, x0+1+r.Intn(sw-x0), y0+1+r.Intn(sh-y0))
		dr := srcRectToDst(sr, sw, sh, dw, dh)
		// 目标矩形内每个块的源范围必须覆盖 sr 的每一个源像素所属块：即 sr 内任意源点所在的目标块都在 dr 内
		for k := 0; k < 20; k++ {
			sx, sy := sr.Min.X+r.Intn(sr.Dx()), sr.Min.Y+r.Intn(sr.Dy())
			owner := func(s, sn, dn int) int {
				for d := 0; d < dn; d++ {
					if d*sn/dn <= s && s < (d+1)*sn/dn {
						return d
					}
				}
				return dn - 1
			}
			dx, dy := owner(sx, sw, dw), owner(sy, sh, dh)
			if dh == 1 || dw == 1 { // 退化尺寸不做逐点校验
				continue
			}
			if !image.Pt(dx, dy).In(dr) {
				t.Fatalf("源点 (%d,%d) 所属目标块 (%d,%d) 不在 %v 内 (%dx%d→%dx%d sr=%v)", sx, sy, dx, dy, dr, sw, sh, dw, dh, sr)
			}
		}
	}
}

// 模拟流水线下的客户端画布：since 取自过去任意一个已应用状态，按返回区域用当前帧内容更新，画布必须恒等于当前帧。
func TestDeltaCanvasAlwaysConverges(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		const W, H, MAXW = 400, 300, 200
		cur := randImg(r, W, H)
		c := newFrameCache()
		key := frameKey{0, MAXW, 70}
		dw, dh := scaledSize(W, H, MAXW)
		seq, _, _, _ := c.checkDelta(key, computeTiles(cur), 0)
		canvas := scaleRect(cur, dw, dh, image.Rect(0, 0, dw, dh))
		history := []uint64{seq}
		for step := 0; step < 60; step++ {
			// 随机改动一小块（有时不改、有时改回去、有时大面积）
			switch r.Intn(5) {
			case 0: // 不变
			case 1:
				for i := 0; i < 5000; i++ {
					cur.Pix[r.Intn(len(cur.Pix))] = uint8(r.Intn(256))
				}
			default:
				x0, y0 := r.Intn(W-20), r.Intn(H-20)
				for y := y0; y < y0+1+r.Intn(20); y++ {
					for x := x0; x < x0+1+r.Intn(20); x++ {
						cur.Pix[y*cur.Stride+x*4] = uint8(r.Intn(256))
					}
				}
			}
			since := history[len(history)-1-r.Intn(min(3, len(history)))]
			seq, same, reg, ok := c.checkDelta(key, computeTiles(cur), since)
			full := scaleRect(cur, dw, dh, image.Rect(0, 0, dw, dh))
			switch {
			case same:
				// 与 since 完全一致：画布应已相同（since 是画布曾处于的状态，而画布现在恰在该状态或更新——此处画布就是上一帧）
			case ok && deltaWorth(srcRectToDst(reg, W, H, dw, dh), dw, dh):
				dr := srcRectToDst(reg, W, H, dw, dh)
				part := scaleRect(cur, dw, dh, dr)
				for y := 0; y < dr.Dy(); y++ {
					copy(canvas.Pix[(dr.Min.Y+y)*canvas.Stride+dr.Min.X*4:], part.Pix[y*part.Stride:y*part.Stride+dr.Dx()*4])
				}
			default:
				copy(canvas.Pix, full.Pix)
			}
			if !bytes.Equal(canvas.Pix, full.Pix) {
				t.Fatalf("seed %d step %d: 画布与当前帧不一致 (since=%d seq=%d same=%v ok=%v)", seed, step, since, seq, same, ok)
			}
			if len(history) == 0 || history[len(history)-1] != seq {
				history = append(history, seq)
			}
		}
	}
}
