package client

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/logfile"
	"github.com/remote-assist/tool/internal/proto"
)

// fakeLog 记下写进来的状态输出和登记的协助码。
type fakeLog struct {
	mu     sync.Mutex
	status strings.Builder
	codes  []string
}

func (f *fakeLog) Status() io.Writer { return fakeLogStatus{f} }

func (f *fakeLog) Redact(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = append(f.codes, code)
}

func (f *fakeLog) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status.String()
}

func (f *fakeLog) redacted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.codes)
}

type fakeLogStatus struct{ f *fakeLog }

func (w fakeLogStatus) Write(p []byte) (int, error) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	return w.f.status.Write(p)
}

func useLogFile(t *testing.T, lf LogFile) {
	t.Helper()
	SetLogFile(lf)
	t.Cleanup(func() { SetLogFile(nil) })
}

// swapStderr 把 os.Stderr 换成临时文件，返回读出写进去的内容的函数。
func swapStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = orig
		f.Close()
	})
	return func() string {
		b, _ := os.ReadFile(f.Name())
		return string(b)
	}
}

// assertNoCodeFragments 断言 data 里找不到 code 的任何 4 个字符以上的片段（不分大小写）。
func assertNoCodeFragments(t *testing.T, data, code string) {
	t.Helper()
	up := strings.ToUpper(data)
	for i := 0; i+4 <= len(code); i++ {
		if frag := code[i : i+4]; strings.Contains(up, frag) {
			t.Fatalf("日志文件里有协助码 %s 的片段 %s:\n%s", code, frag, data)
		}
	}
}

