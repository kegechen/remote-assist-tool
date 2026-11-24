package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/client"
	"github.com/remote-assist/tool/internal/logfile"
	"github.com/remote-assist/tool/internal/version"
)

// 别的测试跑 runHelp 时不往真的临时目录写日志文件；测日志文件的测试自己设。
func init() { os.Setenv(logfile.EnvVar, "off") }

const mcpRequests = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
	`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n" +
	`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"

// runHelpMCP 把 requests 写进 stdin（写完关掉）跑一次 runHelp --mcp-stdio，返回 stdout、stderr。
func runHelpMCP(t *testing.T, requests string, args ...string) (stdout, stderr string) {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdinR, stdoutW, stderrW
	restore := func() { os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr }
	t.Cleanup(func() {
		restore()
		stdinR.Close()
		stdoutR.Close()
		stderrR.Close()
	})
	outCh, errCh := make(chan []byte, 1), make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(stdoutR); outCh <- b }()
	go func() { b, _ := io.ReadAll(stderrR); errCh <- b }()
	if _, err := stdinW.Write([]byte(requests)); err != nil {
		t.Fatal(err)
	}
	stdinW.Close()
	runHelp(append([]string{"--mcp-stdio", "--p2p", "disabled"}, args...))
	restore()
	stdoutW.Close()
	stderrW.Close()
	return string(<-outCh), string(<-errCh)
}

func assertJSONRPCOnly(t *testing.T, stdout string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout 有 %d 行，要 2 个 JSON-RPC 响应: %q", len(lines), stdout)
	}
	for _, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("stdout 里有不是 JSON 的: %q", l)
		}
	}
}

// useTempDir 让 os.TempDir() 指向一个新的测试目录。
func useTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, dir)
	}
	return dir
}

var logPathRE = regexp.MustCompile(`日志文件：(.+?)\r?\n`)

func logPathIn(t *testing.T, stderr string) string {
	t.Helper()
	m := logPathRE.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("stderr 里没有日志文件路径: %q", stderr)
	}
	return m[1]
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sameDir(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

// spec 6.8：help --mcp-stdio 默认写日志文件到临时目录，路径只打到 stderr，stdout 只有协议数据，工具响应的内容不进文件；
// 返回后 log 的输出和 client 的日志文件都还原。
func TestRunHelpMCPLogFile(t *testing.T) {
	tmp := useTempDir(t)
	t.Setenv(logfile.EnvVar, "")
	origLog := log.Writer()
	stdout, stderr := runHelpMCP(t, mcpRequests)
	assertJSONRPCOnly(t, stdout)
	path := logPathIn(t, stderr)
	if !sameDir(filepath.Dir(filepath.Dir(path)), tmp) || !strings.HasPrefix(filepath.Base(filepath.Dir(path)), "remote-assist-") {
		t.Fatalf("日志文件 %s 不在 %s/remote-assist-<用户>/ 下", path, tmp)
	}
	if want := fmt.Sprintf("help-%d.log", os.Getpid()); filepath.Base(path) != want {
		t.Fatalf("文件名 %s，要 %s", filepath.Base(path), want)
	}
	data := readText(t, path)
	for _, want := range []string{
		fmt.Sprintf("remote-assist %s（help，pid %d）\n", version.Info(), os.Getpid()),
		"MCP stdio 模式（bootstrap）",
	} {
		if !strings.Contains(data, want) {
			t.Fatalf("日志文件里没有 %q:\n%s", want, data)
		}
	}
	for _, leak := range []string{"jsonrpc", "read_file", "protocolVersion"} {
		if strings.Contains(data, leak) {
			t.Fatalf("协议数据进了日志文件（%q）:\n%s", leak, data)
		}
	}
	if log.Writer() != origLog {
		t.Fatal("runHelp 返回后要还原 log 的输出")
	}
	var term strings.Builder
	if client.StatusTee(&term) != io.Writer(&term) {
		t.Fatal("runHelp 返回后要清掉 client 的日志文件")
	}
}

func TestRunHelpLogFileOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
	}{
		{"命令行", "", []string{"--log-file", "off"}},
		{"环境变量", "off", nil},
		{"环境变量不分大小写", "OFF", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir()) // 万一 off 没认出来被当成相对路径，别写进源码目录
			tmp := useTempDir(t)
			t.Setenv(logfile.EnvVar, tc.env)
			stdout, stderr := runHelpMCP(t, mcpRequests, tc.args...)
			assertJSONRPCOnly(t, stdout)
			if strings.Contains(stderr, "日志文件") {
				t.Fatalf("关掉了还打日志文件路径: %q", stderr)
			}
			if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
				t.Fatalf("关掉了还建了 %v", entries)
			}
		})
	}
}

// 命令行优先于环境变量；环境变量也可以写路径。
func TestRunHelpLogFilePath(t *testing.T) {
	useTempDir(t)
	t.Setenv(logfile.EnvVar, "off")
	path := filepath.Join(t.TempDir(), "help.log")
	stdout, stderr := runHelpMCP(t, mcpRequests, "--log-file", path)
	assertJSONRPCOnly(t, stdout)
	if got := logPathIn(t, stderr); got != path {
		t.Fatalf("日志文件 %s，要 %s", got, path)
	}
	if !strings.Contains(readText(t, path), "（help，pid ") {
		t.Fatalf("文件里没有版本行: %q", readText(t, path))
	}

	path2 := filepath.Join(t.TempDir(), "env.log")
	t.Setenv(logfile.EnvVar, path2)
	_, stderr = runHelpMCP(t, mcpRequests)
	if got := logPathIn(t, stderr); got != path2 {
		t.Fatalf("日志文件 %s，要环境变量写的 %s", got, path2)
	}
}

