package agent

import (
	"reflect"
	"strings"
	"testing"
)

// parseWindowsCommandLine 是 CommandLineToArgvW 的参考实现（argv[0] 之后的部分），
// 只用于测试：把 buildWindowsCommandLine 的输出再切回 argv，验证往返相等。
//
// 之所以自己写一份而不是调真的 CommandLineToArgvW：这样 Linux CI 上也能跑，而这个
// bug 的后果（提权后白名单被吞）恰恰是最不容易在 Windows 上被人手动发现的那类。
//
// 与真实实现的唯一差异：不实现引号内 "" -> 一个字面引号 的写法。编码端从不产出这种
// 序列（字面引号一律写作 \"），所以往返测试不受影响。
func parseWindowsCommandLine(cmd string) []string {
	var args []string
	var cur strings.Builder
	inQuotes := false
	started := false
	i := 0
	for i < len(cmd) {
		c := cmd[i]
		switch {
		case !inQuotes && (c == ' ' || c == '\t'):
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
			i++
		case c == '\\':
			n := 0
			for i < len(cmd) && cmd[i] == '\\' {
				n++
				i++
			}
			started = true
			if i < len(cmd) && cmd[i] == '"' {
				// 2n 个 -> n 个字面反斜杠 + 引号起止；2n+1 个 -> n 个 + 一个字面引号
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					inQuotes = !inQuotes
				}
				i++
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
			}
		case c == '"':
			inQuotes = !inQuotes
			started = true
			i++
		default:
			cur.WriteByte(c)
			started = true
			i++
		}
	}
	if started {
		args = append(args, cur.String())
	}
	return args
}

// TestBuildWindowsCommandLineRoundTrip 编码后再解析必须一字不差地拿回原 argv。
func TestBuildWindowsCommandLineRoundTrip(t *testing.T) {
	cases := [][]string{
		{"share", "--elevate"},
		{"--root", `C:\Work Dir`, "--allow-exec", "git,go"},
		{"--root", `C:\dir\`},                    // 结尾反斜杠：不翻倍会把收尾引号转义掉
		{"--root", `C:\a b\`, "--deny-exec", ""}, // 空参数不能凭空消失
		{`say "hi"`},
		{`back\\slash`, `quote"inside`, `\"both\"`},
		{"tab\there", "nl\nhere"},
		{`"`, `\`, `\\`, `\"`},
		{"plain"},
		{},
	}
	for _, want := range cases {
		line := buildWindowsCommandLine(want)
		got := parseWindowsCommandLine(line)
		if len(want) == 0 && len(got) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("往返不一致\n  原始: %q\n  命令行: %s\n  解析回: %q", want, line, got)
		}
	}
}

// TestBuildWindowsCommandLineKeepsFlagsAfterSpacedPath 是这次修复针对的具体事故：
// --root 的值里有空格时，朴素的 strings.Join 会让它后面的 --allow-exec 整个消失。
// 子进程的 flag 解析在第一个非旗标处停止，白名单为空即"放行一切"——提权后护栏反而没了。
func TestBuildWindowsCommandLineKeepsFlagsAfterSpacedPath(t *testing.T) {
	args := []string{"share", "--root", `C:\Work Dir`, "--allow-exec", "git,go", "--elevated-child"}

	naive := parseWindowsCommandLine(strings.Join(args, " "))
	if len(naive) == len(args) {
		t.Fatalf("前提不成立：朴素 Join 居然没有撕开参数，got=%q", naive)
	}

	got := parseWindowsCommandLine(buildWindowsCommandLine(args))
	if !reflect.DeepEqual(got, args) {
		t.Fatalf("提权后 argv 变了\n  want: %q\n  got:  %q", args, got)
	}
}

// TestQuoteWindowsArgLeavesPlainArgsAlone 普通参数不加引号，保持命令行可读。
func TestQuoteWindowsArgLeavesPlainArgsAlone(t *testing.T) {
	for _, s := range []string{"share", "--elevate", "git,go", `C:\Windows\System32`} {
		if got := quoteWindowsArg(s); got != s {
			t.Errorf("quoteWindowsArg(%q) = %q，普通参数不该被改写", s, got)
		}
	}
	if got := quoteWindowsArg(""); got != `""` {
		t.Errorf(`quoteWindowsArg("") = %q，想要 ""`, got)
	}
}
