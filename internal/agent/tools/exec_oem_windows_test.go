//go:build windows

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestExecGBKCommandEndToEnd 端到端跑一条真会吐 GBK 的命令，验证 ExecTool 返回的 stdout
// 已经是合法 UTF-8 的中文，而不是被下游判成 binary 的字节堆。
//
// 这是整条修复链路的验收点：用户实际遇到的是"远端 Windows 的第一条输出被判成二进制，
// 只能强制 UTF-8 重跑"。这里用 cmd 的内置命令复现同一条路径——cmd 的内置命令按活动代码页
// 输出，中文 Windows 上就是 CP936。
func TestExecGBKCommandEndToEnd(t *testing.T) {
	if cp := consoleCodePage(); cp != 936 {
		t.Skipf("活动代码页是 %d，不是 936(GBK)，本用例只覆盖中文区域", cp)
	}

	// `cmd /c ver` 在中文 Windows 上输出 "Microsoft Windows [版本 10.0.xxxxx.xxxx]"，
	// 其中 "版本" 两个字走 GBK。选 ver 而不是 dir：输出短、稳定、不依赖当前目录内容。
	e := NewExec(nil)
	raw, err := e.Run(context.Background(), mustExecArgs(t, ExecArgs{
		Argv: []string{"cmd", "/c", "ver"},
	}), nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	var res ExecResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果解析失败: %v", err)
	}
	out := string(res.Stdout)
	t.Logf("stdout = %q", out)

	if !utf8.Valid(res.Stdout) {
		t.Fatalf("stdout 不是合法 UTF-8，转码没生效: % x", res.Stdout)
	}
	// 中文 Windows 的 ver 一定带 "版本" 二字；能匹配上就说明 GBK→UTF-8 转对了，
	// 而不只是"碰巧是合法 UTF-8"(比如整段被丢弃成空串)。
	if !strings.Contains(out, "版本") {
		t.Errorf("期望 stdout 含中文 \"版本\"，实际: %q", out)
	}
}

// TestExecUTF8CommandNotMangled 反向保护：本来就输出 UTF-8 的命令不能被二次转码。
// Windows 上 go / git / node 早就直接吐 UTF-8，若这一层无脑按 CP936 转，正确的中文
// 会被转成乱码——比不转还糟。
func TestExecUTF8CommandNotMangled(t *testing.T) {
	const want = "中文UTF8输出"
	e := NewExec(nil)
	// powershell 显式把输出编码设成 UTF-8，模拟那些自己吐 UTF-8 的现代工具。
	raw, err := e.Run(context.Background(), mustExecArgs(t, ExecArgs{
		Argv: []string{"powershell", "-NoProfile", "-Command",
			"[Console]::OutputEncoding=[Text.UTF8Encoding]::new(); Write-Output '" + want + "'"},
	}), nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	var res ExecResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果解析失败: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != want {
		t.Errorf("UTF-8 输出被改写了: got=%q want=%q", got, want)
	}
}

// runExecForTest 跑一条命令并返回它的 stdout，失败即 t.Fatal。
func runExecForTest(t *testing.T, argv []string) string {
	t.Helper()
	raw, err := NewExec(nil).Run(context.Background(), mustExecArgs(t, ExecArgs{Argv: argv}), nil)
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	var res ExecResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果解析失败: %v", err)
	}
	if !utf8.Valid(res.Stdout) {
		t.Fatalf("stdout 不是合法 UTF-8: % x", res.Stdout)
	}
	return string(res.Stdout)
}

func mustExecArgs(t *testing.T, a ExecArgs) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}
