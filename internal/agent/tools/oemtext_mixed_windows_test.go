//go:build windows

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestDecodingWriterMixedUTF8ThenGBKSameWrite 同一次 Write 里前半是 UTF-8、后半是代码页字节。
//
// 这是 code review 揪出的高危缺口：scanUTF8 已经算出了合法 UTF-8 前缀的长度，但 ok=false
// 时那个长度被丢掉，整段(含已证明合法的前缀)一起送去 DBCS 配对。后果不止是前缀被转成乱码
// ——前缀的字节会让后面的 lead/trail 配对整体错位，连本来能正确解码的 GBK 部分也一起毁掉：
//
//	"中"(E4 B8 AD) + GBK"正在"(D5 FD D4 DA)
//	→ 配对成 (E4 B8)(AD D5)(FD D4) 余 DA，七个字节没有一个落在原来的字符边界上。
//
// 触发场景不罕见：cmd /c "git log -1 --pretty=%s & ver" 这类把 UTF-8 工具和内置命令串起来
// 的命令，两段输出常常落在同一次管道读里。
func TestDecodingWriterMixedUTF8ThenGBKSameWrite(t *testing.T) {
	requireGBK(t)
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)

	mixed := append([]byte("中"), gbkZhengZai...) // UTF-8"中" + GBK"正在 Ping"
	if _, err := d.Write(mixed); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	const want = "中正在 Ping"
	if got := buf.String(); got != want {
		t.Errorf("混合编码被毁掉了:\n got=%q\nwant=%q", got, want)
	}
}

// TestDecodingWriterGBKThenUTF8AcrossWrites 先 GBK 后 UTF-8，分两次写。
//
// code review 的第 5 条：transcode 一旦置位就永不回头，理由写的是"一条流的编码不会中途变"，
// 但 cmd /c "a & b" 恰恰是产生 GBK 输出最常见的方式，两条子命令完全可以一个吐 GBK 一个吐
// UTF-8。锁死之后后半段的 UTF-8 中文会被当 GBK 解，变成静默错误的文本——比修复前判 binary
// 更糟，因为调用方连"这里不对"的信号都没有。
func TestDecodingWriterGBKThenUTF8AcrossWrites(t *testing.T) {
	requireGBK(t)
	var buf bytes.Buffer
	d := newDecodingWriter(&buf)

	if _, err := d.Write(gbkZhengZai); err != nil {
		t.Fatalf("Write GBK: %v", err)
	}
	if _, err := d.Write([]byte("\n提交说明")); err != nil { // 后半段是 UTF-8
		t.Fatalf("Write UTF-8: %v", err)
	}
	if err := d.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	const want = "正在 Ping\n提交说明"
	if got := buf.String(); got != want {
		t.Errorf("锁死在代码页模式，后半段 UTF-8 被误解码:\n got=%q\nwant=%q", got, want)
	}
}

// TestExecMixedEncodingEndToEnd 让 helper 进程真的往管道里写一段 UTF-8 + GBK 混合字节，
// 走完整的 ExecTool 路径，确认修复在端到端也成立而不只是在单元测试的构造数据上成立。
func TestExecMixedEncodingEndToEnd(t *testing.T) {
	requireGBK(t)
	raw, err := NewExec(nil).Run(context.Background(), mustExecArgs(t, ExecArgs{
		Argv: treeHelperArgv(),
		Env:  treeHelperEnv("mixed-encoding"),
	}), nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	var res ExecResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果解析失败: %v", err)
	}
	out := string(res.Stdout)
	if !utf8.Valid(res.Stdout) {
		t.Fatalf("stdout 不是合法 UTF-8: % x", res.Stdout)
	}
	if !strings.Contains(out, "中正在") {
		t.Errorf("混合编码没还原出来:\n got=%q\nwant 含 %q", out, "中正在")
	}
}
