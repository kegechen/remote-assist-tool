package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
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

	if a.Stream && sink != nil {
		return e.runStreaming(runCtx, cmd, sink)
	}

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

// execStreamChunk 单个流式帧的读缓冲上限。管道有多少读多少，所以它只是上限而非攒够
// 才发——命令零星吐一行也会立刻成帧送走，终端才有"边跑边出"的手感。
const execStreamChunk = 32 * 1024

// runStreaming 边跑边把 stdout/stderr 推给 sink，供 GUI 流式终端实时渲染。
//
// 与非流式模式的契约差异（调用方以 stream=true 显式选入）：
//   - 输出不在 share 端累积，也不受 max_output_bytes 截断——实时输出由调用方自己留存
//     （浏览器的滚动缓冲）。长跑命令若在这里攒全量输出，既失去流式的意义又有 OOM 风险。
//   - 最终 ExecResult 只带退出状态（exit_code / error），不重复带 stdout/stderr。
func (e *ExecTool) runStreaming(ctx context.Context, cmd *exec.Cmd, sink agent.StreamSink) (json.RawMessage, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		// 命令没能启动（找不到可执行文件、cwd 不存在等）：与非流式一致，把原因放 error
		// 字段带出去，否则调用方只看到 exit -1 却不知为什么。
		return json.Marshal(ExecResult{ExitCode: -1, Error: err.Error()})
	}

	pump := func(r io.Reader, name string, wg *sync.WaitGroup) {
		defer wg.Done()
		buf := make([]byte, execStreamChunk)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				if sink.Send(name, buf[:n]) != nil {
					return // 隧道写不动了：再读也发不出去，收工让 Wait 去收尸
				}
			}
			if rerr != nil {
				return // 含 io.EOF：命令结束或被 ctx kill 后管道关闭
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pump(stdout, "stdout", &wg)
	go pump(stderr, "stderr", &wg)
	// 必须等两个 pump 读完再 cmd.Wait()：Wait 会关闭管道，先 Wait 会截断尚未读出的输出。
	// 同样必须等它们发完再返回——Run 一返回 daemon 就发 ToolResp，而 ToolResp 是流的终止
	// 信号，此后到达的 chunk 会被 bridge 当迟到帧丢弃。
	wg.Wait()
	werr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("deadline_exceeded: exec timed out")
	}
	return json.Marshal(ExecResult{ExitCode: exitCodeOf(werr)})
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
