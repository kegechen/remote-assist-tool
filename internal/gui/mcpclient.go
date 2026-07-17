package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// MCPClient 把 remote help --mcp-stdio 子进程包装成一个 JSON-RPC over stdio 的 MCP client。
// GUI 后端用它来 initialize / connect / tools/call，等价于 Claude/opencode 与 MCP server 的协议。
type MCPClient struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Scanner

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan rpcResponse
	streamQs map[int64]chan chunkMsg // 流式调用的块队列，见 streamQueueDepth
	initOnce bool

	done      chan struct{} // 子进程退出时关闭
	doneOnce  sync.Once     // 保证 done 只被关一次（readLoop 与 Close 会并发到达）
	closeOnce sync.Once     // 保证子进程只被回收一次（cmd.Wait 不可重入）
}

// markDone 关闭 done，幂等。readLoop 与 Close 都会走到这里，
// 用裸的 select/default 判空再 close 不是原子操作，并发下会 double-close panic。
func (c *MCPClient) markDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse 承载 server 发来的两种帧：响应（有 id + result/error）与通知
// （有 method + params、无 id）。readLoop 按 Method 是否为空区分。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// streamNotifyMethod 与 mcp.streamNotifyMethod 对应：远端工具的流式输出块。
const streamNotifyMethod = "notifications/remote-assist/stream"

// streamQueueDepth 单条流式调用的块队列深度（块最大 32 KiB，故上限约 8 MiB）。
//
// 队列存在的理由：readLoop 是单 goroutine，若在它里面直接回调消费者（GUI 要把块写进
// 浏览器的 HTTP 响应），一个卡住的浏览器就会堵死 readLoop，进而拖垮这个 client 上的
// **所有**调用——本仓库为"一条卡住全卡死"付过代价，不能再犯。所以 readLoop 只做非阻塞
// 入队，慢消费者最多影响自己这条流。
const streamQueueDepth = 256

type chunkMsg struct {
	stream string
	data   []byte
}

// NewMCPClient 启动 remote help --mcp-stdio 子进程并准备 stdio 通道。
//
// 采用 bootstrap 模式（不预先传 --code / --no-auth）：子进程进入等待 connect 工具
// 的状态，GUI 在用户点击「连接」时再通过 connect 工具传入协助码 / server，从而支持
// 动态切换会话、断线后重连，与 Claude/opencode 的使用方式一致。
// binPath 为 remote 可执行文件路径；args 为传给 help 子命令的额外参数（不含 help 与 --mcp-stdio）。
func NewMCPClient(binPath string, args []string) (*MCPClient, error) {
	full := append([]string{"help", "--mcp-stdio"}, args...)
	cmd := exec.Command(binPath, full...)
	cmd.Stderr = os.Stderr // 子进程日志直接打到我们的 stderr，便于排查

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start remote: %w", err)
	}

	c := &MCPClient{
		cmd:      cmd,
		stdin:    stdin,
		nextID:   1, // 从 1 起：通知帧没有 id，反序列化后 ID 是零值 0，混用会歧义
		pending:  make(map[int64]chan rpcResponse),
		streamQs: make(map[int64]chan chunkMsg),
		done:     make(chan struct{}),
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	c.out = sc
	go c.readLoop()
	return c, nil
}

// readLoop 持续读取子进程 stdout 上的 JSON-RPC 响应并分发给对应等待者。
func (c *MCPClient) readLoop() {
	for c.out.Scan() {
		line := c.out.Bytes()
		if len(line) == 0 {
			continue
		}
		var r rpcResponse
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.Method != "" {
			c.handleNotify(&r)
			continue
		}
		if r.ID == 0 {
			// 通知类帧（notifications/* 等）没有 id；请求 id 从 1 起，故这里必非响应
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[r.ID]
		if ok {
			delete(c.pending, r.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- r
		}
	}
	// 子进程退出：唤醒所有等待者，返回错误
	c.mu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- rpcResponse{Error: &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -1, Message: "mcp process exited"}}
	}
	c.mu.Unlock()
	// 通知守护协程：子进程已死
	c.markDone()
}

// handleNotify 处理 server 推来的通知帧。目前只有流式输出块一种。
func (c *MCPClient) handleNotify(r *rpcResponse) {
	if r.Method != streamNotifyMethod {
		return // 其他通知（未来的 server 端扩展）按 JSON-RPC 规范忽略
	}
	var p struct {
		RequestID int64  `json:"requestId"`
		Stream    string `json:"stream"`
		Data      []byte `json:"data"`
	}
	if json.Unmarshal(r.Params, &p) != nil {
		return
	}
	// 查表与投递必须在同一次持锁内完成：callStream 收尾时会在锁内关掉队列，若这里先取出
	// q 再放锁去发，正好撞上关闭就是 send on closed channel —— 直接 panic 掉整个 GUI
	// 进程（readLoop 是裸 goroutine，没人 recover）。
	// 走响应收尾时撞不上（响应也由 readLoop 派发，与本函数天然串行），但 **ctx 取消** 会：
	// GUI 的 /api/exec/stream 用 r.Context()，用户关掉标签页它立刻取消 callStream，而远端
	// 命令还在吐块——两条路径就此真正并发。见 TestStreamCtxCancelRacesChunks。
	// 锁内发不会卡：投递是非阻塞的（下面的 default），且 pump goroutine 从不拿这把锁，
	// 所以慢消费者堵不住其他调用。
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.streamQs[p.RequestID]
	if q == nil {
		return // 非流式调用，或调用已结束：丢弃迟到块
	}
	select {
	case q <- chunkMsg{stream: p.Stream, data: p.Data}:
	default:
		// 消费者跟不上（浏览器卡住/网页已关）。丢这一块，绝不阻塞 readLoop——
		// 见 streamQueueDepth。不静默：如实打日志。
		log.Printf("mcp: stream queue full for request %d, dropping %d bytes", p.RequestID, len(p.Data))
	}
}

