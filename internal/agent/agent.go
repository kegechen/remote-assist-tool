package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// Tool 单个工具的执行单元
type Tool interface {
	Name() string
	// Run 同步返回结果；若工具支持流式输出，通过 sink 推送 StreamChunk，最终 ResultJSON 可为 nil
	Run(ctx context.Context, args json.RawMessage, sink StreamSink) (json.RawMessage, error)
}

// StreamSink agent 注入的流输出通道（Dispatcher 负责把数据封成 MsgToolStream 帧）
type StreamSink interface {
	Send(stream string, data []byte) error // stream = "stdout" | "stderr" | ""
}

// Registry 名称到 Tool 的注册表
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	r.tools[t.Name()] = t
	r.mu.Unlock()
}

// Dispatch 同步执行（流式工具内部用 sink 推 chunks，外部仍返回 ToolResp）
func (r *Registry) Dispatch(ctx context.Context, req *proto.ToolReq, sink StreamSink) proto.ToolResp {
	r.mu.RLock()
	t, ok := r.tools[req.Tool]
	r.mu.RUnlock()
	if !ok {
		return proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "unknown_tool", ErrorMsg: fmt.Sprintf("tool %q not registered", req.Tool)}
	}
	out, err := t.Run(ctx, req.ArgsJSON, sink)
	if err != nil {
		code, msg := classifyError(err)
		return proto.ToolResp{ID: req.ID, OK: false, ErrorCode: code, ErrorMsg: msg}
	}
	return proto.ToolResp{ID: req.ID, OK: true, ResultJSON: out}
}

// classifyError 把工具返回的 error 映射到 spec §5.3 错误码
func classifyError(err error) (code, msg string) {
	s := err.Error()
	switch {
	case strings.Contains(s, "path_outside_root"):
		return "path_outside_root", s
	case strings.Contains(s, "exec_denied"):
		return "exec_denied", s
	case strings.Contains(s, "deadline_exceeded"), strings.Contains(s, "context deadline exceeded"):
		return "deadline_exceeded", s
	case strings.Contains(s, "permission denied"):
		return "permission_denied", s
	case strings.Contains(s, "no such file"), strings.Contains(s, "file does not exist"):
		return "file_not_found", s
	default:
		return "internal_error", s
	}
}

// defaultToolTimeout 给未显式带 DeadlineMs 的工具请求兜底执行上限，按工具分档：
// 元数据类快工具短兜底；exec / grep / 文件传输等可能合理长跑的给更长上限。与 host 端
// mcp.fallbackTimeoutFor 呼应，但更紧（share 端先于 host 兜底返回，避免误报 tunnel_lost）。
func defaultToolTimeout(tool string) time.Duration {
	switch tool {
	case "stat", "list_dir", "process_list", "tail_log", "glob":
		return 30 * time.Second
	default:
		return 5 * time.Minute
	}
}

// MsgConn agent 与 share Client 之间的最小依赖契约（便于单测注入）
type MsgConn interface {
	SendMessage(t proto.MessageType, payload interface{}) error
	// agent 不主动 read；share 端 dispatch 把收到的 Tool 消息通过 Inject 投递
}

// Daemon share 端工具消息分发器；持有 Registry + outbound conn
type Daemon struct {
	reg        *Registry
	conn       MsgConn
	connMu     sync.RWMutex // protects conn for SwapConn
	key        [32]byte
	inbound    chan *proto.Message
	cancels    sync.Map          // id -> context.CancelFunc
	replay     replayGuard       // 按调用 ID 抗重放，每把 key 一份
	OnActivity func(line string) // 可选钩子，每条工具调用完成时触发
}

func NewDaemon(reg *Registry, conn MsgConn, key [32]byte) *Daemon {
	return &Daemon{reg: reg, conn: conn, key: key, inbound: make(chan *proto.Message, 64)}
}

// RotateKey 用新的 session_key 替换；用于同一 share 服务多个 help 端续连。
// 取消所有 in-flight 请求（旧 key 加密的不再有意义）。
func (d *Daemon) RotateKey(key [32]byte) {
	d.connMu.Lock()
	same := d.key == key
	d.key = key
	d.connMu.Unlock()
	if same {
		return // key 没变，在途请求依然有效，不该被打断
	}
	// 换 key ⟹ 新会话，抗重放窗口必须跟着重置：新会话的调用 ID 与旧会话无关，
	// 留着旧位会把合法请求误判成重放。
	d.replay.reset()
	d.cancels.Range(func(k, v any) bool {
		if cancel, ok := v.(context.CancelFunc); ok {
			cancel()
		}
		return true
	})
}

