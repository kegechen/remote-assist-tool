package gui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/gui/assets"
)

// Server 是 GUI 的后端：托管前端静态页 + 通过 JSON 把请求转发给 MCP 子进程。
type Server struct {
	binPath string
	defaultServer string
	token   string // 启动时随机生成，见 guard

	mu     sync.Mutex
	client *MCPClient
	connected bool
	serverArgs []string

	// 守护：缓存最近一次成功的连接参数，子进程意外退出时自动重连
	lastCode     string
	lastServer   string
	lastNoAuth   bool
	guardStarted bool

	// SSE 广播
	sseMu   sync.Mutex
	sseSubs map[chan string]struct{}
}

// NewServer 创建一个 GUI 后端。binPath 为 remote 可执行文件路径。
func NewServer(binPath, defaultServer string) *Server {
	return &Server{
		binPath:       binPath,
		defaultServer: defaultServer,
		token:         randomToken(),
		sseSubs:       make(map[chan string]struct{}),
	}
}

// Token 返回本次进程的访问令牌。调用方（cmd/gui）要把它拼进首页 URL 交给浏览器。
func (s *Server) Token() string { return s.token }

// randomToken 生成 32 字符十六进制令牌。取不到随机数就直接 panic：宁可起不来，
// 也不能悄悄退化成一个可预测（甚至空）的令牌——那等于没有鉴权。
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("gui: 无法生成访问令牌: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Routes 注册所有 HTTP 路由，返回已套上 guard 的 handler。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/connect", s.handleConnect)
	mux.HandleFunc("/api/call", s.handleCall)
	mux.HandleFunc("/api/exec/stream", s.handleExecStream)
	mux.HandleFunc("/api/disconnect", s.handleDisconnect)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/download", s.handleDownload)
	return s.guard(mux)
}

