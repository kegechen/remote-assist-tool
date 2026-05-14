package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

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
