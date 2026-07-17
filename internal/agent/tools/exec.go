package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

type ExecArgs struct {
	Argv          []string          `json:"argv"`
	Cwd           string            `json:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	TimeoutMs     uint32            `json:"timeout_ms,omitempty"`
	MaxOutputBytes int              `json:"max_output_bytes,omitempty"`
	Stream        bool              `json:"stream,omitempty"`
	StdinBytes    []byte            `json:"stdin,omitempty"`
}

type ExecResult struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          []byte `json:"stdout,omitempty"`
	Stderr          []byte `json:"stderr,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	// Error 是命令“没能启动”的原因（可执行文件找不到、权限拒绝、cwd 不存在等）。
	// 这类失败下 exit_code=-1、stderr 为空，若不单独暴露，调用方只看到 exit -1、
	// 完全不知道为什么——终端里敲 pwd/ls 就是栽在这。命令正常跑完（含非零退出）时为空。
	Error string `json:"error,omitempty"`
}

const execDefaultMaxOutput = 32 * 1024 // 32 KiB per stream

// ExecTool 通过 argv 列表（不经过 shell）执行命令
type ExecTool struct{ sb *agent.Sandbox }

func NewExec(sb *agent.Sandbox) *ExecTool { return &ExecTool{sb: sb} }
func (e *ExecTool) Name() string          { return "exec" }

const defaultExecTimeout = 5 * time.Minute

func (e *ExecTool) Run(ctx context.Context, raw json.RawMessage, sink agent.StreamSink) (json.RawMessage, error) {
	var a ExecArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if len(a.Argv) == 0 {
		return nil, fmt.Errorf("argv required")
	}
	if e.sb != nil {
		if err := e.sb.CheckExec(a.Argv); err != nil {
			return nil, err
		}
	}
	timeout := defaultExecTimeout
	if a.TimeoutMs > 0 {
		timeout = time.Duration(a.TimeoutMs) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, a.Argv[0], a.Argv[1:]...)
	if a.Cwd != "" {
		cmd.Dir = a.Cwd
	}
	if len(a.Env) > 0 {
		envv := append([]string{}, os.Environ()...)
		for k, v := range a.Env {
			envv = append(envv, k+"="+v)
		}
		cmd.Env = envv
	}
	if len(a.StdinBytes) > 0 {
		cmd.Stdin = bytes.NewReader(a.StdinBytes)
	}

	// v1: stream 模式不支持，始终同步收集
	out, err := cmd.Output()
	stderr := capturedStderr(err)
	exitCode := exitCodeOf(err)
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("deadline_exceeded: exec timed out")
	}

	// 命令没能启动（找不到可执行文件、权限拒绝、cwd 不存在等）时 err 不是 *exec.ExitError，
	// stderr 为空、exitCode=-1。必须把原因单独带出去，否则调用方只看到 exit -1。
	var startErr string
	if err != nil {
		if _, isExit := err.(*exec.ExitError); !isExit {
			startErr = err.Error()
		}
	}

	maxOut := execDefaultMaxOutput
	if a.MaxOutputBytes > 0 {
		maxOut = a.MaxOutputBytes
	}
	stdout, stdoutTrunc := truncateStream(out, maxOut)
	stderrB, stderrTrunc := truncateStream(stderr, maxOut)
	return json.Marshal(ExecResult{
		ExitCode:        exitCode,
		Stdout:          stdout,
		Stderr:          stderrB,
		StdoutTruncated: stdoutTrunc,
		StderrTruncated: stderrTrunc,
		Error:           startErr,
	})
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

func capturedStderr(err error) []byte {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.Stderr
	}
	return nil
}

// truncateStream 截断 exec 的一条输出流：保留头尾、省略中间。
// 编译错误、panic 堆栈、测试失败摘要几乎总在输出末尾，只保开头会把定位问题最需要的
// 信息丢掉，所以这里不能用简单的 s[:max]。
func truncateStream(data []byte, max int) ([]byte, bool) {
	if len(data) <= max {
		return data, false
	}
	s, truncated := TruncateMiddle(string(data), max)
	return []byte(s), truncated
}