// spec 6.8：用已知的协助码跑一遍 share 注册（第二次注册换了码，换码提示里新旧码都有），日志文件里找不到码的任何
// 4 字符以上片段，终端上仍是完整的码。
func TestShareLogFileRedactsCode(t *testing.T) {
	const first, second = "M7QXKZV3WJTA", "M9RBHQ4YPCDN"
	path := filepath.Join(t.TempDir(), "share.log")
	lf, err := logfile.Open(logfile.Options{Setting: path, Mode: "share", Warn: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	useLogFile(t, lf)
	prev := log.Writer()
	log.SetOutput(logfile.Tee(prev, lf))
	t.Cleanup(func() { log.SetOutput(prev) })

	var n atomic.Int32
	f := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message {
		code := first
		if n.Add(1) > 1 {
			code = second
		}
		return regOK(code, "tok", "", nil)(req)
	})
	s, out, _ := nodeShareEnv(t, nil, nil, f.addr)
	reconnect5s(t, s)
	s.client.Close()
	reconnect5s(t, s)
	if s.GetCode() != second {
		t.Fatalf("第二次注册后的码 %q，要 %q", s.GetCode(), second)
	}
	lf.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := string(b)
	for _, code := range []string{first, second} {
		assertNoCodeFragments(t, data, code)
		if !strings.Contains(out.String(), formatCode(code)) {
			t.Fatalf("终端上要有完整的码 %s:\n%s", formatCode(code), out.String())
		}
	}
	for _, want := range []string{"协助码: ********\n", "等待协助端连接", "已变更为 ********"} {
		if !strings.Contains(data, want) {
			t.Fatalf("日志文件里没有 %q:\n%s", want, data)
		}
	}
}

// share 的控制台输出（Print）再写一份到日志文件；同一个窗口里不进 log 的传输汇总也写，进度行不写。
func TestTransferProgressTranscript(t *testing.T) {
	fl := &fakeLog{}
	useLogFile(t, fl)
	var out syncBuf
	var logs []string
	p := newTransferProgress(&out, true, func() int { return 120 })
	p.quietSummaryLogs = true
	p.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	p.Print("状态一行\n")
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/data/a.bin", Start: true, Total: 40})
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/data/a.bin", Offset: 0, Bytes: 20, Total: 40})
	p.Print("中间\n") // 进度行在屏幕上：擦掉、写、重画，重画的进度行不进文件
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/data/a.bin", Offset: 20, Bytes: 20, Total: 40})
	got := fl.text()
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 || lines[0] != "状态一行" || lines[1] != "中间" || !strings.Contains(lines[2], "上传完成 /data/a.bin") {
		t.Fatalf("日志文件里的状态输出 %q", got)
	}
	if strings.Contains(got, "\r") || !strings.Contains(out.String(), "\r") {
		t.Fatalf("进度行只上屏，不进文件: 文件 %q，屏幕 %q", got, out.String())
	}
	if len(logs) != 0 {
		t.Fatalf("同一个窗口里汇总不进 log: %q", logs)
	}

	// 不在同一个窗口时汇总走 log（log 本身也写进日志文件），控制台副本不再写一份。
	fl2 := &fakeLog{}
	useLogFile(t, fl2)
	p2 := newTransferProgress(&out, false, nil)
	logs = nil
	p2.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	p2.Event(agent.TransferEvent{Dir: "upload", Path: "/data/b.bin", Start: true, Total: 10})
	p2.Event(agent.TransferEvent{Dir: "upload", Path: "/data/b.bin", Offset: 0, Bytes: 10, Total: 10})
	if got := fl2.text(); got != "" {
		t.Fatalf("汇总已经进 log 了，控制台副本不再写: %q", got)
	}
	if len(logs) != 2 || !strings.Contains(logs[1], "上传完成 /data/b.bin") {
		t.Fatalf("log 里要有开始和汇总: %q", logs)
	}
}

// help 面向用户的提示（stderr 上的、MCP connect 提示）再写一份到日志文件，传输进度行不写（带被协助机器上的路径）；
// 终端上照旧。
func TestHelpOutputToLogFile(t *testing.T) {
	fl := &fakeLog{}
	useLogFile(t, fl)
	stderr := swapStderr(t)
	helpLogf("找节点 %s", "W")
	fmt.Fprintln((&HelpMCPBootstrap{}).errw(), "MCP: 已连接")
	logTransferProgress("upload", "/remote/secret.bin", 1, 2)
	got := fl.text()
	if got != "找节点 W\nMCP: 已连接\n" {
		t.Fatalf("日志文件里的状态输出 %q", got)
	}
	term := stderr()
	for _, want := range []string{"找节点 W\n", "MCP: 已连接\n", "/remote/secret.bin"} {
		if !strings.Contains(term, want) {
			t.Fatalf("终端上要有 %q: %q", want, term)
		}
	}
	var buf syncBuf
	fmt.Fprintln((&HelpMCPBootstrap{stderr: &buf}).errw(), "测试缓冲")
	if strings.Contains(fl.text(), "测试缓冲") {
		t.Fatal("换成了缓冲的 stderr 不再写日志文件")
	}
}

// help 登记它用的码（--code、MCP connect 给的码），--no-auth 的固定码不登记。
func TestHelpRedactsCode(t *testing.T) {
	fl := &fakeLog{}
	useLogFile(t, fl)
	NewHelpMode(&Config{}, "m7qx-kzv3-wjta", "127.0.0.1:0")
	NewHelpModeMCP(&Config{}, "M9RBHQ4YPCDN")
	NewHelpModeMCP(&Config{}, proto.NoAuthCode)
	NewHelpMode(&Config{}, "NOAUTH", "127.0.0.1:0")
	if got, want := fl.redacted(), []string{"m7qxkzv3wjta", "M9RBHQ4YPCDN"}; !slices.Equal(got, want) {
		t.Fatalf("登记的码 %q，要 %q", got, want)
	}
}

func TestStatusTee(t *testing.T) {
	var term syncBuf
	SetLogFile(nil)
	if w := StatusTee(&term); w != io.Writer(&term) {
		t.Fatal("没有日志文件时就是终端本身")
	}
	redactCode("M7QXKZV3WJTA") // 没有日志文件时什么也不做
	fl := &fakeLog{}
	useLogFile(t, fl)
	fmt.Fprint(StatusTee(&term), "一行\n")
	if term.String() != "一行\n" || fl.text() != "一行\n" {
		t.Fatalf("终端 %q，日志文件 %q", term.String(), fl.text())
	}
}