// currentKey 取当前会话密钥快照。与 conn 同锁保护：P2P 热升级会在工具调用飞行途中
// 并发替换二者，裸读会构成数据竞态。
func (d *Daemon) currentKey() [32]byte {
	d.connMu.RLock()
	defer d.connMu.RUnlock()
	return d.key
}

// SwapConn 原子替换出站连接（relay ⇄ P2P），并按需轮换会话密钥。
//
// key 与当前相同时**不取消在途请求**：P2P 热升级复用 relay 握手协商出的同一把 key，
// 换的只是传输通道，正在跑的工具调用不该因此被打断。升级发生在会话中途（connect
// 之后几秒），很可能正压在用户的第一个 exec 上——这里若无条件 RotateKey，那个调用
// 会直接以 cancelled 收场。
// key 确实变了（重新握手，如 P2P 断开后降级回 relay）才轮换并取消在途请求，
// 因为旧 key 加密的请求已经没有意义。
func (d *Daemon) SwapConn(conn MsgConn, key [32]byte) {
	d.connMu.Lock()
	d.conn = conn
	d.connMu.Unlock()
	d.RotateKey(key)
}

// sendMsg sends a message via the current conn, safe for concurrent SwapConn.
func (d *Daemon) sendMsg(t proto.MessageType, payload interface{}) error {
	d.connMu.RLock()
	c := d.conn
	d.connMu.RUnlock()
	return c.SendMessage(t, payload)
}

// Inject share 端 dispatch 收到 MsgToolReq/MsgToolCancel 时调用。
// 非阻塞：如果 daemon 处理慢、inbound buffer 满，丢弃该消息（log warning），
// 避免回灌 share 端的主 dispatch 循环阻塞 SSH 隧道与心跳。
func (d *Daemon) Inject(msg *proto.Message) {
	select {
	case d.inbound <- msg:
	default:
		// 缓冲满不能静默丢 MsgToolReq，否则 host 永远等不到响应、干等兜底超时。
		// 解出 req.ID 回一条 server_busy 错误，让调用方立即快速失败。
		// Inject 必须保持非阻塞（见函数头注释）——sendMsg 可能阻塞（受写超时限最多 ~30s），
		// 故放 goroutine 发，绝不阻塞 share 端主 dispatch 读循环 / 心跳。
		//
		// 必须走 sendResp 而不是裸 sendMsg：握手后 host 要求每条响应都带密文
		// （明文的 ok/error_code 无从认证），裸发的 server_busy 会被判成
		// unauthenticated，真正的原因「daemon 过载」就此丢失，排障时只看得到
		// 一条误导人的鉴权错误。
		if msg.Type == proto.MsgToolReq {
			var req proto.ToolReq
			if err := proto.DecodePayload(msg, &req); err == nil {
				go d.sendResp(d.currentKey(), proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "server_busy", ErrorMsg: "daemon inbound full"})
			}
		}
		log.Printf("daemon: inbound full, dropping %s", msg.Type)
	}
}

// RunLoop 直到 ctx 完成
func (d *Daemon) RunLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-d.inbound:
			switch msg.Type {
			case proto.MsgToolReq:
				go d.handleReq(ctx, msg)
			case proto.MsgToolCancel:
				var c proto.Cancel
				if err := proto.DecodePayload(msg, &c); err != nil {
					log.Printf("daemon: bad cancel payload: %v", err)
					continue
				}
				if v, ok := d.cancels.Load(c.ID); ok {
					v.(context.CancelFunc)()
				}
			}
		}
	}
}

