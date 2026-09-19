package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// gbkPingOutput 是中文 Windows 上 `ping` 的典型开头 "正在 Ping"，按 CP936(GBK) 编码。
// 正=D5FD 在=D4DA，随后是 ASCII " Ping"。这串不是合法 UTF-8：D5 起了一个 2 字节序列，
// 而后继的 FD 不是 10xxxxxx 续字节。
var gbkPingOutput = []byte{0xD5, 0xFD, 0xD4, 0xDA, 0x20, 0x50, 0x69, 0x6E, 0x67}

// TestHumanizeExecNonUTF8StdoutKeepsContent 锁定：exec 的 stdout 是非 UTF-8 文本时，
// MCP 出口不得把内容整个丢掉。
//
// 这是 1e393b2 留下的缺口——那次提交认定 "exec 的 stdout/stderr 是进程完整输出(非分块)，
// 沿用 utf8.Valid 不变"，只考虑了 chunk 边界切断，没考虑被协助端的活动代码页不是 UTF-8。
// 中文 Windows 上 cmd 内置命令吐的是 GBK，于是整条 stdout 被标成 binary、内容不回传，
// 调用方唯一的补救是把整条命令重跑一遍。
//
// read_file 判 binary 后丢内容是对的(真会读到 exe)；exec 的 stdout 不是——它是文本的
// 概率接近 1，丢掉的代价远大于给一份带标记的有损文本。
func TestHumanizeExecNonUTF8StdoutKeepsContent(t *testing.T) {
	raw := mustJSON(t, struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout,omitempty"`
	}{ExitCode: 0, Stdout: gbkPingOutput})

	got := humanizeToolResult("exec", raw)

	// ASCII 部分至少要活下来：调用方据此能认出这是文本、是哪条命令的输出。
	if !strings.Contains(got, "Ping") {
		t.Errorf("非 UTF-8 stdout 的可读部分被丢弃了，调用方只能重跑命令: %s", got)
	}
	// 不该再宣称这是二进制——它是文本，只是编码不是 UTF-8。
	if strings.Contains(got, `"stdout_binary":true`) {
		t.Errorf("非 UTF-8 文本被误标为 binary: %s", got)
	}
	// 必须有明确的编码警示，否则调用方会把乱码字当成命令的真实输出。
	if !strings.Contains(got, "stdout_encoding") {
		t.Errorf("缺少编码警示标记，调用方无从知道这段文本是有损的: %s", got)
	}
	// 也要给字节数：这条路径下非 ASCII 内容基本都没了，不说原始长度的话调用方既读不到
	// 内容、也不知道丢了多少——那正是 binary 分支当初要避免的处境。
	if !strings.Contains(got, `"stdout_size":9`) {
		t.Errorf("有损文本缺少 stdout_size，调用方不知道丢了多少: %s", got)
	}
}

// TestHumanizeExecLossyWarnsAboutFabricatedChars 有损分支的警示措辞必须提到"可能解出错误
// 但看似正常的字符"，不能只说"非法字节已替换为 U+FFFD"。
//
// 原因是 strings.ToValidUTF8 只动**非法**序列：GBK 里 lead 落在 C2-DF、trail 落在 80-BF
// 的字节对本身就是结构合法的 UTF-8，它不会碰，于是解出一个真实存在却毫不相干的字符。
// 调用方分辨不出哪个字是真的，警示不说清楚就等于没说。
func TestHumanizeExecLossyWarnsAboutFabricatedChars(t *testing.T) {
	// GBK "正在测试中文"：其中含有会被解成合法 UTF-8 的字节对。
	gbk := []byte{0xD5, 0xFD, 0xD4, 0xDA, 0xB2, 0xE2, 0xCA, 0xD4, 0xD6, 0xD0, 0xCE, 0xC4}
	raw := mustJSON(t, struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout,omitempty"`
	}{ExitCode: 0, Stdout: gbk})

	got := humanizeToolResult("exec", raw)
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, got)
	}
	warn, _ := out["stdout_encoding"].(string)
	if !strings.Contains(warn, "错误") {
		t.Errorf("警示没提到可能解出错误字符，调用方会把伪造的字当真: %q", warn)
	}
	if out["stdout_size"] == nil {
		t.Errorf("缺少 stdout_size: %s", got)
	}
}

// TestHumanizeExecGenuineBinaryStillFlagged 反向保护：真二进制(NUL + 非法序列)仍应判
// binary 并且不回传原始字节。放宽非 UTF-8 的处理不能顺手把这条也放宽了。
func TestHumanizeExecGenuineBinaryStillFlagged(t *testing.T) {
	raw := mustJSON(t, struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout,omitempty"`
	}{ExitCode: 0, Stdout: []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00}})

	got := humanizeToolResult("exec", raw)
	if !strings.Contains(got, `"stdout_binary":true`) {
		t.Errorf("真二进制应仍标 binary: %s", got)
	}
	if strings.Contains(got, `"stdout":`) {
		t.Errorf("二进制不应回传内容: %s", got)
	}
}

// TestHumanizeExecValidUTF8Untouched 回归保护：合法 UTF-8(含中文)必须原样走文本路径，
// 不因为新增的非 UTF-8 分支而被改写或被打上编码标记。
func TestHumanizeExecValidUTF8Untouched(t *testing.T) {
	raw := mustJSON(t, struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout,omitempty"`
	}{ExitCode: 0, Stdout: []byte("正在 Ping 中文输出")})

	got := humanizeToolResult("exec", raw)
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, got)
	}
	if out["stdout"] != "正在 Ping 中文输出" {
		t.Errorf("合法 UTF-8 被改写了: %#v", out["stdout"])
	}
	if _, marked := out["stdout_encoding"]; marked {
		t.Errorf("合法 UTF-8 不该带编码警示: %s", got)
	}
}
