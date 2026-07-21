package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// humanizeToolResult 把工具返回的原始 JSON 转成对 Claude 更友好的可见文本。
//
// 背景：read_file / exec 的内容字段在 Go 端是 []byte，Go 的 encoding/json 默认把
// []byte 编码成 base64 字符串，于是 Claude 在 MCP content 里看到的是不可读的 base64。
// 这里在面向 Claude 的唯一出口（MCP server 的 tools/call 响应）做一次后处理：
//   - 合法 UTF-8  → 直接给文本（text 字段）
//   - 二进制      → 只标注 binary + 字节数 + 人类可读大小，不再把无意义的 base64 大块
//                   塞给 Claude（顺带省 token / 带宽）
//
// 这一层只做编码转换，不做截断。限量一律在源头（agent 端）完成：read_file 靠默认块
// 大小限量以保证 bytes_len/eof 自洽可续读，exec 靠 max_output_bytes 限量。在这里二次
// 截断会让 bytes_len 与 text 对不上、并把 max_output_bytes 这个逃生舱悄悄废掉。
//
// 仅对 read_file / exec 生效（只有它们的结果含 []byte 字段）；其余工具及任何无法解析的
// 结果一律原样返回，保证零数据丢失、不改变既有契约。
//
// 开销：一次 json.Unmarshal + 一次 utf8.Valid（O(n) 单遍）+ 一次 json.Marshal，相对
// 磁盘 / 隧道 IO 可忽略，不影响 MCP 效率；二进制场景反而更省（不回传 base64）。
func humanizeToolResult(name string, result json.RawMessage) string {
	switch name {
	case "read_file":
		if s, ok := humanizeReadFile(result); ok {
			return s
		}
	case "exec":
		if s, ok := humanizeExec(result); ok {
			return s
		}
	}
	return string(result)
}

func humanizeReadFile(result json.RawMessage) (string, bool) {
	var r struct {
		Bytes  []byte `json:"bytes"`
		EOF    bool   `json:"eof"`
		AsText bool   `json:"as_text"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return "", false
	}
	// bytes_len 始终回传：read_file 是分块协议（单次最多 1 MiB，靠 offset 续读），
	// 把 base64 的 bytes 换成 text 后调用方就无法再从结果推算本块字节数来定下一个
	// offset，必须显式给出，否则大文件分块读会错位。
	out := map[string]any{"eof": r.EOF, "bytes_len": len(r.Bytes)}
	if r.AsText {
		// The GUI requests this only after a user explicitly opts into a text
		// preview. Keep the bytes so its browser decoder can handle GBK/UTF-16.
		out["bytes_b64"] = base64.StdEncoding.EncodeToString(r.Bytes)
	} else if isTextChunk(r.Bytes) {
		out["text"] = string(r.Bytes)
	} else {
		out["binary"] = true
		out["size_human"] = humanSize(len(r.Bytes))
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// isTextChunk 判断 read_file 的一块数据是否人类可读文本。它在 utf8.Valid 之上额外
// 容忍「块边界把一个多字节 UTF-8 字符切断」——当文本文件大于单次读取上限被分块读取
// 时，某个字符会被切成两半，前一块末尾留半截、后一块开头留半截。若不容忍，这种本是
// 文本的块会因为 1~3 字节的残片而被误判 binary，从而把可读内容藏起来（read_file
// 在本仓库常用于读中文源码/日志，多字节字符很常见）。
//
// 处理：先剥掉开头可能的连续延续字节（被前一块切走的字符尾），再剥掉结尾可能的不完整
// 字符（被后一块切走的字符头），剩余部分若是合法 UTF-8 即判为文本；中间存在非法字节
// （真正的二进制）则仍判 binary。开销 O(n) 单遍，不影响效率。
func isTextChunk(data []byte) bool {
	if len(data) == 0 || utf8.Valid(data) {
		return true
	}
	lo, hi := 0, len(data)
	// 剥掉开头最多 3 个延续字节（10xxxxxx）：被上一块切走的字符尾。
	for lo < hi && lo < 3 && !utf8.RuneStart(data[lo]) {
		lo++
	}
	// 结尾若是被切断的不完整字符（最后一个字符起点在末 3 字节内且解码失败），剥掉它。
	for i := 1; i <= 3 && hi-i >= lo; i++ {
		if utf8.RuneStart(data[hi-i]) {
			if r, _ := utf8.DecodeRune(data[hi-i : hi]); r == utf8.RuneError {
				hi -= i
			}
			break
		}
	}
	return utf8.Valid(data[lo:hi])
}

func humanizeExec(result json.RawMessage) (string, bool) {
	var r struct {
		ExitCode        int    `json:"exit_code"`
		Stdout          []byte `json:"stdout"`
		Stderr          []byte `json:"stderr"`
		StdoutTruncated bool   `json:"stdout_truncated"`
		StderrTruncated bool   `json:"stderr_truncated"`
		Error           string `json:"error"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return "", false
	}
	out := map[string]any{"exit_code": r.ExitCode}
	addStream(out, "stdout", r.Stdout)
	addStream(out, "stderr", r.Stderr)
	// 透传 agent 端的截断标记（agent 截断后 humanize 不会二次截断，但标记必须保留）
	if r.StdoutTruncated {
		out["stdout_truncated"] = true
	}
	if r.StderrTruncated {
		out["stderr_truncated"] = true
	}
	// 命令没能启动的原因：不透传的话调用方只看到 exit -1，无从判断
	if r.Error != "" {
		out["error"] = r.Error
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// addStream 把 exec 的某条流写进 out：空流跳过；合法 UTF-8 给文本；二进制只标注
// <key>_binary + <key>_size + <key>_size_human，不回传原始字节。
// 不在这里截断：限量已由 agent 端的 max_output_bytes 完成，这里再截会让调用方调高
// max_output_bytes 也拿不到完整输出。
func addStream(out map[string]any, key string, data []byte) {
	if len(data) == 0 {
		return
	}
	if utf8.Valid(data) {
		out[key] = string(data)
		return
	}
	out[key+"_binary"] = true
	out[key+"_size"] = len(data)
	out[key+"_size_human"] = humanSize(len(data))
}

// humanSize 把字节数格式化为人类可读（B / KB / MB / GB ...，1024 进制）。
func humanSize(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := int64(n) / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