// isBlankArgs 判断 args 是否等于"什么都没给"。
//
// 不能只看 len == 0：ToolReq.ArgsJSON 的 tag 没有 omitempty，发送方把字段留空时线上
// 传的是 args:null，解出来是 4 字节的 "null" 而非 nil。只看长度的话，这种请求会掉进
// 解密分支报 decrypt_failed，把一次"根本没鉴权"说成"密文坏了"，排障时会指向错误的方向。
func isBlankArgs(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func (d *Daemon) handleReq(parent context.Context, msg *proto.Message) {
	var req proto.ToolReq
	if err := proto.DecodePayload(msg, &req); err != nil {
		return
	}
	// 整个请求用同一份 key 快照：解密入参与加密结果必须配对，中途若发生 SwapConn
	// （P2P 升级/降级）也不能让一半用旧 key、一半用新 key。
	key := d.currentKey()
	// 握手完成后，args 必须是合法密文——包括"没有参数"的调用，host 也会封一个 "{}"。
	//
	// 以前的判据是 len(req.ArgsJSON) > 0，等于留了个后门：注入方发一条不带 args 的
	// tool_req{tool:"process_list"} 就绕过全部解密直接触发远端 fork tasklist/ps，
	// 全程不需要会话密钥。现在没有合法密文就一律拒绝，且拒绝发生在 Dispatch 之前。
	if key != [32]byte{} {
		if isBlankArgs(req.ArgsJSON) {
			d.sendResp(key, proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "unauthenticated", ErrorMsg: "args must be AEAD-sealed after handshake"})
			return
		}
		// AAD 绑定 id/tool/deadline_ms：把捕获的密文改挂到别的工具上会解密失败。
		plain, err := proto.AEADOpenJSON(&key, req.ArgsJSON, proto.ToolReqAAD(req.ID, req.Tool, req.DeadlineMs))
		if err != nil {
			d.sendResp(key, proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "decrypt_failed", ErrorMsg: err.Error()})
			return
		}
		req.ArgsJSON = plain
		// 抗重放：AAD 挡住了"改 ID 重挂"，但原样重放仍然成立（nonce 由发送方给），
		// 只能靠接收侧去重。放在解密之后，避免未认证的 ID 污染窗口。
		if !d.replay.accept(req.ID) {
			log.Printf("daemon: rejected replayed tool_req id=%d tool=%s", req.ID, req.Tool)
			d.sendResp(key, proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "replayed", ErrorMsg: "duplicate or out-of-window request id"})
			return
		}
	}
	// 兜底执行上限：host 通常不发 DeadlineMs；不加 deadline 时，阻塞型 I/O（FIFO /
	// 特殊文件 / 卡死 syscall）会让工具永不返回、host 干等 2~10min 兜底且 goroutine 泄漏。
	// 按工具分档给默认 deadline，保证每个请求有界。
	timeout := time.Duration(req.DeadlineMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultToolTimeout(req.Tool)
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	d.cancels.Store(req.ID, context.CancelFunc(cancel))
	defer d.cancels.Delete(req.ID)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tool | %s | panic | 0ms | err:remote_panic", req.Tool)
			d.sendResp(key, proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "remote_panic", ErrorMsg: "tool panic"})
		}
	}()
	sink := &chunkSink{daemon: d, id: req.ID, active: requestWantsStream(req.ArgsJSON)}
	start := time.Now()
	// Dispatch 放 goroutine 里 race 计时器：即便工具阻塞在不响应 ctx 的 I/O，也保证 host
	// 在 deadline 内拿到响应（deadline_exceeded / cancelled），绝不让其永等。被遗弃的
	// goroutine 若跑 exec/ps 等 ctx-aware 工具会随 cancel 结束；纯阻塞 I/O 可能泄漏，属
	// 可接受代价（换取“任何请求必有返回”）。
	respCh := make(chan proto.ToolResp, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				respCh <- proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "remote_panic", ErrorMsg: "tool panic"}
			}
		}()
		respCh <- d.reg.Dispatch(ctx, &req, sink)
	}()
	var resp proto.ToolResp
	select {
	case resp = <-respCh:
	case <-ctx.Done():
		code := "deadline_exceeded"
		if ctx.Err() == context.Canceled {
			code = "cancelled"
		}
		resp = proto.ToolResp{ID: req.ID, OK: false, ErrorCode: code, ErrorMsg: fmt.Sprintf("tool %s: %s (limit %s)", req.Tool, code, timeout)}
	}
	status := "ok"
	if !resp.OK {
		status = "err:" + resp.ErrorCode
	}
	dur := time.Since(start).Milliseconds()
	argsSummary := summarizeArgs(req.Tool, req.ArgsJSON)
	log.Printf("tool | %s | %s | %dms | %s", req.Tool, argsSummary, dur, status)
	if d.OnActivity != nil {
		d.OnActivity(fmt.Sprintf("[%s] %s: %s (%dms, %s)",
			time.Now().Format("15:04:05"), req.Tool, argsSummary, dur, status))
	}
	// 流式调用必须先发一个经过 AEAD 认证的结束帧，再发 ToolResp。结束帧携带最终
	// Seq，使接收侧能够区分「真的没有更多输出」和「最后一帧刚好丢了」。
	if err := sink.Finish(); err != nil {
		log.Printf("tool | %s | stream terminator send failed: %v", req.Tool, err)
	}
	if err := d.sendResp(key, resp); err != nil {
		log.Printf("tool | %s | response send failed: %v", req.Tool, err)
	}
}

