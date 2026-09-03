package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

type ExecArgs struct {
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutMs      uint32            `json:"timeout_ms,omitempty"`
	MaxOutputBytes int               `json:"max_output_bytes,omitempty"`
	Stream         bool              `json:"stream,omitempty"`
	StdinBytes     []byte            `json:"stdin,omitempty"`
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

// execWaitDelay 取消命令之后，最多再给 I/O 管道多少时间自行收尾。
//
// 只杀进程还不够：孙子进程继承了 stdout/stderr 的写端，只要还有一个活着，管道就读不到
// EOF，cmd.Wait 会无限期挂着——工具调用既不返回结果也不返回错误，daemon 那一侧的槽位
// 就此漏掉。configureProcessGroup 已经尽量把整棵树杀干净，WaitDelay 是它漏杀时的兜底：
// 时间一到 Go 会强制关掉管道，Wait 带 exec.ErrWaitDelay 返回。
//
// 取 2s 而不是更短，是留给「被杀的子进程正在被内核回收、缓冲里还有最后几 KB」的正常
// 收尾；这段时间只在命令已经被取消之后才会发生，不影响正常路径。
const execWaitDelay = 2 * time.Second

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
	// 超时/取消时连同孙子进程一起杀，并给管道收尾兜个底。两者缺一不可：
	// 只杀树而没有 WaitDelay，漏网的孙子仍能把 Wait 吊死；只有 WaitDelay 而不杀树，
	// 孤儿进程会继续在被协助端的机器上跑下去。
	configureProcessGroup(cmd)
	cmd.WaitDelay = execWaitDelay
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
		return e.runStreaming(runCtx, cancel, cmd, sink)
	}

	maxOut := resolveMaxOutput(a.MaxOutputBytes)
	// 自己接管两条流，而不是用 cmd.Output()：后者先把全部输出攒进 bytes.Buffer，再交给
	// 截断函数，于是内存峰值由命令决定而不是由 max_output_bytes 决定——一条 `find /`
	// 就能把被协助端撑爆，而攒下来的东西 99% 马上就要被扔掉。
	outBuf := newBoundedStream(maxOut)
	errBuf := newBoundedStream(maxOut)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("deadline_exceeded: exec timed out")
	}

	exitCode := exitCodeOf(err)
	// WaitDelay 到期时 err 是 exec.ErrWaitDelay 而非 *ExitError，但进程本身已经被回收，
	// ProcessState 里的退出码是真的，不该退化成 -1。
	if exitCode == -1 && cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// 命令没能启动（找不到可执行文件、权限拒绝、cwd 不存在等）时 err 不是 *exec.ExitError，
	// stderr 为空、exitCode=-1。必须把原因单独带出去，否则调用方只看到 exit -1。
	var startErr string
	if err != nil {
		switch {
		case errors.Is(err, exec.ErrWaitDelay):
			startErr = fmt.Sprintf("命令已退出，但仍有后台子进程占着输出管道；等待 %s 后强制收尾，尾部输出可能不完整", execWaitDelay)
		default:
			if _, isExit := err.(*exec.ExitError); !isExit {
				startErr = err.Error()
			}
		}
	}

	stdout, stdoutTrunc := outBuf.result()
	stderrB, stderrTrunc := errBuf.result()
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
func (e *ExecTool) runStreaming(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, sink agent.StreamSink) (json.RawMessage, error) {
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
					// 隧道写不动了：光自己收工还不够——本 pump 一停读，输出密集的子进程
					// 很快把这条管道写满并阻塞，另一个 pump 和 cmd.Wait 就都卡到外层
					// deadline（默认 5min）才醒。取消命令杀掉子进程，让两个 pump 一起收尾。
					cancel()
					return
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
