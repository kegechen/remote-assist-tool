package agent

import "strings"

// Windows 没有 argv 数组这层抽象：CreateProcess / ShellExecuteW 收到的是一整根命令行
// 字符串，由子进程自己（Go runtime 用的是 CommandLineToArgvW 的同一套规则）再切回 argv。
// 所以"把 os.Args[1:] 用空格 Join 起来"只在所有参数都不含空格时碰巧成立：
//
//	remote share --elevate --root "C:\Work Dir" --allow-exec git,go
//
// 朴素 Join 之后子进程看到的是 ... --root C:\Work Dir --allow-exec git,go，argv 在 Work
// 处被撕开，Dir 成了一个位置参数。Go 的 flag 在第一个非旗标处停止解析，于是后面的
// --allow-exec 被整体吞掉——用户以为拿到"管理员身份 + 收紧的白名单"，实得"管理员身份 +
// 白名单为空（放行一切）"，而且没有任何提示。

// quoteWindowsArg 按 CommandLineToArgvW 的规则把单个参数编码成命令行片段。
//
// 规则要点（见 CommandLineToArgvW 文档）：
//   - 反斜杠只有紧挨着引号时才有转义含义。2n 个反斜杠 = n 个字面反斜杠 + 一个起止引号；
//     2n+1 个 = n 个字面反斜杠 + 一个字面引号。
//   - 因此包引号时，引号前（含结尾处）的连续反斜杠要翻倍，字面引号写成 \"。
//   - 空参数必须显式写成 ""，否则会凭空消失、把后面的参数整体前移。
func quoteWindowsArg(s string) string {
	// 不含分隔符也不含引号的普通参数原样传，保持命令行可读（日志、任务管理器里好认）。
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}

	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			// 先攒着：是否需要翻倍，取决于它后面是不是引号（或字符串结尾）。
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
			slashes = 0
			b.WriteByte('"')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteByte(c)
		}
	}
	// 结尾的反斜杠紧挨着收尾引号，同样要翻倍，否则 C:\dir\ 会把收尾引号转义掉。
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}

// buildWindowsCommandLine 把一组参数拼成可安全传给 ShellExecuteW lpParameters 的字符串。
// 保证子进程 CommandLineToArgvW 解析回来的 argv 与传入的 args 逐个相等。
func buildWindowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quoteWindowsArg(a)
	}
	return strings.Join(quoted, " ")
}
