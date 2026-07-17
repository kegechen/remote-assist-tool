package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// humanize 只做编码转换，不做截断。限量属于 agent 端（read_file 的默认块大小、
// exec 的 max_output_bytes）。在这里二次截断会把调用方的逃生舱悄悄废掉。

// exec：调用方调高 max_output_bytes 拿到的完整输出，不能被 humanize 又截回默认上限。
func TestHumanizeExecDoesNotDefeatMaxOutputBytes(t *testing.T) {
	full := strings.Repeat("A", 200*1024) // agent 在 max_output_bytes=1MiB 下返回的完整输出
	raw, _ := json.Marshal(map[string]any{
		"exit_code":        0,
		"stdout":           []byte(full),
		"stdout_truncated": false,
	})

	var got map[string]any
	if err := json.Unmarshal([]byte(humanizeToolResult("exec", raw)), &got); err != nil {
		t.Fatalf("humanize 输出无法解析: %v", err)
	}
	if s, _ := got["stdout"].(string); len(s) != len(full) {
		t.Errorf("humanize 把 %d 字节截到 %d 字节，max_output_bytes 逃生舱失效", len(full), len(s))
	}
	if _, flagged := got["stdout_truncated"]; flagged {
		t.Error("agent 未截断，humanize 不应凭空标 stdout_truncated")
	}
}

// exec：agent 端确实截断了，humanize 必须把标记透传出去。
func TestHumanizeExecPassesThroughAgentTruncationFlag(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"exit_code":        1,
		"stderr":           []byte("boom"),
		"stderr_truncated": true,
	})
	var got map[string]any
	json.Unmarshal([]byte(humanizeToolResult("exec", raw)), &got)
	if flag, _ := got["stderr_truncated"].(bool); !flag {
		t.Error("agent 端的截断标记必须透传，否则调用方不知道输出不全")
	}
}

// read_file：humanize 不截断 text，bytes_len 必须与 text 长度一致，
// 否则调用方靠 offset += bytes_len 续读会错位、拿不到完整文件。
func TestHumanizeReadFileBytesLenMatchesText(t *testing.T) {
	content := strings.Repeat("B", 128*1024)
	raw, _ := json.Marshal(map[string]any{"bytes": []byte(content), "eof": false})

	var got map[string]any
	if err := json.Unmarshal([]byte(humanizeToolResult("read_file", raw)), &got); err != nil {
		t.Fatalf("humanize 输出无法解析: %v", err)
	}
	text, _ := got["text"].(string)
	bytesLen, _ := got["bytes_len"].(float64)

	if len(text) != len(content) {
		t.Errorf("text 被截断: %d -> %d", len(content), len(text))
	}
	if int(bytesLen) != len(text) {
		t.Errorf("bytes_len=%d 与 text 长度 %d 不一致，续读会错位", int(bytesLen), len(text))
	}
}
