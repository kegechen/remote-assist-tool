package tools

import (
	"bytes"
	"testing"
)

func TestScanUTF8(t *testing.T) {
	cases := []struct {
		name     string
		in       []byte
		complete int
		ok       bool
	}{
		{"空", nil, 0, true},
		{"纯 ASCII", []byte("hello"), 5, true},
		{"完整中文", []byte("中文"), 6, true},
		{"尾部被切断的三字节字符", []byte("中文")[:4], 3, true},
		{"只剩一个首字节", []byte{0xE4}, 0, true},
		{"非法首字节", []byte{0x41, 0xFF, 0x42}, 1, false},
		{"孤立的续字节", []byte{0x80}, 0, false},
		// GBK 的 "正在"：D5 起了个 2 字节序列，FD 不是续字节 → 非法，应触发代码页回退。
		{"GBK 中文", []byte{0xD5, 0xFD, 0xD4, 0xDA}, 0, false},
		// 前面有 ASCII 时，非法字节之前的部分仍算完整前缀。
		{"ASCII 后接 GBK", []byte{0x6F, 0x6B, 0xD5, 0xFD}, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			complete, ok := scanUTF8(c.in)
			if complete != c.complete || ok != c.ok {
				t.Errorf("scanUTF8(% x) = (%d, %v), 期望 (%d, %v)", c.in, complete, ok, c.complete, c.ok)
			}
		})
	}
}

// TestDecodingWriterUTF8Passthrough 合法 UTF-8 必须原样穿过，一个字节都不能改。
// 这是最要紧的一条：Windows 上 go / git / node 早就直接输出 UTF-8 了，如果这一层
// 不分青红皂白按 CP936 转，本来正确的中文会被转成乱码——比不转还糟。
func TestDecodingWriterUTF8Passthrough(t *testing.T) {
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	in := "正在 Ping 中文输出\n第二行\n"
	if _, err := d.Write([]byte(in)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := buf.String(); got != in {
		t.Errorf("UTF-8 被改写了:\n got=%q\nwant=%q", got, in)
	}
}

// TestDecodingWriterUTF8SplitAcrossWrites 一个汉字被切在两次 Write 之间时不能误判。
//
// os/exec 的 Write 边界完全由管道缓冲决定，切在多字节字符中间是常态。若把切断的半个字符
// 当成非法字节，整条流会误切到代码页模式，后面所有正确的 UTF-8 输出都被转成乱码。
func TestDecodingWriterUTF8SplitAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	full := []byte("中文abc")
	// 逐字节写，每个多字节字符都会被切开。
	for i := range full {
		if _, err := d.Write(full[i : i+1]); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := buf.Bytes(); !bytes.Equal(got, full) {
		t.Errorf("逐字节写之后内容不一致:\n got=%q\nwant=%q", got, full)
	}
}

// TestDecodingWriterBinaryPassthrough 真二进制必须原样透传，交给下游判 binary。
// 硬转只会造出一堆看着像中文的垃圾，比明说"这是二进制"更糟。
func TestDecodingWriterBinaryPassthrough(t *testing.T) {
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	in := []byte{0x00, 0x01, 0xFF, 0xFE, 0x00, 0x7F}
	if _, err := d.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := buf.Bytes(); !bytes.Equal(got, in) {
		t.Errorf("二进制应原样透传:\n got=% x\nwant=% x", got, in)
	}
}

// TestDecodingWriterFlushEmptyIsNoop 没有残片时 Flush 不应写出任何东西。
// 否则每条命令的输出末尾都会多出一段空写，扰动 boundedStream 的 total 计数。
func TestDecodingWriterFlushEmptyIsNoop(t *testing.T) {
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)
	if _, err := d.Write([]byte("ok")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := d.Flush(); err != nil { // 再 flush 一次也不该有副作用
		t.Fatalf("Flush 第二次: %v", err)
	}
	if got := buf.String(); got != "ok" {
		t.Errorf("Flush 多写了东西: %q", got)
	}
}