// Done 返回一个在子进程退出时关闭的 channel，供守护协程监听。
func (c *MCPClient) Done() <-chan struct{} { return c.done }

// IsAlive 报告子进程是否仍在运行。
func (c *MCPClient) IsAlive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Initialize 执行 MCP 握手。必须在使用任何工具前调用一次。
func (c *MCPClient) Initialize(ctx context.Context) error {
	c.mu.Lock()
	if c.initOnce {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "remote-assist-gui", "version": "1"},
	})
	if err != nil {
		return err
	}
	// 通知 server 已初始化（无需等待响应）
	_ = c.notify("notifications/initialized", map[string]any{})
	c.mu.Lock()
	c.initOnce = true
	c.mu.Unlock()
	return nil
}

// CallTool 调用一个 MCP 工具，返回工具结果（已是 humanized JSON 文本）。
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": json.RawMessage(rawArgs),
	})
}

// CallToolStream 调用一个工具，并在它执行途中把输出块实时交给 onChunk；最终结果由返回
// 值给出。onChunk 在一个专属 goroutine 上被顺序调用，允许阻塞（不会拖累其他调用），且
// 保证在本函数返回前全部回调完毕——即"结果一定在最后一块之后"。
func (c *MCPClient) CallToolStream(ctx context.Context, name string, args map[string]any, onChunk func(stream string, data []byte)) (json.RawMessage, error) {
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return c.callStream(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": json.RawMessage(rawArgs),
	}, onChunk)
}

// ListTools 返回 MCP server 暴露的工具 schema 列表。
func (c *MCPClient) ListTools(ctx context.Context) ([]map[string]any, error) {
	res, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(res, &body); err != nil {
		return nil, err
	}
	return body.Tools, nil
}

func (c *MCPClient) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	return c.callStream(ctx, method, params, nil)
}

// callStream 发一条 JSON-RPC 请求并等响应；onChunk 非 nil 时同时接收流式块。
func (c *MCPClient) callStream(ctx context.Context, method string, params map[string]any, onChunk func(string, []byte)) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	var q chan chunkMsg
	if onChunk != nil {
		q = make(chan chunkMsg, streamQueueDepth)
		c.streamQs[id] = q
	}
	c.mu.Unlock()
	// 兜住所有退出路径（正常返回 / ctx 取消 / 写失败）：正常路径下 readLoop 收到响应时
	// already 删过，这里再删一次是 no-op；但 ctx 取消与写失败那两条路 readLoop 永远等不到
	// 响应，不删就一直泄漏在 map 里。
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if onChunk != nil {
		pumpDone := make(chan struct{})
		go func() {
			defer close(pumpDone)
			for m := range q {
				onChunk(m.stream, m.data)
			}
		}()
		// 摘登记与关队列必须在同一次持锁内做完，且与 handleNotify 的投递互斥——只摘登记
		// 再放锁去关，仍会与"已取出 q、正要发"的 handleNotify 撞上（见那边的注释）。
		// 关掉队列后等 pump 排空：readLoop 保证块都排在响应之前入队，所以排空即"所有块
		// 都已回调完"，本函数返回的结果必然落在最后一块之后。
		defer func() {
			c.mu.Lock()
			delete(c.streamQs, id)
			close(q)
			c.mu.Unlock()
			<-pumpDone
		}()
	}

	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		b, _ := json.Marshal(params)
		req.Params = b
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')

	if _, err := c.stdin.Write(b); err != nil {
		return nil, fmt.Errorf("write to mcp: %w", err)
	}

	select {
	case <-ctx.Done():
		// 必须通知子进程取消，否则远端命令会变成没人收的孤儿：ctx 只活在**本进程**里，
		// 子进程那次 tools/call 有它自己的 ctx，唯一能捅动它的入口就是这条通知
		// （见 mcp.Server 的 notifications/cancelled 分支：按 requestId 查 inflight 并 cancel，
		// 进而让 bridge 发 MsgToolCancel 到 share 端把命令杀掉）。
		// 典型场景：用户关掉浏览器标签页 → handleExecStream 的 r.Context() 取消 → 到这里。
		//
		// **必须异步发**：stdin.Write 没有超时，子进程一旦不读 stdin 就会一直阻塞在那儿——
		// 而这里正是「要放弃、要赶紧脱身」的路径，同步发等于让取消卡在它想取消的东西上。
		// 并发写不会撕帧：每条消息是单次 Write，os.File.Write 内部自带锁。
		go func() { _ = c.notify("notifications/cancelled", map[string]any{"requestId": id}) }()
		return nil, ctx.Err()
	case r := <-ch:
		if r.Error != nil {
			return nil, fmt.Errorf("%s", r.Error.Message)
		}
		return r.Result, nil
	}
}

func (c *MCPClient) notify(method string, params map[string]any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, _ := json.Marshal(params)
		req.Params = b
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

// Close 终止子进程。幂等：重复调用安全，并发调用也安全。
func (c *MCPClient) Close() error {
	c.closeOnce.Do(func() {
		if c.cmd == nil || c.cmd.Process == nil {
			c.markDone()
			return
		}
		// 先关 stdin，让子进程自己退出；超时再强杀
		_ = c.stdin.Close()
		waited := make(chan error, 1)
		go func() { waited <- c.cmd.Wait() }()
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-waited // 等 Wait 收尸，避免僵尸进程与 goroutine 泄漏
		}
		c.markDone()
	})
	return nil
}
