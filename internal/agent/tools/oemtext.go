package tools

import (
	"io"
)

// 命令输出的编码规整：让上行的 exec 输出永远是 UTF-8。
//
// 背景：被协助端不一定说 UTF-8。中文 Windows 上 cmd 内置命令、ping、systeminfo 这些
// 吐的是活动代码页(CP936/GBK)的字节；日文机器是 CP932，韩文是 CP949。这些字节不是合法
// UTF-8，一路上行之后：
//   - MCP 出口(humanize.addStream)判成 binary，内容整段不回传，调用方只能重跑命令；
//   - boundedStream 的截断对齐按 utf8.RuneStart 找边界，对 DBCS 字节根本不成立，
//     32 KiB 处会切出半个汉字。
//
// 在被协助端转而不是在消费端转，是因为只有被协助端知道自己的活动代码页——上行之后那个
// 信息就没了，消费端只能猜。
//
// 作用范围仅限 exec 的**非流式**路径(cmd.Stdout/Stderr)，也就是 MCP 走的那条。
// GUI 终端走 stream=true，用的是 StdoutPipe 而不是 cmd.Stdout，不经过这一层，仍然依赖
// 前端 internal/gui/assets/assets.go 里 makeDecoder 的 UTF-8/GBK 嗅探。
// 动它要先把这里的块边界缓存搬到 runStreaming 的 pump 里，是笔单独的账。
// **在那之前不要删前端那个回退**，否则 GUI 在中文 Windows 上会立刻退回乱码。
//
// 转码只在"确认不是 UTF-8"之后才发生：Windows 上 go / git / node / python 很多已经直接
// 输出 UTF-8，无脑按 CP936 转会把本来正确的中文转成乱码。顺序必须是先验 UTF-8 再回退。

// utf8MaxPending 是 UTF-8 模式下允许攒在 pending 里的最大字节数。合法 UTF-8 的尾部残片
// 最多 3 字节(4 字节序列缺 1)，超过这个数说明扫描逻辑出了问题，宁可吐出去也不无限攒。
const utf8MaxPending = 4

// scanUTF8 扫一段字节：complete 是合法且完整的前缀长度，ok=false 表示撞到了非法字节。
//
// 末尾"被写边界切断的多字节字符"不算非法(ok 仍为 true)，它只是还没走完，留给下一次写去拼。
// 区分"这不是 UTF-8"与"UTF-8 还没读完"是整件事的关键：cmd.Stdout 会被 os/exec 分多次
// Write，一个汉字被切在两次 Write 之间是常态，若把它当非法字节就会误切到代码页模式，
// 把本来正确的 UTF-8 输出转成乱码。
func scanUTF8(b []byte) (complete int, ok bool) {
	i := 0
	for i < len(b) {
		c := b[i]
		var need int
		switch {
		case c < 0x80:
			need = 1
		case c >= 0xC2 && c <= 0xDF:
			need = 2
		case c >= 0xE0 && c <= 0xEF:
			need = 3
		case c >= 0xF0 && c <= 0xF4:
			need = 4
		default:
			return i, false // 0x80-0xC1、0xF5-0xFF：不可能是合法序列的首字节
		}
		if i+need > len(b) {
			return i, true // 尾巴被切断，下一次写再说
		}
		for j := 1; j < need; j++ {
			if b[i+j]&0xC0 != 0x80 {
				return i, false
			}
		}
		i += need
	}
	return i, true
}

// decodingWriter 夹在命令的输出管道与 boundedStream 之间，把非 UTF-8 的控制台输出按
// 被协助端的活动代码页转成 UTF-8 再往下游写。
//
// 为什么放在 boundedStream 的**上游**而不是在 result() 之后转：result() 会往截断处插一段
// UTF-8 的中文省略标记，GBK 主体混 UTF-8 标记之后整段既不是合法 GBK 也不是合法 UTF-8，
// 再想转就晚了。放上游还有个附带好处——boundedStream 的 utf8.RuneStart 对齐终于名副其实。
//
// 代价是 max_output_bytes 的语义变成"转码后的 UTF-8 字节数"。这是对的：调用方关心的是
// 自己要消化多少内容，不是被协助端的代码页恰好用了几个字节。
type decodingWriter struct {
	dst io.Writer
	// pending 是尾部还没法定论的字节：被切断的 UTF-8 序列，或孤立的 DBCS lead byte。
	// 下一次 Write 时拼到开头一起处理。
	pending []byte
	// pendingUTF8 记录 pending 是哪一种残片。Flush 时必须分开对待：一个被切断的 UTF-8
	// 序列(比如 "中" 的前两字节 E4 B8)在 CP936 里恰好是合法的 lead+trail 对，照着转会
	// 凭空造出一个汉字。UTF-8 残片只能原样吐出去。
	pendingUTF8 bool
	// passthrough 置位表示代码页转码也失败了(真二进制，或系统本身就是 UTF-8)，此后一律
	// 原样透传，把判定权交回下游。
	//
	// 这个锁存是有意保留的，与下面"每次写都重新判 UTF-8"不同：转码失败基本只在真二进制
	// 上发生，而二进制流后半段变回文本的可能性极低，没必要为每个块都重试一遍转码。
	passthrough bool
}

