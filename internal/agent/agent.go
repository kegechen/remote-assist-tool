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

// MsgConn agent 与 share Client 之间的最小依赖契约（便于单测注入）
type MsgConn interface {
	SendMessage(t proto.MessageType, payload interface{}) error
	// agent 不主动 read；share 端 dispatch 把收到的 Tool 消息通过 Inject 投递
}

// Daemon share 端工具消息分发器；持有 Registry + outbound conn
type Daemon struct {
	reg        *Registry
	conn       MsgConn
	key        [32]byte
	inbound    chan *proto.Message
	cancels    sync.Map // id -> context.CancelFunc
	OnActivity func(line string) // 可选钩子，每条工具调用完成时触发
}

func NewDaemon(reg *Registry, conn MsgConn, key [32]byte) *Daemon {
	return &Daemon{reg: reg, conn: conn, key: key, inbound: make(chan *proto.Message, 64)}
}

// Inject share 端 dispatch 收到 MsgToolReq/MsgToolCancel 时调用。
// 非阻塞：如果 daemon 处理慢、inbound buffer 满，丢弃该消息（log warning），
// 避免回灌 share 端的主 dispatch 循环阻塞 SSH 隧道与心跳。
func (d *Daemon) Inject(msg *proto.Message) {
	select {
	case d.inbound <- msg:
	default:
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

func (d *Daemon) handleReq(parent context.Context, msg *proto.Message) {
	var req proto.ToolReq
	if err := proto.DecodePayload(msg, &req); err != nil {
		return
	}
	// 解密 args（key 非零时才解密；密文以 JSON 字符串 base64 形式承载）
	if d.key != [32]byte{} && len(req.ArgsJSON) > 0 {
		plain, err := proto.AEADOpenJSON(&d.key, req.ArgsJSON)
		if err != nil {
			d.conn.SendMessage(proto.MsgToolResp, &proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "decrypt_failed", ErrorMsg: err.Error()})
			return
		}
		req.ArgsJSON = plain
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if req.DeadlineMs > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(req.DeadlineMs)*time.Millisecond)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel() // 防止 ctx goroutine 泄漏
	d.cancels.Store(req.ID, context.CancelFunc(cancel))
	defer d.cancels.Delete(req.ID)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tool | %s | panic | 0ms | err:remote_panic", req.Tool)
			d.conn.SendMessage(proto.MsgToolResp, &proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "remote_panic", ErrorMsg: "tool panic"})
		}
	}()
	sink := &chunkSink{daemon: d, id: req.ID}
	start := time.Now()
	resp := d.reg.Dispatch(ctx, &req, sink)
	// 加密 result（密文以 JSON 字符串 base64 形式承载，保证 json.RawMessage 合法）
	if d.key != [32]byte{} && len(resp.ResultJSON) > 0 {
		if wrapped, err := proto.AEADSealJSON(&d.key, resp.ResultJSON); err == nil {
			resp.ResultJSON = wrapped
		}
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
	d.conn.SendMessage(proto.MsgToolResp, &resp)
}

// chunkSink reserved for v2 streaming; v1 tools do not use sink
type chunkSink struct {
	daemon *Daemon
	id     uint64
	seq    uint32
	mu     sync.Mutex
}

func (s *chunkSink) Send(stream string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := data
	if s.daemon.key != [32]byte{} {
		ct, err := proto.AEADSeal(&s.daemon.key, data)
		if err != nil {
			return err
		}
		payload = ct
	}
	c := proto.StreamChunk{ID: s.id, Seq: s.seq, Stream: stream, Data: payload}
	s.seq++
	return s.daemon.conn.SendMessage(proto.MsgToolStream, &c)
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