// 打不开（这里路径是个目录）只警告一次，照常运行。
func TestRunHelpLogFileOpenFails(t *testing.T) {
	useTempDir(t)
	stdout, stderr := runHelpMCP(t, mcpRequests, "--log-file", t.TempDir())
	assertJSONRPCOnly(t, stdout)
	if n := strings.Count(stderr, "[日志]"); n != 1 || !strings.Contains(stderr, "不写日志文件") {
		t.Fatalf("要警告一次不写日志文件: %q", stderr)
	}
	if strings.Contains(stderr, "日志文件：") {
		t.Fatalf("打不开还打了路径: %q", stderr)
	}
}

// help 异常结束的原因也写进日志文件（里面的协助码换掉），随后 os.Exit 不跑 defer，所以写完就关掉。
func TestReportHelpFailureToLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "help.log")
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	v := logFileFlag(fs)
	if err := fs.Parse([]string{"--log-file", path}); err != nil {
		t.Fatal(err)
	}
	origLog := log.Writer()
	term := captureStderr(t, func() {
		closeLog := openLogFile(fs, *v, "help")
		client.NewHelpModeMCP(&client.Config{}, "M7QX-KZV3-WJTA")
		reportHelpFailure(fmt.Errorf("加入 M7QXKZV3WJTA 失败: EOF"), true, closeLog)
	})
	if !strings.Contains(term, "连接中断: 加入 M7QXKZV3WJTA 失败: EOF") {
		t.Fatalf("终端上要有失败原因: %q", term)
	}
	if log.Writer() != origLog {
		t.Fatal("要关掉日志文件、还原 log 的输出")
	}
	data := readText(t, path)
	if !strings.Contains(data, "连接中断: 加入 ******** 失败: EOF") || !strings.Contains(data, "重新调用 connect") || strings.Contains(data, "KZV3") {
		t.Fatalf("日志文件内容:\n%s", data)
	}
}

// help 显式模式连私网 relay 时跳过 P2P 的提示也写进日志文件。
func TestHelpConfigureNoticeToLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "help.log")
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	v := logFileFlag(fs)
	if err := fs.Parse([]string{"--log-file", path}); err != nil {
		t.Fatal(err)
	}
	cfg := &client.Config{ServerAddr: "127.0.0.1:8443", UseTLS: true, P2PMode: "auto"}
	captureStderr(t, func() {
		closeLog := openLogFile(fs, *v, "help")
		defer closeLog()
		helpRelay{}.configure(cfg)
	})
	if cfg.P2PMode != "disabled" || !strings.Contains(readText(t, path), "跳过 P2P") {
		t.Fatalf("P2P = %q，日志文件:\n%s", cfg.P2PMode, readText(t, path))
	}
}

// 通道内升级出来的 share 没有可见的控制台：升级隔离结束那一行要写进日志文件。
func TestAnnounceRestoredUpgradeHomeToLogFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv(upgradeOrigHomeEnv, string(upgradeEnvPresent)+home)
	t.Setenv(upgradeOrigUserProfileEnv, string(upgradeEnvPresent)+home)
	path := filepath.Join(t.TempDir(), "share.log")
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	v := logFileFlag(fs)
	if err := fs.Parse([]string{"--log-file", path}); err != nil {
		t.Fatal(err)
	}
	term := captureStderr(t, func() {
		closeLog := openLogFile(fs, *v, "share")
		defer closeLog()
		announceRestoredUpgradeHome()
	})
	if !strings.Contains(term, "升级隔离结束") || !strings.Contains(readText(t, path), "升级隔离结束：已还原 HOME/USERPROFILE") {
		t.Fatalf("终端 %q，日志文件:\n%s", term, readText(t, path))
	}
}

// share 也用 openLogFile：log 包的输出、client 的状态输出都写进去，第一行是版本行；关掉后都还原。
func TestOpenLogFileShare(t *testing.T) {
	useTempDir(t)
	t.Setenv(logfile.EnvVar, "")
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	v := logFileFlag(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	origLog := log.Writer()
	var closeLog func()
	stderr := captureStderr(t, func() { closeLog = openLogFile(fs, *v, "share") })
	path := logPathIn(t, stderr)
	if want := fmt.Sprintf("share-%d.log", os.Getpid()); filepath.Base(path) != want {
		t.Fatalf("文件名 %s，要 %s", filepath.Base(path), want)
	}
	log.Printf("来自 log 的一行")
	fmt.Fprintln(client.StatusTee(io.Discard), "状态一行")
	closeLog()
	if log.Writer() != origLog {
		t.Fatal("关掉后要还原 log 的输出")
	}
	data := readText(t, path)
	lines := strings.Split(strings.TrimSpace(data), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], fmt.Sprintf("remote-assist %s（share，pid %d）", version.Info(), os.Getpid())) ||
		!strings.HasSuffix(lines[1], " 来自 log 的一行") || !strings.HasSuffix(lines[2], " 状态一行") {
		t.Fatalf("日志文件内容:\n%s", data)
	}
}
