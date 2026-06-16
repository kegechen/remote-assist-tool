package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ToolCaller MCP server 把 tools/call 转发给 share 端的契约
type ToolCaller interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

type Server struct {
	bridge   ToolCaller
	writeMu  sync.Mutex // 串行化并发 goroutine 对 out 的写，避免 JSON 行交错
	inflight sync.Map   // requestID(JSON 原文 string) -> context.CancelFunc
}

func NewServer(b ToolCaller) *Server { return &Server{bridge: b} }

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve 在 in/out 上跑一个 MCP server loop
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			s.write(out, rpcResp{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error"}})
			continue
		}
		// tools/call 可能长跑（exec / 大文件 / 慢隧道）。若同步执行，整个 stdin 读
		// 循环会被这一条阻塞——后续工具调用以及 notifications/cancelled 都读不到，
		// 一条卡住就全部卡死。所以 tools/call 并发处理；其余方法（initialize /
		// tools/list 等握手类）保持顺序语义。
		// 安全性：rpcReq 的 json.RawMessage 字段在 Unmarshal 时已拷贝出独立底层数组，
		// 且 req 每轮新建，可安全交给 goroutine（不会被下一轮 sc.Scan 覆写）。
		if req.Method == "tools/call" {
			rc := req
			go s.dispatch(ctx, &rc, out)
		} else {
			s.dispatch(ctx, &req, out)
		}
	}
	return sc.Err()
}

func (s *Server) dispatch(ctx context.Context, req *rpcReq, out io.Writer) {
	switch req.Method {
	case "initialize":
		s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "remote-assist", "version": "1"},
		}})
	case "tools/list":
		s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": AllSchemas()}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		if s.bridge == nil {
			s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32603, Message: "no bridge"}})
			return
		}
		// 每条调用一个可取消子 ctx，按 JSON-RPC id 登记，供 notifications/cancelled 精确取消。
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if len(req.ID) > 0 {
			key := string(req.ID)
			s.inflight.Store(key, cancel)
			defer s.inflight.Delete(key)
		}
		result, err := s.bridge.CallTool(callCtx, p.Name, p.Arguments)
		if err != nil {
			s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32000, Message: err.Error()}})
			return
		}
		s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(result)}},
		}})
	case "notifications/initialized":
		// no-op
	case "notifications/cancelled":
		// Claude 取消某条 in-flight 请求：解析 requestId，取消对应调用的 ctx。
		// 这会触发 bridge.CallTool 的 <-ctx.Done() 分支——向 share 发 MsgToolCancel
		// 并立即返回，避免用户中断/拒绝后仍干等兜底超时。
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		json.Unmarshal(req.Params, &p)
		if len(p.RequestID) > 0 {
			if v, ok := s.inflight.Load(string(p.RequestID)); ok {
				v.(context.CancelFunc)()
			}
		}
	default:
		if len(req.ID) > 0 {
			s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}})
		}
	}
}

func (s *Server) write(out io.Writer, r rpcResp) {
	r.JSONRPC = "2.0"
	b, _ := json.Marshal(r)
	s.writeMu.Lock()
	out.Write(b)
	out.Write([]byte("\n"))
	s.writeMu.Unlock()
}
