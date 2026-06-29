package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// mustJSON 用 json.Marshal 构造工具返回，复现真实工具对 []byte 字段的 base64 编码。
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestHumanizeReadFileText(t *testing.T) {
	raw := mustJSON(t, struct {
		Bytes []byte `json:"bytes"`
		EOF   bool   `json:"eof"`
	}{Bytes: []byte("hello\nworld"), EOF: true})

	got := humanizeToolResult("read_file", raw)
	var r struct {
		Text string `json:"text"`
		EOF  bool   `json:"eof"`
	}
	if err := json.Unmarshal([]byte(got), &r); err != nil {
		t.Fatalf("unmarshal humanized: %v (%s)", err, got)
	}
	if r.Text != "hello\nworld" {
		t.Fatalf("text = %q, want %q", r.Text, "hello\nworld")
	}
	if !r.EOF {
		t.Fatalf("eof lost: %s", got)
	}
	// 不应再出现 base64 的 bytes 字段
	if strings.Contains(got, `"bytes":`) {
		t.Fatalf("base64 bytes field leaked: %s", got)
	}
	// 分块续读需要字节数
	if !strings.Contains(got, `"bytes_len":11`) {
		t.Fatalf("bytes_len missing/wrong: %s", got)
	}
}

func TestHumanizeReadFileBinary(t *testing.T) {
	raw := mustJSON(t, struct {
		Bytes []byte `json:"bytes"`
		EOF   bool   `json:"eof"`
	}{Bytes: []byte{0xff, 0xfe, 0x00, 0x01}, EOF: false})

	got := humanizeToolResult("read_file", raw)
	if !strings.Contains(got, `"binary":true`) {
		t.Fatalf("binary not flagged: %s", got)
	}
	if !strings.Contains(got, `"bytes_len":4`) {
		t.Fatalf("bytes_len missing/wrong: %s", got)
	}
	if !strings.Contains(got, `"size_human":"4 B"`) {
		t.Fatalf("size_human wrong: %s", got)
	}
	if !strings.Contains(got, `"eof":false`) {
		t.Fatalf("eof lost: %s", got)
	}
	if strings.Contains(got, `"text":`) {
		t.Fatalf("binary should not carry text: %s", got)
	}
}

// TestIsTextChunkBoundary 验证多字节字符被 chunk 边界切断时仍判为文本（HIGH 修复）。
func TestIsTextChunkBoundary(t *testing.T) {
	full := []byte("你好世界") // 每字 3 字节，共 12 字节
	// 在第 2 个字 "好"（字节 3..5）中间切：chunk1 末尾留半截、chunk2 开头留半截。
	split := 4
	chunk1, chunk2 := full[:split], full[split:]

	if utf8.Valid(chunk1) || utf8.Valid(chunk2) {
		t.Fatalf("test setup wrong: both chunks should be invalid UTF-8 standalone")
	}
	if !isTextChunk(chunk1) {
		t.Errorf("chunk1 (trailing partial rune) should be text")
	}
	if !isTextChunk(chunk2) {
		t.Errorf("chunk2 (leading partial rune) should be text")
	}
	// read_file 出口对这两块都应给 text + bytes_len，而非 binary。
	for i, c := range [][]byte{chunk1, chunk2} {
		raw := mustJSON(t, struct {
			Bytes []byte `json:"bytes"`
			EOF   bool   `json:"eof"`
		}{Bytes: c, EOF: false})
		got := humanizeToolResult("read_file", raw)
		if strings.Contains(got, `"binary":true`) {
			t.Errorf("chunk%d mislabeled binary: %s", i+1, got)
		}
		if !strings.Contains(got, `"text":`) {
			t.Errorf("chunk%d should carry text: %s", i+1, got)
		}
	}
}

// TestIsTextChunkGenuineBinary 验证真正的二进制（含非法 UTF-8 序列）判 binary。
//
// 设计取舍：isTextChunk 为了不把「跨块切断的多字节文本」误判 binary（本次 HIGH 修复），
// 会容忍开头≤3 个延续字节、结尾不完整字符。代价是：若一段二进制恰好「开头几字节是延续
// 字节、其后全是合法 UTF-8」（如 PNG 头 0x89 后接 ASCII "PNG"），会被判成文本。这种
// 误判只是把内容显示成带 � 的乱码 + 仍给出 bytes_len（无数据丢失），且 read_file
// 不应用来读二进制（那是 download_file 的职责），故有意偏向 text。因此这里只用「真二进制
// 必然命中、且非法字节不在可剥离边缘」的反例。
func TestIsTextChunkGenuineBinary(t *testing.T) {
	cases := [][]byte{
		{0x00, 0x01, 0x02, 0xff, 0xfe},             // 非法序列在中段，无法靠剥边缘救回
		{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10},       // JPEG 头：0xff 是 RuneStart 但非法，不会被剥
		{0x41, 0x42, 0xff, 0x43, 0xff, 0x44, 0x45}, // 非法字节夹在中间
	}
	for i, c := range cases {
		if isTextChunk(c) {
			t.Errorf("case %d: genuine binary should not be text: % x", i, c)
		}
	}
}

func TestHumanizeExecMixed(t *testing.T) {
	raw := mustJSON(t, struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout,omitempty"`
		Stderr   []byte `json:"stderr,omitempty"`
	}{ExitCode: 2, Stdout: []byte("ok output"), Stderr: []byte{0x00, 0xff}})

	got := humanizeToolResult("exec", raw)
	if !strings.Contains(got, `"exit_code":2`) {
		t.Fatalf("exit_code lost: %s", got)
	}
	if !strings.Contains(got, `"stdout":"ok output"`) {
		t.Fatalf("stdout text missing: %s", got)
	}
	if !strings.Contains(got, `"stderr_binary":true`) || !strings.Contains(got, `"stderr_size":2`) {
		t.Fatalf("stderr binary flag missing: %s", got)
	}
	if strings.Contains(got, `"stderr":`) {
		t.Fatalf("binary stderr should not carry raw value: %s", got)
	}
}

func TestHumanizeExecEmptyStreams(t *testing.T) {
	// stdout/stderr 都空（omitempty 后字段缺失）：只保留 exit_code，不应崩。
	raw := json.RawMessage(`{"exit_code":0}`)
	got := humanizeToolResult("exec", raw)
	if !strings.Contains(got, `"exit_code":0`) {
		t.Fatalf("exit_code lost: %s", got)
	}
	if strings.Contains(got, "stdout") || strings.Contains(got, "stderr") {
		t.Fatalf("empty streams should be omitted: %s", got)
	}
}

func TestHumanizePassthroughOtherTools(t *testing.T) {
	raw := json.RawMessage(`{"entries":[{"name":"a","kind":"file"}]}`)
	if got := humanizeToolResult("list_dir", raw); got != string(raw) {
		t.Fatalf("list_dir should pass through unchanged: %s", got)
	}
}

func TestHumanizeMalformedPassthrough(t *testing.T) {
	// read_file 名字但结果不可解析：原样返回，绝不丢数据。
	raw := json.RawMessage(`not-json`)
	if got := humanizeToolResult("read_file", raw); got != string(raw) {
		t.Fatalf("malformed should pass through: %s", got)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int]string{
		0:               "0 B",
		512:             "512 B",
		1024:            "1.0 KB",
		1536:            "1.5 KB",
		1024 * 1024:     "1.0 MB",
		3 * 1024 * 1024: "3.0 MB",
	}
	for n, want := range cases {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