func newDecodingWriter(dst io.Writer) *decodingWriter { return &decodingWriter{dst: dst} }

func (d *decodingWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	buf := p
	if len(d.pending) > 0 {
		buf = make([]byte, 0, len(d.pending)+len(p))
		buf = append(buf, d.pending...)
		buf = append(buf, p...)
		d.pending = d.pending[:0]
	}

	if d.passthrough {
		_, err := d.dst.Write(buf)
		return n, err
	}

	// 每次写都重新判，不锁死在某个模式：cmd /c "a & b" 完全可以一条子命令吐 GBK、另一条
	// 吐 UTF-8，锁死之后后半段会被按错的编码解成静默错误的文本——那比修复前判 binary 更糟，
	// 调用方连"这里不对"的信号都拿不到。scanUTF8 是 O(n) 单遍，重判不值得省。
	complete, ok := scanUTF8(buf)
	if ok {
		// 尾部残片留到下一次；超过 UTF-8 序列可能的长度说明不对劲，整段吐出去别攒着。
		if rest := len(buf) - complete; rest > 0 && rest <= utf8MaxPending {
			d.keepPending(buf[complete:], true)
		} else {
			complete = len(buf)
		}
		if complete == 0 {
			return n, nil
		}
		_, err := d.dst.Write(buf[:complete])
		return n, err
	}

	// 撞到非法 UTF-8 字节。complete 之前的部分刚刚被证明是合法 UTF-8，必须原样送走：
	// 把它一起丢进 DBCS 配对不只是让前缀变乱码，更会让后面的 lead/trail 整体错位，
	// 连本来能正确解码的代码页文本也一起毁掉。
	//   "中"(E4 B8 AD) + GBK"正在"(D5 FD D4 DA)
	//   → 配成 (E4 B8)(AD D5)(FD D4) 余 DA，七个字节没一个落在原字符边界上。
	if complete > 0 {
		if _, err := d.dst.Write(buf[:complete]); err != nil {
			return n, err
		}
		buf = buf[complete:]
	}

	// 剩下的按代码页解；尾部可能卡着半个双字节字符，留给下一次写。
	head, tail := splitTrailingPartial(buf)
	if len(head) == 0 {
		d.keepPending(tail, false)
		return n, nil
	}
	utf8Bytes, ok := consoleTranscode(head)
	if !ok {
		// 代码页也解不动：这要么是真二进制，要么系统本身就是 UTF-8(那非 UTF-8 字节只能是
		// 二进制)。原样透传并永久放弃转码，由下游按二进制处理——硬转只会把二进制变成一堆
		// 看着像中文的垃圾，比明说"这是二进制"更糟。
		d.passthrough = true
		_, err := d.dst.Write(buf)
		return n, err
	}
	d.keepPending(tail, false)
	_, err := d.dst.Write(utf8Bytes)
	return n, err
}

func (d *decodingWriter) keepPending(b []byte, isUTF8 bool) {
	d.pending = append(d.pending[:0], b...)
	d.pendingUTF8 = isUTF8
}

// Flush 把流末尾剩下的残片吐出去。必须在 cmd.Wait() 返回之后调用——那时 os/exec 的复制
// goroutine 已经收工，不会再有并发 Write。
//
// 不 flush 的后果是命令输出的最后一个字符凭空消失(GBK 尾字、或被切断的 UTF-8 尾字)，
// 而且只在特定长度下复现，是那种能查一整天的 bug。
func (d *decodingWriter) Flush() error {
	if len(d.pending) == 0 {
		return nil
	}
	b := d.pending
	d.pending = nil
	// pendingUTF8 的残片绝不能拿去转码：被切断的 UTF-8 序列在 DBCS 代码页里往往是合法的
	// lead+trail 对("中" 的前两字节 E4 B8 在 CP936 下就是)，转出来是个凭空捏造的汉字。
	if !d.pendingUTF8 && !d.passthrough {
		if u, ok := consoleTranscode(b); ok {
			_, err := d.dst.Write(u)
			return err
		}
	}
	// 残片本身解不出来(被截断的半个字符就会这样)：原样吐出，让下游决定怎么标。
	_, err := d.dst.Write(b)
	return err
}
