package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

type ExecArgs struct {
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutMs  uint32            `json:"timeout_ms,omitempty"`
	Stream     bool              `json:"stream,omitempty"`
	StdinBytes []byte            `json:"stdin,omitempty"`
}

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
}

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
		envv := make([]string, 0, len(a.Env))
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
	return json.Marshal(ExecResult{ExitCode: exitCode, Stdout: out, Stderr: stderr})
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
