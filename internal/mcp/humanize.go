package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
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

// addStream 把 exec 的某条流写进 out：空流跳过；合法 UTF-8 给文本；真二进制只标注
// <key>_binary + <key>_size + <key>_size_human，不回传原始字节；既非 UTF-8 又不像二进制
// 的，给有损文本并打上 <key>_encoding 警示。
// 不在这里截断：限量已由 agent 端的 max_output_bytes 完成，这里再截会让调用方调高
// max_output_bytes 也拿不到完整输出。
//
// 非 UTF-8 不等于二进制，这是本函数最初(1e393b2)漏掉的一档。那次只考虑了 chunk 边界，
// 认定"exec 是进程完整输出"就沿用了 utf8.Valid；但中文 Windows 的 cmd 吐 CP936 字节，
// 于是整条 stdout 被判 binary、内容一个字节都不回传，调用方唯一的补救是重跑整条命令。
// agent 端现在会按活动代码页先转一道(见 tools/oemtext.go)，能走到这里说明那道也没成，
// 此时宁可给一份标注清楚的有损文本：exec 的输出是文本的概率接近 1，而"内容凭空消失"
// 是所有结局里最坏的一种——调用方连自己丢了什么都不知道。
func addStream(out map[string]any, key string, data []byte) {
	if len(data) == 0 {
		return
	}
	if utf8.Valid(data) {
		out[key] = string(data)
		return
	}
	if looksBinary(data) {
		out[key+"_binary"] = true
		out[key+"_size"] = len(data)
		out[key+"_size_human"] = humanSize(len(data))
		return
	}
	out[key] = strings.ToValidUTF8(string(data), "�")
	// 字节数必须给：这条路径下非 ASCII 内容基本都没了，不说明原始长度的话调用方既读不到
	// 内容、也不知道自己丢了多少——那正是 binary 分支当初要避免的处境。
	out[key+"_size"] = len(data)
	// 措辞不能只说"非法字节已替换"：DBCS 的字节对里，lead 落在 C2-DF、trail 落在 80-BF 的
	// 那部分(GBK 汉字里约占一成)本身就是结构合法的 UTF-8，ToValidUTF8 不会碰它们，于是解出
	// 一个真实存在但完全无关的字符。调用方无法把它和真输出区分开，必须在这里讲明白。
	out[key+"_encoding"] = "non-utf8: 被协助端的输出编码无法识别(agent 端按活动代码页转码也失败)。" +
		"非法字节已替换为 U+FFFD；少数字节对可能被解成看似正常但错误的字符。" +
		"仅 ASCII 部分可信，需要准确内容请在远端用 UTF-8 重新输出。"
}

// looksBinary 在"已知不是合法 UTF-8"的前提下，判断这段字节更像二进制还是像某个非 UTF-8
// 编码的文本。
//
// 判据是控制字符密度，不是非法 UTF-8 字节的比例——后者区分不了：一段纯中文的 GBK 文本里
// 几乎每个字节都是非法 UTF-8，比例和随机二进制一样接近 100%。而 DBCS 文本的字节都落在
// 0x40 以上(GBK lead 0x81-0xFE、trail 0x40-0xFE)，控制字符只有换行和制表；二进制则遍布
// 0x00-0x1F。NUL 单独提前返回：任何编码的文本输出里都不会有它。
func looksBinary(data []byte) bool {
	ctrl := 0
	for _, b := range data {
		if b == 0 {
			return true
		}
		// ESC 不算：带 ANSI 颜色的命令输出里它很密集(每个着色片段两个)，算进来会把
		// 彩色的非 UTF-8 输出误判成二进制——那恰好是最需要看清内容的一类。
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != 0x1B {
			ctrl++
		}
	}
	// 阈值 1/32(约 3%)：正常文本里除了 \t\n\r\x1b 基本不出现控制字符；二进制轻松超过。
	return ctrl*32 > len(data)
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