// guard 是所有请求的第一道关卡。
//
// 为什么必须有：这个 GUI 能在**远端机器上执行任意命令**，而「只监听 127.0.0.1」在浏览器
// 场景下**根本不是防护**：
//  1. 用户浏览的任意网页都能往 127.0.0.1 发跨站 POST。Content-Type: text/plain 属于 CORS
//     安全列表类型、不触发预检，浏览器会真投递；后端一解析就执行了。攻击者读不到响应，
//     但「远端已经执行了命令」这个副作用无法挽回。
//  2. DNS rebinding：攻击者把自己的域名重绑到 127.0.0.1，此后即为同源，连响应都能读走。
//
// 三道关：
//   - token：随机生成，只出现在启动时打印的 URL 里。跨站攻击者无从得知，因此发不出
//     合法请求。API 优先认自定义头 X-Auth-Token——**自定义头必然触发 CORS 预检**，
//     跨站请求会先死在预检上（我们不回 CORS 头）。
//   - Host 白名单：只认 loopback，挡 DNS rebinding（重绑过来的请求 Host 是攻击者域名）。
//   - Origin 白名单：带了 Origin 且不是本机来源，直接拒。
//
// 首页(/)不校验 token：token 要靠首页 URL 的 query 交给前端，前端拿到后才能带着它调 API。
// 首页 HTML 本身不含任何机密，被人拿到也无所谓。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden: 只接受本机访问（防 DNS rebinding）。远程访问请用 ssh -L 端口转发。", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !loopbackOrigin(o) {
			http.Error(w, "forbidden: bad origin", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/" && !s.tokenOK(r) {
			http.Error(w, "forbidden: 缺少或错误的访问令牌（请用启动时打印的带 token 的地址打开）", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenOK 校验令牌。优先自定义头；EventSource 与下载用的 <a> 标签设不了自定义头，
// 只能退而求其次认 query 参数（令牌本身就是凭据，跨站方拿不到，故同样安全）。
func (s *Server) tokenOK(r *http.Request) bool {
	got := r.Header.Get("X-Auth-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	// 定长比较，避免按字节提前返回泄漏令牌前缀
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// loopbackHost 判断 Host 头是否指向本机。只看主机名、不管端口（端口由监听方决定）。
func loopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // 没带端口
	}
	return isLoopbackName(h)
}

func loopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackName(u.Hostname())
}

func isLoopbackName(h string) bool {
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// baseName 取路径最后一段。远端可能是 Windows 也可能是 Linux，而 filepath.Base 只认
// 本机的分隔符，所以这里两种都认。
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// handleDownload 把远端文件下载给浏览器。
//
// 不能用 read_file：它的结果经 humanize 后只剩 text 字段，二进制文件只会得到一个
// binary=true 标记，拿不到原始字节。download_file 是 host 端复合工具，走裸 bridge
// 分块拉取（自带重试/续传），落到本地临时文件后再流回浏览器——二进制与大文件都正确，
// 且下载进度由浏览器原生呈现。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	remotePath := r.URL.Query().Get("path")
	if remotePath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	client := s.client
	connected := s.connected
	s.mu.Unlock()
	if !connected || client == nil {
		http.Error(w, "not connected", http.StatusBadRequest)
		return
	}

	tmp, err := os.CreateTemp("", "ra-download-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if _, err := client.CallTool(ctx, "download_file", map[string]any{
		"remote_path": remotePath,
		"local_path":  tmpPath,
	}); err != nil {
		http.Error(w, "download failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	name := baseName(remotePath)
	w.Header().Set("Content-Type", "application/octet-stream")
	// filename* 用 RFC 5987 编码，中文名不会乱码
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	io.Copy(w, f)
}

func (s *Server) broadcast(msg string) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for ch := range s.sseSubs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(assets.IndexHTML)
}

// handleConnect 建立到远端 share 的 MCP 隧道。
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Code   string `json:"code"`
		Server string `json:"server"`
		NoAuth bool   `json:"no_auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// 关掉旧的子进程（如果有的话），再起新的
	s.mu.Lock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
		s.connected = false
	}
	s.mu.Unlock()

	// bootstrap 模式：子进程进入等待 connect 工具的状态，不预先传 --code / --no-auth，
	// 否则会走 NewHelpModeMCP（无 connect 工具）。server/code/no_auth 全部通过 connect
	// 工具传入，支持动态连接与重连。
	var spawnArgs []string
	if req.Server != "" {
		spawnArgs = append(spawnArgs, "--server", req.Server)
	} else if s.defaultServer != "" {
		spawnArgs = append(spawnArgs, "--server", s.defaultServer)
	}

	client, err := NewMCPClient(s.binPath, spawnArgs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		client.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "initialize failed: " + err.Error()})
		return
	}

	// connect 工具建立隧道
	connectArgs := map[string]any{}
	if req.NoAuth {
		connectArgs["no_auth"] = true
	} else {
		connectArgs["code"] = req.Code
	}
	if req.Server != "" {
		connectArgs["server"] = req.Server
	}
	res, err := client.CallTool(ctx, "connect", connectArgs)
	if err != nil {
		// connect 失败：不杀子进程（MCP server 仍存活在 bootstrap 状态），
		// 用户修正参数后可再次点击连接（下次进来会先关掉这个旧进程再起新的）。
		s.mu.Lock()
		s.client = client
		s.connected = false
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "connect failed: " + err.Error()})
		return
	}

	s.mu.Lock()
	s.client = client
	s.connected = true
	// 缓存连接参数，供子进程意外退出时自动重连
	s.lastCode = req.Code
	s.lastServer = req.Server
	s.lastNoAuth = req.NoAuth
	if !s.guardStarted {
		s.guardStarted = true
		go s.guardLoop()
	}
	s.mu.Unlock()

	s.broadcast("event: connected\n")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": json.RawMessage(res)})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
		s.connected = false
	}
	s.mu.Unlock()
	s.broadcast("event: disconnected\n")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// guardLoop 守护 MCP 子进程：一旦它意外退出（远端断线、relay 抖动、OOM 等），
// 若此前已成功连接，则自动用缓存参数重建子进程并重连，避免整个后端"死掉"。
// 用户主动断开（connected=false）时不重连。
func (s *Server) guardLoop() {
	for {
		s.mu.Lock()
		client := s.client
		s.mu.Unlock()
		if client == nil {
			time.Sleep(time.Second)
			continue
		}
		<-client.Done()

		s.mu.Lock()
		// 这条已死的 client 可能早已被 handleConnect / tryReconnect 换成新的了。
		// 无条件清空会把刚建好的新连接误杀，所以只在它仍是当前 client 时才收拾。
		if s.client != client {
			s.mu.Unlock()
			continue
		}
		wasConnected := s.connected
		code, server, noAuth := s.lastCode, s.lastServer, s.lastNoAuth
		s.connected = false
		s.client = nil
		s.mu.Unlock()
		// 必须回收：Done 只代表 readLoop 看到子进程 stdout 关了，cmd.Wait 还没跑过——
		// 不 Close 就每断一次线留一个僵尸进程 + 一对泄漏的管道 fd。Close 是幂等的。
		client.Close()

		if !wasConnected {
			s.broadcast("event: disconnected\n")
			continue
		}

		// 自动重连，轻量退避，最多 6 次（约 1+2+3+4+5+5 = 20s）
		s.broadcast("event: reconnecting\n")
		reconnected := false
		backoff := []time.Duration{1, 2, 3, 4, 5, 5}
		for attempt, d := range backoff {
			time.Sleep(d * time.Second)
			if s.tryReconnect(code, server, noAuth) {
				reconnected = true
				break
			}
			s.broadcast(fmt.Sprintf("event: reconnect-fail\nretry %d/%d\n", attempt+1, len(backoff)))
		}
		if !reconnected {
			s.broadcast("event: lost\n")
		}
	}
}

// tryReconnect 用缓存参数重建子进程并完成 connect，成功返回 true。
func (s *Server) tryReconnect(code, server string, noAuth bool) bool {
	s.mu.Lock()
	binPath := s.binPath
	defSrv := s.defaultServer
	s.mu.Unlock()

	spawnArgs := []string{}
	if server != "" {
		spawnArgs = append(spawnArgs, "--server", server)
	} else if defSrv != "" {
		spawnArgs = append(spawnArgs, "--server", defSrv)
	}
	client, err := NewMCPClient(binPath, spawnArgs)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		client.Close()
		return false
	}
	connectArgs := map[string]any{}
	if noAuth {
		connectArgs["no_auth"] = true
	} else {
		connectArgs["code"] = code
	}
	if server != "" {
		connectArgs["server"] = server
	}
	if _, err := client.CallTool(ctx, "connect", connectArgs); err != nil {
		client.Close()
		return false
	}
	s.mu.Lock()
	s.client = client
	s.connected = true
	s.mu.Unlock()
	s.broadcast("event: connected\n")
	return true
}

// handleCall 转发一次工具调用。body: {tool, args}
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Tool string                 `json:"tool"`
		Args map[string]interface{} `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	s.mu.Lock()
	client := s.client
	connected := s.connected
	s.mu.Unlock()
	if !connected || client == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "not connected; call /api/connect first"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := client.CallTool(ctx, req.Tool, req.Args)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": json.RawMessage(res)})
}