// sendResp 加封并发送一条工具响应。
//
// 握手后**每一条**响应都要封，包括 ResultJSON 为空的错误响应：密文里的 MAC 是
// ok / error_code / error_msg 这些明文字段唯一的认证依据（它们都在 AAD 里）。
// 不封的话，中间人把一次成功的 read_file 改成 ok:true + result 清空，接收侧会跳过
// 解密、直接把"空结果 + 成功"交给调用方——比一次明确的失败危险得多。
//
// 空 result 归一成 "{}" 而不是留空，是为了让"封过"与"没封"在接收侧可以只按
// len(result) > 0 区分，判据简单到不会有歧义。
func (d *Daemon) sendResp(key [32]byte, resp proto.ToolResp) error {
	if key != [32]byte{} {
		plain := resp.ResultJSON
		if len(plain) == 0 {
			plain = json.RawMessage("{}")
		}
		// 密文以 JSON 字符串 base64 形式承载，保证 json.RawMessage 合法。
		wrapped, err := proto.AEADSealJSON(&key, plain, proto.ToolRespAAD(resp.ID, resp.OK, resp.ErrorCode, resp.ErrorMsg))
		if err != nil {
			// 几乎只可能是熵源故障。宁可发一条接收侧必然拒绝的响应，也不能退回明文：
			// 那等于把工具结果原样交给 relay。
			log.Printf("daemon: seal tool_resp id=%d failed: %v", resp.ID, err)
			resp.ResultJSON = nil
		} else {
			resp.ResultJSON = wrapped
		}
	}
	return d.sendMsg(proto.MsgToolResp, &resp)
}

// chunkSink reserved for v2 streaming; v1 tools do not use sink
type chunkSink struct {
	daemon   *Daemon
	id       uint64
	seq      uint32
	active   bool
	finished bool
	mu       sync.Mutex
}

func (s *chunkSink) Send(stream string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return nil
	}
	s.active = true
	// 先把帧整体建好，AAD 直接取自它的字段——以后给 StreamChunk 添了新的明文字段
	// （或真的用起 Fin），漏进 AAD 的话是编译期看得见的改动。
	c := proto.StreamChunk{ID: s.id, Seq: s.seq, Stream: stream, Data: data}
	// key 取当前值而非请求开始时的快照：接收侧（mcp.Bridge.HandleInbound）同样按
	// 当前 key 解流帧，两边必须对称。换 key 会取消在途请求，所以这个窗口本就极短，
	// 真撞上时接收侧记一个空洞并把整次调用判为 stream_incomplete。
	if key := s.daemon.currentKey(); key != [32]byte{} {
		ct, err := proto.AEADSeal(&key, data, proto.StreamChunkAAD(c.ID, c.Seq, c.Stream, c.Fin))
		if err != nil {
			return err
		}
		c.Data = ct
	}
	s.seq++
	return s.daemon.sendMsg(proto.MsgToolStream, &c)
}

// Finish 发一条独立的、带 Fin=true 的空帧作为流终止符。它与数据帧共用 Seq 序列，
// 因而终止符本身丢失、或它之前的最后一帧丢失，接收侧都能明确判定流不完整。
func (s *chunkSink) Finish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.finished {
		return nil
	}
	s.finished = true
	c := proto.StreamChunk{ID: s.id, Seq: s.seq, Fin: true}
	if key := s.daemon.currentKey(); key != [32]byte{} {
		ct, err := proto.AEADSeal(&key, nil, proto.StreamChunkAAD(c.ID, c.Seq, c.Stream, c.Fin))
		if err != nil {
			return err
		}
		c.Data = ct
	}
	s.seq++
	return s.daemon.sendMsg(proto.MsgToolStream, &c)
}

// requestWantsStream covers the current stream=true request flag. Send also marks a sink
// active, so stream-capable tools that do not expose this flag still get a terminator once
// they produce output.
func requestWantsStream(raw json.RawMessage) bool {
	var flags struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(raw, &flags) == nil && flags.Stream
}

// summarizeArgs 对各工具脱敏：read_file/write_file 只记 path+size；exec 记 argv[0]+argc
func summarizeArgs(tool string, raw json.RawMessage) string {
	var generic map[string]json.RawMessage
	json.Unmarshal(raw, &generic)
	switch tool {
	case "exec":
		var argv []string
		json.Unmarshal(generic["argv"], &argv)
		if len(argv) == 0 {
			return "exec[]"
		}
		return fmt.Sprintf("exec %s argc=%d", argv[0], len(argv))
	case "read_file", "stat", "tail_log", "list_dir":
		var p string
		json.Unmarshal(generic["path"], &p)
		return tool + " " + p
	case "write_file":
		var p string
		var content []byte
		json.Unmarshal(generic["path"], &p)
		json.Unmarshal(generic["content"], &content)
		return fmt.Sprintf("write_file %s bytes=%d", p, len(content))
	default:
		return tool
	}
}
