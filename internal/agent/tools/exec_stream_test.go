package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// execHelperEnv 置位时，TestExecStreamHelper 才真正执行。
const execHelperEnv = "REMOTE_ASSIST_EXEC_HELPER"

// TestExecStreamHelper 不是真正的测试：流式测试把测试二进制自己当成"被执行的命令"拉起，
// 让它按固定节奏分阶段吐输出。比起调 sh/powershell，这样两个平台行为完全一致，也不用
// 赌 shell 的缓冲行为——os.Stdout 无缓冲，写下去立刻进管道，正是流式断言需要的确定性。
func TestExecStreamHelper(t *testing.T) {
	if os.Getenv(execHelperEnv) == "" {
		t.Skip("helper process: 仅在被流式测试拉起时执行")
	}
	os.Stdout.WriteString("first\n")
	os.Stderr.WriteString("to-stderr\n")
	time.Sleep(helperPause)
	os.Stdout.WriteString("second\n")
	os.Exit(helperExitCode)
}

const (
	helperPause    = 600 * time.Millisecond
	helperExitCode = 7
)

// execHangHelperEnv 置位时，TestExecHangHelper 才真正执行。
const execHangHelperEnv = "REMOTE_ASSIST_EXEC_HANG_HELPER"

// hangHelperSleep 是"没人取消就会睡满"的时长：远超取消测试的判定阈值，
// 这样"取消生效=秒回" 与 "取消失效=卡住" 能被清楚区分开。
const hangHelperSleep = 5 * time.Second

// TestExecHangHelper 不是真正的测试：取消测试把测试二进制当成一条"先吐一行、再长睡"的
// 命令拉起。若 Send 失败后没取消命令，这个子进程会睡满 hangHelperSleep；取消生效则它被
// 立刻杀掉。用自身当命令，两平台行为一致、也不赌 shell 缓冲。
func TestExecHangHelper(t *testing.T) {
	if os.Getenv(execHangHelperEnv) == "" {
		t.Skip("helper process: 仅在取消测试拉起时执行")
	}
	os.Stdout.WriteString("burst\n")
	time.Sleep(hangHelperSleep)
	os.Exit(0)
}

// failingSink 的 Send 恒失败，模拟物理隧道断开后写不动。
type failingSink struct{}

func (failingSink) Send(stream string, data []byte) error { return errors.New("tunnel gone") }

// TestExecStreamCancelsOnSendFailure 钉住：流式 pump 的 sink.Send 一失败，就必须取消命令，
// 而不是任子进程卡在写满且无人读的管道上、拖到外层 deadline 才收尾。helper 会长睡
// hangHelperSleep，取消生效则 Run 秒回；若哪天有人把 cancel() 删了，这条会卡到超时失败。
func TestExecStreamCancelsOnSendFailure(t *testing.T) {
	args, err := json.Marshal(map[string]any{
		"argv":   []string{os.Args[0], "-test.run=TestExecHangHelper"},
		"env":    map[string]string{execHangHelperEnv: "1"},
		"stream": true,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		NewExec(nil).Run(context.Background(), args, failingSink{})
		close(done)
	}()
	guard := hangHelperSleep - 2*time.Second
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed >= guard {
			t.Fatalf("Run 耗时 %v：Send 失败后没有立刻取消命令", elapsed)
		}
	case <-time.After(guard):
		t.Fatalf("Send 失败后未取消命令：Run 卡了 >= %v", guard)
	}
}

// helperArgv 重新执行测试二进制、只跑 helper 那条用例。
func helperArgv() []string {
	return []string{os.Args[0], "-test.run=TestExecStreamHelper"}
}

func helperArgs(t *testing.T, stream bool) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"argv":   helperArgv(),
		"env":    map[string]string{execHelperEnv: "1"},
		"stream": stream,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return raw
}

