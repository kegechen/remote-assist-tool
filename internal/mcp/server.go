package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Bridge MCP server 把 tools/call 转发给 share 端的契约
type Bridge interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

type Server struct{ bridge Bridge }

func NewServer(b Bridge) *Server { return &Server{bridge: b} }

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
		s.dispatch(ctx, &req, out)
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
		result, err := s.bridge.CallTool(ctx, p.Name, p.Arguments)
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
		// bridge 处理（Task 15 留口子）
	default:
		if len(req.ID) > 0 {
			s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}})
		}
	}
}

func (s *Server) write(out io.Writer, r rpcResp) {
	r.JSONRPC = "2.0"
	b, _ := json.Marshal(r)
	out.Write(b)
	out.Write([]byte("\n"))
}
