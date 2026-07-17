package tools

import (
	"fmt"
	"unicode/utf8"
)

// 截断的目的是省 token / 省带宽，但绝不能让使用者丢掉定位问题所需的信息。
// 因此按数据的语义决定保留哪一段：
//
//   - TruncateHead   保留开头 —— grep 单行，匹配通常在行首附近
//   - TruncateTail   保留结尾 —— tail_log，语义就是看最新的那几行
//   - TruncateMiddle 保留头尾、省略中间 —— exec，报错与堆栈几乎总在输出末尾，
//     只保开头等于把最关键的信息丢掉
//
// read_file 不在此列：它有 offset/length 分块协议，靠源头 cap 保证可续读，
// 截断反而会让 eof/bytes_len 与实际内容对不上（见 file.go）。
//
// 所有函数都在 UTF-8 字符边界对齐，不会切出非法字节序列。

// alignDown 返回 <= i 的最大 UTF-8 字符边界，使 s[:结果] 一定是合法 UTF-8。
func alignDown(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// alignUp 返回 >= i 的最小 UTF-8 字符边界，使 s[结果:] 一定是合法 UTF-8。
func alignUp(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// TruncateHead 保留 s 开头至多 max 字节。
func TruncateHead(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:alignDown(s, max)]
}

// TruncateTail 保留 s 结尾至多 max 字节。
func TruncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[alignUp(s, len(s)-max):]
}

// truncMarkerReserve 是中间省略标记预留的字节数（标记含一个十进制字节数，留足余量）。
const truncMarkerReserve = 96

// TruncateMiddle 保留 s 的头部与尾部、省略中间，返回值总长不超过 max。
// 第二个返回值表示是否发生了截断。
func TruncateMiddle(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	// max 小到放不下标记时退化为保留头部，避免标记本身撑爆预算
	if max <= truncMarkerReserve {
		return TruncateHead(s, max), true
	}
	budget := max - truncMarkerReserve
	head := alignDown(s, budget/2)
	tail := alignUp(s, len(s)-(budget-budget/2))
	marker := fmt.Sprintf("\n...[中间 %d 字节已省略；需要完整输出请调高 max_output_bytes]...\n", tail-head)
	return s[:head] + marker + s[tail:], true
}
