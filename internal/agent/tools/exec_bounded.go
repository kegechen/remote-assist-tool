package tools

import (
	"fmt"
	"unicode/utf8"
)

// execMaxOutputCeiling 是 max_output_bytes 的硬上限。
//
// boundedStream 的内存占用是 max 的常数倍而非命令输出量的函数，但 max 本身来自调用方
// 传进来的 JSON，不封顶的话一个 max_output_bytes: 1<<40 就能让被协助端 OOM。8 MiB 远
// 超任何"给人读"的输出量，超出部分本来也只会被 relay 的帧大小限制打回来。
const execMaxOutputCeiling = 8 << 20

// resolveMaxOutput 把 ExecArgs.MaxOutputBytes 归一成实际生效的上限：0（未指定）取默认值，
// 超过硬上限的按硬上限收。
func resolveMaxOutput(requested int) int {
	if requested <= 0 {
		return execDefaultMaxOutput
	}
	if requested > execMaxOutputCeiling {
		return execMaxOutputCeiling
	}
	return requested
}

// boundedStream 是一个"只留头尾"的 io.Writer：写进来的内容里，开头 max 字节和最后
// max 字节被保留，中间部分边写边丢。
//
// 存在的理由是 cmd.Output() 会先把命令的全部输出攒进 bytes.Buffer、再交给截断函数——
// 一条 `find /` 或者 `cat 大文件` 就能在被协助端上吃掉几个 GB，而这些内存最终 99% 都
// 是要被 TruncateMiddle 扔掉的。这里把截断提前到写入的那一刻。
//
// result() 的输出与 TruncateMiddle(完整输出, max) 逐字节相同，因此调用方无法区分
// "先攒后截"和"边写边截"，省略标记里的字节数也仍然是真实的完整长度。
type boundedStream struct {
	max int
	// head 是最前面至多 max+utf8.UTFMax 字节。max 同时也是"未截断"的判定阈值，所以只要
	// 没超限，head 里就是全部内容。
	//
	// 多留 UTFMax 字节不是冗余：max 太小放不下省略标记时要退化成"只保开头"，而
	// alignDown(s, max) 必须能读到 s[max] 才知道那里是不是切在多字节字符中间。只留 max
	// 字节的话它会走 i >= len(s) 的分支直接返回 max，切出半个汉字。
	head []byte
	// tail 保存结尾。为省去环形缓冲的下标簿记，这里让它长到 2*max 再一次性搬走前半段，
	// 均摊下来每字节仍只被复制常数次，峰值占用 2*max。
	tail []byte
	// total 是写入的总字节数，用来还原省略标记里的真实长度。
	total int64
}

func newBoundedStream(max int) *boundedStream {
	if max < 1 {
		max = 1
	}
	return &boundedStream{max: max}
}

func (b *boundedStream) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	if room := b.max + utf8.UTFMax - len(b.head); room > 0 {
		n := room
		if n > len(p) {
			n = len(p)
		}
		b.head = append(b.head, p[:n]...)
	}
	// 单次写就盖过整个尾窗口时，前面攒的全都没用了，直接换掉，避免先 append 出一个
	// 与 p 同样大的切片——那正是本类型要避免的事。
	if len(p) >= b.max {
		b.tail = append(b.tail[:0], p[len(p)-b.max:]...)
		return len(p), nil
	}
	b.tail = append(b.tail, p...)
	if len(b.tail) >= 2*b.max {
		b.tail = b.tail[:copy(b.tail, b.tail[len(b.tail)-b.max:])]
	}
	return len(p), nil
}

// tailWindow 返回结尾至多 max 字节。
func (b *boundedStream) tailWindow() []byte {
	if len(b.tail) > b.max {
		return b.tail[len(b.tail)-b.max:]
	}
	return b.tail
}

// result 返回截断后的内容与"是否发生了截断"，规则与 TruncateMiddle 完全一致。
func (b *boundedStream) result() ([]byte, bool) {
	if b.total <= int64(b.max) {
		return b.head, false // 没超限时 head 就是全部内容
	}
	if b.max <= truncMarkerReserve {
		return []byte(TruncateHead(string(b.head), b.max)), true
	}
	budget := b.max - truncMarkerReserve
	headN := budget / 2
	tailN := budget - headN

	headStr := string(b.head)
	headIdx := alignDown(headStr, headN)

	// 对应 TruncateMiddle 里的 alignUp(s, len(s)-tailN)：从完整输出的 total-tailN 处
	// 向后找字符边界。UTF-8 后续字节最多连续 3 个，所以只会前进 ≤3 字节，而尾窗口有
	// max > tailN 字节，这段一定落在窗口内。
	t := b.tailWindow()
	off := len(t) - tailN
	for off < len(t) && !utf8.RuneStart(t[off]) {
		off++
	}
	tailIdx := b.total - int64(len(t)-off)

	marker := fmt.Sprintf("\n...[中间 %d 字节已省略；需要完整输出请调高 max_output_bytes]...\n", tailIdx-int64(headIdx))
	return []byte(headStr[:headIdx] + marker + string(t[off:])), true
}
