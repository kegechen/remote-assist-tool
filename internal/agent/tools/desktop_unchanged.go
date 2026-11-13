package tools

import (
	"encoding/binary"
	"image"
	"sync"
	"time"
)

// 帧无变化检测：对整幅捕获图做快速哈希（按行并行，64 位字一次处理），成本约 1~3ms，
// 远小于缩放 + JPEG 编码（几十 ms）。同一显示器的哈希与编码参数缓存在 frameCache 里，
// 请求带 since（上一帧的 seq）且画面和参数都没变时，直接返回很小的 {unchanged:true,seq}。
//
// seq 的语义：标识「某显示器 + 某组编码参数下的某个画面内容」。画面哈希或编码参数一变就递增，
// 所以 since==当前 seq 就意味着对端手里的那一帧与现在要发的完全一致；
// 任何不匹配（重启后 seq 不同、参数变了、画面变了）都回完整帧，不会出现错误的“未变化”。

// frameHash 对整幅 RGBA 图按行并行哈希。每行独立哈希后再顺序合并，结果与并行度无关（稳定）。
func frameHash(img *image.RGBA) uint64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return 0
	}
	rows := make([]uint64, h)
	rowBytes := w * 4
	parallelRows(h, 32, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			off := y * img.Stride
			rows[y] = hashRow(img.Pix[off : off+rowBytes])
		}
	})
	const prime = 1099511628211
	sum := uint64(14695981039346656037) ^ uint64(w)<<32 ^ uint64(h)
	for _, r := range rows {
		sum = (sum ^ r) * prime
		sum ^= sum >> 29
	}
	return sum
}

// hashRow 以 8 字节为单位做乘法混合；两条独立的累加链减少乘法延迟的串行依赖。
func hashRow(p []byte) uint64 {
	const (
		m1 = 0x9E3779B97F4A7C15
		m2 = 0xC2B2AE3D27D4EB4F
	)
	var a, c uint64 = 0x243F6A8885A308D3, 0x13198A2E03707344
	i := 0
	for ; i+16 <= len(p); i += 16 {
		a = (a ^ binary.LittleEndian.Uint64(p[i:])) * m1
		a ^= a >> 32
		c = (c ^ binary.LittleEndian.Uint64(p[i+8:])) * m2
		c ^= c >> 32
	}
	for ; i < len(p); i++ {
		a = (a ^ uint64(p[i])) * m1
	}
	return a ^ (c << 1) ^ (c >> 63)
}

// frameKey 某显示器的编码参数；参数变了对端手里的旧帧就不能代替新帧。
type frameKey struct {
	display, maxWidth, quality int
}

type frameState struct {
	hash uint64
	key  frameKey
	seq  uint64
}

// frameCache 每个显示器缓存最近一次的哈希、参数与 seq。
type frameCache struct {
	mu   sync.Mutex
	last map[int]*frameState
	next uint64
}

func newFrameCache() *frameCache {
	// seq 起点取启动时刻的毫秒数：被控端重启后不会与旧 seq 撞上（撞上则可能误判“未变化”）。
	return &frameCache{last: map[int]*frameState{}, next: uint64(time.Now().UnixMilli())}
}

// check 登记本次画面哈希与参数，返回当前 seq 与「相对 since 是否未变化」。
// since==0 表示对端没有可复用的旧帧，永远视为有变化。
func (c *frameCache) check(k frameKey, hash, since uint64) (seq uint64, unchanged bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.last[k.display]
	if st != nil && st.hash == hash && st.key == k {
		return st.seq, since != 0 && since == st.seq
	}
	c.next++
	c.last[k.display] = &frameState{hash: hash, key: k, seq: c.next}
	return c.next, false
}
