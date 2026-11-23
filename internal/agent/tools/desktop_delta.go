package tools

import (
	"image"
)

// 增量帧（脏区）：把捕获图按 64x64 切块逐块哈希，只把相对对端已有画面发生变化的区域重新缩放、编码并返回。
// 桌面常见操作（输入、光标闪烁、局部刷新）改动面积远小于整屏，编码量和传输量随之成比例下降。
//
// 正确性的关键：请求是流水线并发的，对端画布可能已经比请求里的 since 更新（更晚的响应先应用）。
// 所以服务端按「每次 seq 变化时哪些块变了」记账（环形缓冲），对 since 之后的全部记录取并集：
// 并集覆盖画布可能处于的任何中间状态，把这些块用当前内容重发即可使画布与当前帧完全一致。
// since 不在记录里（太旧、参数变了、重启）就回完整帧。

const (
	tileSize        = 64
	tileRingSize    = 24 // 记录的 seq 个数；流水线深度只有 1~3，足够
	deltaMaxAreaPct = 55 // 变化包围盒超过整幅这个比例就直接发整帧
)

type tileGrid struct {
	tw, th int // 块列数、行数
	w, h   int // 源图尺寸
	hashes []uint64
	total  uint64
}

// computeTiles 并行计算每个 64x64 块的哈希，及整幅的汇总哈希（与块内容和位置相关）。
func computeTiles(img *image.RGBA) *tileGrid {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	g := &tileGrid{w: w, h: h, tw: (w + tileSize - 1) / tileSize, th: (h + tileSize - 1) / tileSize}
	if w <= 0 || h <= 0 {
		return g
	}
	g.hashes = make([]uint64, g.tw*g.th)
	parallelRows(g.th, 1, func(lo, hi int) {
		for ty := lo; ty < hi; ty++ {
			for tx := 0; tx < g.tw; tx++ {
				g.hashes[ty*g.tw+tx] = tileHash(img, tx, ty)
			}
		}
	})
	g.total = foldTiles(g)
	return g
}

// tileHash 计算块 (tx,ty) 的哈希。
func tileHash(img *image.RGBA, tx, ty int) uint64 {
	b := img.Bounds()
	x0, x1 := tx*tileSize, (tx+1)*tileSize
	y0, y1 := ty*tileSize, (ty+1)*tileSize
	if x1 > b.Dx() {
		x1 = b.Dx()
	}
	if y1 > b.Dy() {
		y1 = b.Dy()
	}
	var acc uint64 = 1469598103934665603
	for y := y0; y < y1; y++ {
		off := y*img.Stride + x0*4
		acc = (acc ^ hashRow(img.Pix[off:off+(x1-x0)*4])) * 1099511628211
		acc ^= acc >> 29
	}
	return acc
}

// foldTiles 把各块哈希折叠成整幅哈希（与尺寸相关）。
func foldTiles(g *tileGrid) uint64 {
	const prime = 1099511628211
	sum := uint64(14695981039346656037) ^ uint64(g.w)<<32 ^ uint64(g.h)
	for _, v := range g.hashes {
		sum = (sum ^ v) * prime
		sum ^= sum >> 29
	}
	return sum
}

// changedTiles 返回 a→b 之间哈希不同的块下标集合（两者尺寸须一致）。
func changedTiles(a, b *tileGrid) []int {
	var out []int
	for i := range b.hashes {
		if a.hashes[i] != b.hashes[i] {
			out = append(out, i)
		}
	}
	return out
}

type tileRecord struct {
	seq     uint64
	changed []int // 相对上一条记录变化的块；首条（基准）为空
}

// tileTracker 某显示器（固定编码参数）下的块哈希与变化记录。
type tileTracker struct {
	grid *tileGrid
	recs []tileRecord
}

func (t *tileTracker) push(seq uint64, g *tileGrid) {
	var ch []int
	if t.grid != nil && t.grid.tw == g.tw && t.grid.th == g.th && t.grid.w == g.w && t.grid.h == g.h {
		ch = changedTiles(t.grid, g)
	} else {
		t.recs = t.recs[:0] // 尺寸变了：旧记录作废
	}
	t.recs = append(t.recs, tileRecord{seq: seq, changed: ch})
	if len(t.recs) > tileRingSize {
		t.recs = append(t.recs[:0], t.recs[len(t.recs)-tileRingSize:]...)
	}
	t.grid = g
}

// regionSince 返回自 since 以来变化过的块的源图包围盒；since 不在记录里或没有后续记录时 ok=false。
func (t *tileTracker) regionSince(since uint64) (image.Rectangle, bool) {
	if t.grid == nil || since == 0 {
		return image.Rectangle{}, false
	}
	idx := -1
	for i, r := range t.recs {
		if r.seq == since {
			idx = i
			break
		}
	}
	if idx < 0 || idx == len(t.recs)-1 {
		return image.Rectangle{}, false
	}
	minX, minY, maxX, maxY := t.grid.tw, t.grid.th, -1, -1
	for _, r := range t.recs[idx+1:] {
		for _, i := range r.changed {
			x, y := i%t.grid.tw, i/t.grid.tw
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	if maxX < 0 {
		return image.Rectangle{}, false
	}
	r := image.Rect(minX*tileSize, minY*tileSize, (maxX+1)*tileSize, (maxY+1)*tileSize)
	return r.Intersect(image.Rect(0, 0, t.grid.w, t.grid.h)), true
}

// srcRectToDst 把源图矩形映射成缩放后目标图里「完全覆盖它」的最小矩形（块边界与 scaleRect 一致）。
func srcRectToDst(r image.Rectangle, sw, sh, dw, dh int) image.Rectangle {
	lo := func(s, sn, dn int) int { // 包含源坐标 s 的目标块下标
		d := s * dn / sn
		for d > 0 && d*sn/dn > s {
			d--
		}
		for (d+1)*sn/dn <= s && d+1 < dn {
			d++
		}
		return d
	}
	x0, y0 := lo(r.Min.X, sw, dw), lo(r.Min.Y, sh, dh)
	x1, y1 := lo(r.Max.X-1, sw, dw)+1, lo(r.Max.Y-1, sh, dh)+1
	return image.Rect(x0, y0, x1, y1).Intersect(image.Rect(0, 0, dw, dh))
}

// deltaWorth 判断变化区域是否小到值得发增量。
func deltaWorth(r image.Rectangle, w, h int) bool {
	return r.Dx()*r.Dy()*100 <= w*h*deltaMaxAreaPct
}

// computeTilesHinted 在 old（上一幅图像的块哈希）基础上，只重算与 dirty 相交的块。
func computeTilesHinted(img *image.RGBA, old *tileGrid, dirty image.Rectangle) *tileGrid {
	b := img.Bounds()
	if old.w != b.Dx() || old.h != b.Dy() {
		return computeTiles(img)
	}
	d := dirty.Intersect(b)
	if d.Empty() {
		return old
	}
	g := &tileGrid{w: old.w, h: old.h, tw: old.tw, th: old.th, hashes: append([]uint64(nil), old.hashes...)}
	tx0, tx1 := d.Min.X/tileSize, (d.Max.X-1)/tileSize
	ty0, ty1 := d.Min.Y/tileSize, (d.Max.Y-1)/tileSize
	parallelRows(ty1-ty0+1, 1, func(lo, hi int) {
		for ty := ty0 + lo; ty < ty0+hi; ty++ {
			for tx := tx0; tx <= tx1; tx++ {
				g.hashes[ty*g.tw+tx] = tileHash(img, tx, ty)
			}
		}
	})
	g.total = foldTiles(g)
	return g
}
