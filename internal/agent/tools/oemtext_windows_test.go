//go:build windows

package tools

import (
	"bytes"
	"testing"
)

// gbkZhengZai 是 "正在 Ping" 的 CP936(GBK) 编码：正=D5FD 在=D4DA，随后是 ASCII " Ping"。
var gbkZhengZai = []byte{0xD5, 0xFD, 0xD4, 0xDA, 0x20, 0x50, 0x69, 0x6E, 0x67}

// requireGBK 跳过非 GBK 区域的机器。转码走系统 API，日文/韩文/英文 Windows 上活动代码页
// 不是 936，这批固定字节自然解不出 "正在"——那不是缺陷，是测试样本只覆盖了中文区域。
func requireGBK(t *testing.T) {
	t.Helper()
	if cp := consoleCodePage(); cp != 936 {
		t.Skipf("活动代码页是 %d，不是 936(GBK)，跳过中文样本", cp)
	}
}

func TestConsoleTranscodeGBK(t *testing.T) {
	requireGBK(t)
	got, ok := consoleTranscode(gbkZhengZai)
	if !ok {
		t.Fatal("GBK 字节应能转码")
	}
	if string(got) != "正在 Ping" {
		t.Errorf("转码结果 = %q，期望 %q", got, "正在 Ping")
	}
}

// TestConsoleTranscodeRejectsBinary 真二进制不该被"转换成功"。
// 这一条挂了就说明 mbErrInvalidChars 没生效，任意字节流都会被转成乱码汉字，
// decodingWriter 再也分不出文本和二进制。
func TestConsoleTranscodeRejectsBinary(t *testing.T) {
	// 0x80 在 CP936 里不是合法的 lead byte，0x00-0x02 也凑不出双字节字符。
	if _, ok := consoleTranscode([]byte{0x80, 0x00, 0x01, 0x02, 0xFF}); ok {
		t.Error("二进制不该被判为可转码的代码页文本")
	}
}

// TestDecodingWriterGBKWholeWrite 一次写入完整的 GBK 输出，应转成 UTF-8。
// 这是用户实际遇到的场景：远端中文 Windows 上 ping / dir 的第一条输出。
func TestDecodingWriterGBKWholeWrite(t *testing.T) {
	requireGBK(t)
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	if _, err := d.Write(gbkZhengZai); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := buf.String(); got != "正在 Ping" {
		t.Errorf("got=%q, want=%q", got, "正在 Ping")
	}
}

// TestDecodingWriterGBKSplitMidChar 双字节字符被切在两次 Write 之间。
//
// 这正是 splitTrailingPartial 存在的理由：不把落单的 lead byte 留到下一次，
// MultiByteToWideChar 会因为末尾那个半截字符判定整段非法，一整块输出退化成原样透传。
func TestDecodingWriterGBKSplitMidChar(t *testing.T) {
	requireGBK(t)
	for _, cut := range []int{1, 2, 3, 5} {
		var buf bytes.Buffer
		d := newDecodingWriter(&buf)
		if _, err := d.Write(gbkZhengZai[:cut]); err != nil {
			t.Fatalf("cut=%d 第一次 Write: %v", cut, err)
		}
		if _, err := d.Write(gbkZhengZai[cut:]); err != nil {
			t.Fatalf("cut=%d 第二次 Write: %v", cut, err)
		}
		if err := d.Flush(); err != nil {
			t.Fatalf("cut=%d Flush: %v", cut, err)
		}
		if got := buf.String(); got != "正在 Ping" {
			t.Errorf("cut=%d: got=%q, want=%q", cut, got, "正在 Ping")
		}
	}
}

// TestDecodingWriterGBKByteAtATime 逐字节写——最极端的边界情况，每个汉字都被切开。
func TestDecodingWriterGBKByteAtATime(t *testing.T) {
	requireGBK(t)
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	for i := range gbkZhengZai {
		if _, err := d.Write(gbkZhengZai[i : i+1]); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := buf.String(); got != "正在 Ping" {
		t.Errorf("got=%q, want=%q", got, "正在 Ping")
	}
}

// TestDecodingWriterFlushUTF8RemnantNotTranscoded 流在一个 UTF-8 字符中间就结束了。
//
// 残片必须原样吐出，绝不能拿去代码页转码：被切断的 UTF-8 序列在 DBCS 里常常是合法的
// lead+trail 对——"中"(E4 B8 AD) 的前两字节 E4 B8 在 CP936 下就能解成一个汉字，于是流末尾
// 会凭空多出一个谁也没输出过的字。这是加 pendingUTF8 标记要挡的东西。
func TestDecodingWriterFlushUTF8RemnantNotTranscoded(t *testing.T) {
	requireGBK(t)
	// 先确认这个残片在 CP936 下确实"转得动"，否则本用例是空跑。
	if _, ok := consoleTranscode([]byte{0xE4, 0xB8}); !ok {
		t.Skip("E4 B8 在当前代码页下不是合法字节对，本用例无意义")
	}

	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	if _, err := d.Write([]byte("中")[:2]); err != nil { // 只写 E4 B8，流就断了
		t.Fatalf("Write: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := buf.Bytes(); !bytes.Equal(got, []byte{0xE4, 0xB8}) {
		t.Errorf("UTF-8 残片被当成 DBCS 转码了，凭空造出字符: got=%q (% x)", got, got)
	}
}

// TestDecodingWriterASCIIThenGBK 开头是 ASCII、后面才出现 GBK 中文。
//
// 纯 ASCII 在两种编码下字节相同，此时还不能定论；直到遇到非法 UTF-8 字节才切代码页模式。
// 已经透传出去的 ASCII 不受影响，接在后面的中文要能正常转出来。
func TestDecodingWriterASCIIThenGBK(t *testing.T) {
	requireGBK(t)
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	if _, err := d.Write([]byte("C:\\> ")); err != nil {
		t.Fatalf("Write ASCII: %v", err)
	}
	if _, err := d.Write(gbkZhengZai); err != nil {
		t.Fatalf("Write GBK: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, want := buf.String(), "C:\\> 正在 Ping"; got != want {
		t.Errorf("got=%q, want=%q", got, want)
	}
}
