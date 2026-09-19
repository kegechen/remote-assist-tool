//go:build windows

package tools

import (
	"sync"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

var (
	oemKernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleOutputCP = oemKernel32.NewProc("GetConsoleOutputCP")
	procGetOEMCP           = oemKernel32.NewProc("GetOEMCP")
	procIsDBCSLeadByteEx   = oemKernel32.NewProc("IsDBCSLeadByteEx")
)

const (
	cpUTF8 = 65001
	// mbErrInvalidChars 让 MultiByteToWideChar 在遇到非法字节时报错而不是静默替换成 U+FFFD。
	// 这是"真二进制不要被硬转成乱码汉字"的关键：没有它，任意字节流都能"转换成功"，
	// decodingWriter 就再也分不出代码页文本和二进制。
	mbErrInvalidChars = 0x00000008
)

// oemCP 缓存代码页探测结果与该代码页的 DBCS lead byte 表。
//
// 缓存是必要的而不是过早优化：splitTrailingPartial 要逐字节判 lead byte，一条命令吐
// 32 KiB 就是三万次 DLL 调用(每次 LazyProc.Call 还要分配参数切片)，而 consoleCodePage
// 本身在每次 Write 里被调三遍。探测一次、把 256 个字节的判定结果摊平成数组之后，热路径上
// 只剩一次数组索引。
//
// 缓存进程生命周期是安全的：活动代码页只能由进程自己 chcp 改，agent 通常是后台服务，
// 子进程里的 chcp 也影响不到父进程。
var oemCP struct {
	once sync.Once
	cp   uint32
	lead [256]bool
}

// consoleCodePage 返回该用哪个代码页来解释非 UTF-8 的命令输出，0 表示没有可用的。
//
// 优先 GetConsoleOutputCP：控制台程序(cmd 内置命令、多数 CRT 程序)按它决定输出编码，
// 子进程继承 agent 的控制台。它明确返回 65001 时直接放弃转码并返回 0——控制台既然已经
// 说了自己是 UTF-8，走到这一步的非 UTF-8 字节就只可能是二进制，再拿别的代码页去套，
// 结果是把二进制解成一串像模像样的 CJK，比老实标成 binary 糟得多(MCP 出口那层还有
// 有损文本兜底，不会真的丢内容)。
//
// agent 以服务方式运行时没有控制台，该调用返回 0，此时退回 GetOEMCP。用 OEM 而不是
// ACP：控制台/CRT 输出走的是 OEM 代码页，两者在不少区域并不相等(ru-RU 是 866 对 1251，
// en-US 是 437 对 1252)。这类差异全是单字节代码页，MultiByteToWideChar 不会报错，
// 只会安静地给出错误的字符——挑错了根本不会有人发现。中文 Windows 上两者都是 936，
// 所以这条修正对本地场景无感，但对俄语/西欧区域是实打实的正确性。
func consoleCodePage() uint32 {
	oemCP.once.Do(initConsoleCodePage)
	return oemCP.cp
}

func initConsoleCodePage() {
	oemCP.cp = detectConsoleCodePage()
	if oemCP.cp == 0 {
		return
	}
	for c := 0; c < 256; c++ {
		r, _, _ := procIsDBCSLeadByteEx.Call(uintptr(oemCP.cp), uintptr(c))
		oemCP.lead[c] = r != 0
	}
}

func detectConsoleCodePage() uint32 {
	// 有控制台就以它为准，包括它说自己是 UTF-8 的情况——那时返回 0，不再往下试。
	if r, _, _ := procGetConsoleOutputCP.Call(); r != 0 {
		if cp := uint32(r); cp != cpUTF8 {
			return cp
		}
		return 0
	}
	if r, _, _ := procGetOEMCP.Call(); r != 0 && uint32(r) != cpUTF8 {
		return uint32(r)
	}
	return 0
}

// consoleTranscode 把一段活动代码页的字节转成 UTF-8。ok=false 表示这段字节不是该代码页的
// 合法文本(多半是二进制)，调用方应原样透传而不是使用返回值。
//
// 用系统的 MultiByteToWideChar 而不是自带一张转码表：不引新依赖(x/sys 已在用)，且自动
// 支持 932/936/949/1251 等任意区域——写死 GBK 的话日文机器照样乱码。
func consoleTranscode(b []byte) ([]byte, bool) {
	if len(b) == 0 {
		return nil, true
	}
	cp := consoleCodePage()
	if cp == 0 {
		return nil, false
	}
	n, err := windows.MultiByteToWideChar(cp, mbErrInvalidChars, &b[0], int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return nil, false
	}
	w := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cp, mbErrInvalidChars, &b[0], int32(len(b)), &w[0], n); err != nil {
		return nil, false
	}
	return []byte(string(utf16.Decode(w))), true
}

// splitTrailingPartial 把缓冲切成"能转的部分"和"尾部孤立的 DBCS lead byte"。
//
// 双字节代码页里一个汉字占 2 字节，而 os/exec 的 Write 边界落在哪儿完全看管道缓冲，
// 切在汉字中间是常态。不把这半个字节留到下一次，MultiByteToWideChar 会因为末尾那个
// 落单的 lead byte 判定整段非法(mbErrInvalidChars)，于是一整块输出退化成原样透传。
//
// 正向扫描而不是从尾部回看：0x81-0xFE 这个范围里 trail byte 和 lead byte 会重叠，
// 只有从头按"lead 吃 2 字节、否则吃 1 字节"走一遍才知道末字节到底是不是落单的 lead。
func splitTrailingPartial(b []byte) (head, tail []byte) {
	if consoleCodePage() == 0 {
		return b, nil
	}
	i := 0
	for i < len(b) {
		if oemCP.lead[b[i]] {
			if i+1 >= len(b) {
				return b[:i], b[i:]
			}
			i += 2
			continue
		}
		i++
	}
	return b, nil
}
