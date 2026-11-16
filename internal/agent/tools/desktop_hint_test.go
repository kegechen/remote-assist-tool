package tools

import (
	"image"
	"math/rand"
	"reflect"
	"testing"
)

// 只重算脏区覆盖的块，结果必须与整幅重算逐块一致；脏区为空时直接复用旧结果。
func TestComputeTilesHintedEqualsFull(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for _, sz := range [][2]int{{400, 300}, {1280, 800}, {65, 63}, {64, 64}, {1, 1}} {
		img := randImg(r, sz[0], sz[1])
		old := computeTiles(img)
		for i := 0; i < 40; i++ {
			next := image.NewRGBA(img.Bounds())
			copy(next.Pix, img.Pix)
			x0, y0 := r.Intn(sz[0]), r.Intn(sz[1])
			d := image.Rect(x0, y0, x0+1+r.Intn(sz[0]-x0), y0+1+r.Intn(sz[1]-y0))
			for y := d.Min.Y; y < d.Max.Y; y++ {
				for x := d.Min.X; x < d.Max.X; x++ {
					next.Pix[y*next.Stride+x*4] = uint8(r.Intn(256))
				}
			}
			got, want := computeTilesHinted(next, old, d), computeTiles(next)
			if !reflect.DeepEqual(got.hashes, want.hashes) || got.total != want.total {
				t.Fatalf("%v 脏区 %v：增量哈希与整幅重算不一致", sz, d)
			}
		}
		if g := computeTilesHinted(img, old, image.Rectangle{}); g != old {
			t.Fatal("脏区为空应直接复用旧哈希")
		}
	}
}

// gridFor 只有在 prev 正是缓存哈希对应的图像时才走增量，否则必须整幅重算（并发下缓存被别人更新的情形）。
func TestGridForIgnoresMismatchedPrev(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	c := newFrameCache()
	k := frameKey{0, 200, 70}
	a := randImg(r, 300, 200)
	c.checkDelta(k, computeTiles(a), a, 0)
	other := randImg(r, 300, 200) // 与缓存无关的图像，却被声称是 prev
	b := randImg(r, 300, 200)
	got := c.gridFor(k, b, other, image.Rect(0, 0, 10, 10))
	if !reflect.DeepEqual(got.hashes, computeTiles(b).hashes) {
		t.Fatal("prev 与缓存不符时必须整幅重算")
	}
}
