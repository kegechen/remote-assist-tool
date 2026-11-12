package gui

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/gui/assets"
)

// 真终端（PTY）后端：把浏览器里的 xterm.js 接到被控端的 pty_* 工具上。
//
//	POST /api/pty/open    NDJSON 流：{"type":"session","id"} / {"type":"chunk","data":b64} / {"type":"result"} / {"type":"error"}
//	POST /api/pty/write   {id,data:b64}   输入（浏览器端串行发送；服务端按会话加锁保序）
//	POST /api/pty/resize  {id,cols,rows}
//	POST /api/pty/close   {id}
//
// 会话生命周期绑定 /api/pty/open 这条 HTTP 流：浏览器关页/断开 → r.Context 取消 → 向被控端发
// cancel 并补一次 pty_close，远端整棵进程树被清理。

// ptyClientCalls 抽出 pty 路由需要的 MCP 调用能力，便于单测注入。
type ptyClientCalls interface {
	CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error)
	CallToolStream(ctx context.Context, name string, args map[string]any, onChunk func(stream string, data []byte)) (json.RawMessage, error)
}

type ptyEntry struct {
	client ptyClientCalls
	cancel context.CancelFunc
	wmu    sync.Mutex // 同一会话的输入串行
}

// ptySessions 会话 id → 条目。id 是 128 位随机数，跨 Server 实例也不会冲突，故用包级表。
var ptySessions sync.Map

const (
	ptyCallTimeout = 15 * time.Second
	ptyMaxCols     = 1000
)

func (s *Server) currentClient() (ptyClientCalls, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected || s.client == nil {
		return nil, false
	}
	return s.client, true
}

func (s *Server) handlePTYOpen(w http.ResponseWriter, r *http.Request) {
	if s.rejectWhileUpgrading(w) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Cols  int    `json:"cols"`
		Rows  int    `json:"rows"`
		Shell string `json:"shell"`
		Cwd   string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.Cols < 0 || req.Rows < 0 || req.Cols > ptyMaxCols || req.Rows > ptyMaxCols {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cols/rows out of range"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	client, ok := s.currentClient()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "not connected; call /api/connect first"})
		return
	}

	id := randomToken()
	// 不设超时：终端是长会话；结束靠 r.Context（浏览器断开）/ pty_close / shell 退出。
	// 被控端 pty_open 自身有 24h 上限。
	ctx, cancel := context.WithCancel(r.Context())
	entry := &ptyEntry{client: client, cancel: cancel}
	ptySessions.Store(id, entry)
	defer func() {
		cancel()
		ptySessions.Delete(id)
		// 双保险：取消通知已会让远端清理，这里再显式 pty_close 一次（幂等），不依赖通知送达。
		go func() {
			cctx, ccancel := context.WithTimeout(context.Background(), ptyCallTimeout)
			defer ccancel()
			_, _ = client.CallTool(cctx, "pty_close", map[string]any{"session": id})
		}()
	}()

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	var wmu sync.Mutex
	send := func(v any) {
		wmu.Lock()
		enc.Encode(v)
		flusher.Flush()
		wmu.Unlock()
	}
	send(map[string]any{"type": "session", "id": id})

	args := map[string]any{"session": id, "stream": true}
	if req.Cols > 0 {
		args["cols"] = req.Cols
	}
	if req.Rows > 0 {
		args["rows"] = req.Rows
	}
	if req.Shell != "" {
		args["shell"] = req.Shell
	}
	if req.Cwd != "" {
		args["cwd"] = req.Cwd
	}
	res, err := client.CallToolStream(ctx, "pty_open", args, func(stream string, data []byte) {
		send(map[string]any{"type": "chunk", "data": data})
	})
	if err != nil {
		send(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	send(map[string]any{"type": "result", "result": json.RawMessage(res)})
}

// ptyLookup 解析请求体里的 id，找不到时写 404。
func ptyLookup(w http.ResponseWriter, id string) (*ptyEntry, bool) {
	v, ok := ptySessions.Load(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "pty_not_found: 终端会话不存在或已结束"})
		return nil, false
	}
	return v.(*ptyEntry), true
}

func (s *Server) handlePTYWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		ID   string `json:"id"`
		Data []byte `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	e, ok := ptyLookup(w, req.ID)
	if !ok {
		return
	}
	// 同一会话的输入串行：锁内完成整次调用，到达顺序即送达顺序。注意：不得记录 req.Data。
	e.wmu.Lock()
	defer e.wmu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), ptyCallTimeout)
	defer cancel()
	if _, err := e.client.CallTool(ctx, "pty_write", map[string]any{"session": req.ID, "data": req.Data}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePTYResize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID   string `json:"id"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.Cols < 1 || req.Rows < 1 || req.Cols > ptyMaxCols || req.Rows > ptyMaxCols {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cols/rows out of range"})
		return
	}
	e, ok := ptyLookup(w, req.ID)
	if !ok {
		return
	}
	e.wmu.Lock() // 与输入共用顺序，避免 resize 插到半条输入前后造成错位感
	defer e.wmu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), ptyCallTimeout)
	defer cancel()
	if _, err := e.client.CallTool(ctx, "pty_resize", map[string]any{"session": req.ID, "cols": req.Cols, "rows": req.Rows}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePTYClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 幂等：会话不存在也返回 ok。
	v, ok := ptySessions.Load(req.ID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "closed": false})
		return
	}
	e := v.(*ptyEntry)
	ctx, cancel := context.WithTimeout(r.Context(), ptyCallTimeout)
	defer cancel()
	_, err := e.client.CallTool(ctx, "pty_close", map[string]any{"session": req.ID})
	e.cancel() // 同时结束 open 流
	if err != nil {
		log.Printf("gui: pty_close: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "closed": true})
}

// handleAssets 提供离线内嵌的前端静态资源（xterm.js / fit addon / 终端页脚本）。
// 只读、公开的第三方库文件，不含任何机密，因此不要求 token（Host/Origin 仍由 guard 校验）。
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/assets/")), "/")
	b, err := fs.ReadFile(assets.StaticFS, name)
	if err != nil || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	ct := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".js"):
		ct = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		ct = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".txt"), strings.HasPrefix(path.Base(name), "LICENSE"):
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache") // 随二进制升级可能变，但文件小且在本机，不缓存最省心
	w.Write(b)
}

// isPublicAsset 供 guard 放行：仅 GET/HEAD 的 /assets/ 静态资源。
func isPublicAsset(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasPrefix(r.URL.Path, "/assets/")
}