// handleExecStream 跑一条 exec，并把它的输出边跑边流回浏览器。
//
// 响应是 NDJSON（一行一个 JSON 对象）而不是 SSE：SSE 的 EventSource 只能发 GET，命令的
// argv 塞进 query string 既难编码又有长度上限；前端用 fetch + ReadableStream 读同样简单。
// 也不复用 /api/events——那是广播给所有页面的连接状态流，终端输出必须只回给发起的那次请求。
//
//	{"type":"chunk","stream":"stdout","data":"<base64>"}
//	{"type":"result","result":{...}}   // 收尾，带 exit_code
//	{"type":"error","error":"..."}
func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Argv      []string `json:"argv"`
		Cwd       string   `json:"cwd"`
		TimeoutMs int      `json:"timeout_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if len(req.Argv) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "argv required"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	client := s.client
	connected := s.connected
	s.mu.Unlock()
	if !connected || client == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "not connected; call /api/connect first"})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff") // 别让浏览器嗅探/缓冲，块要立刻可见
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

	args := map[string]any{"argv": req.Argv, "stream": true}
	if req.Cwd != "" {
		args["cwd"] = req.Cwd
	}
	if req.TimeoutMs > 0 {
		args["timeout_ms"] = req.TimeoutMs
	}

	// 用 r.Context()：浏览器关页时它被取消 → callStream 发 notifications/cancelled 给子进程
	// → mcp.Server 按 requestId 取消那次调用 → bridge 发 MsgToolCancel → share 端杀掉命令。
	// 这条链**每一环都得有人接**：早先这里想当然写着「取消会一路传到 bridge」，但 callStream
	// 当时只是 return，通知从没发出去，远端命令关页后照跑不误。
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	res, err := client.CallToolStream(ctx, "exec", args, func(stream string, data []byte) {
		send(map[string]any{"type": "chunk", "stream": stream, "data": data})
	})
	if err != nil {
		send(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	send(map[string]any{"type": "result", "result": json.RawMessage(res)})
}

// handleEvents SSE 流，推送连接状态变化与子进程日志。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 16)
	s.sseMu.Lock()
	s.sseSubs[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseSubs, ch)
		s.sseMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "%s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, "event: ping\n\n")
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