// timedSink 记录每块的到达时刻与内容，供断言顺序与"边跑边推"。
type timedSink struct {
	mu     sync.Mutex
	stdout string
	stderr string
	firstA time.Time // 第一块到达时刻
}

func (s *timedSink) Send(stream string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstA.IsZero() {
		s.firstA = time.Now()
	}
	switch stream {
	case "stdout":
		s.stdout += string(data)
	case "stderr":
		s.stderr += string(data)
	}
	return nil
}

// TestExecStreamDeliversWhileRunning 守住流式终端的核心承诺：输出边跑边到，而不是攒到
// 命令结束一次性倒出来。helper 在中途睡 600ms，所以第一块必须显著早于 Run 返回；若哪天
// 有人把实现改回"先收集后发送"，这条会失败——只断言"最终收到了内容"是抓不住的。
func TestExecStreamDeliversWhileRunning(t *testing.T) {
	sink := &timedSink{}
	start := time.Now()
	if _, err := NewExec(nil).Run(context.Background(), helperArgs(t, true), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	finished := time.Since(start)

	sink.mu.Lock()
	firstAt := sink.firstA
	stdout, stderr := sink.stdout, sink.stderr
	sink.mu.Unlock()

	if firstAt.IsZero() {
		t.Fatal("没有收到任何流式块")
	}
	lead := finished - firstAt.Sub(start)
	if lead < helperPause/2 {
		t.Fatalf("第一块只比命令结束早 %v（应 >= %v）：看起来是攒完才发的", lead, helperPause/2)
	}
	if stdout != "first\nsecond\n" {
		t.Fatalf("stdout=%q，想要 \"first\\nsecond\\n\"（顺序错或有丢块）", stdout)
	}
	if stderr != "to-stderr\n" {
		t.Fatalf("stderr=%q", stderr)
	}
}

// TestExecStreamResultCarriesOnlyExitStatus 钉住流式模式的契约：输出已经逐块送走，
// result 不再重复带 stdout/stderr，只报退出状态。前端据此不重复渲染一遍输出。
func TestExecStreamResultCarriesOnlyExitStatus(t *testing.T) {
	out, err := NewExec(nil).Run(context.Background(), helperArgs(t, true), &timedSink{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.ExitCode != helperExitCode {
		t.Fatalf("exit_code=%d，想要 %d", r.ExitCode, helperExitCode)
	}
	if len(r.Stdout) != 0 || len(r.Stderr) != 0 {
		t.Fatalf("流式结果不应重复带输出，got stdout=%q stderr=%q", r.Stdout, r.Stderr)
	}
}

// TestExecStreamOffKeepsBufferedResult 流式是显式选入的：不传 stream 时行为一字不变
// （输出仍在 result 里），Claude 那条路径不受影响。
func TestExecStreamOffKeepsBufferedResult(t *testing.T) {
	sink := &timedSink{}
	out, err := NewExec(nil).Run(context.Background(), helperArgs(t, false), sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	json.Unmarshal(out, &r)
	if string(r.Stdout) != "first\nsecond\n" {
		t.Fatalf("stdout=%q", r.Stdout)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.firstA.IsZero() {
		t.Fatal("没要求流式却往 sink 推了块")
	}
}

// TestExecStreamStartFailureReportsReason 命令根本没启动起来时（可执行文件不存在），
// 流式路径也必须把原因带出来，而不是只给一个 exit -1 让调用方猜。
func TestExecStreamStartFailureReportsReason(t *testing.T) {
	args, _ := json.Marshal(map[string]any{
		"argv":   []string{"definitely-not-a-real-binary-xyz"},
		"stream": true,
	})
	out, err := NewExec(nil).Run(context.Background(), args, &timedSink{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	json.Unmarshal(out, &r)
	if r.ExitCode != -1 {
		t.Fatalf("exit_code=%d，想要 -1", r.ExitCode)
	}
	if r.Error == "" {
		t.Fatal("命令没能启动，error 字段却是空的")
	}
}
