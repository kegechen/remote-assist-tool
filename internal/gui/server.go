package gui

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/gui/assets"
)

// Server 是 GUI 的后端：托管前端静态页 + 通过 JSON 把请求转发给 MCP 子进程。
type Server struct {
	binPath       string
	defaultServer string
	token         string // 启动时随机生成，见 guard

	mu         sync.Mutex
	client     *MCPClient
	connected  bool
	// connGen 每次用户主动改变连接意图（connect/disconnect）时自增。守护线程发起自动
	// 重连时捕获它，安装前若发现已变，说明用户在退避/连接期间自己接管了连接，这次过期
	// 重连必须放弃、绝不覆盖用户当前的 client。
	connGen    uint64
	serverArgs []string

	// 守护：缓存最近一次成功的连接参数，子进程意外退出时自动重连
	lastCode     string
	lastServer   string
	lastNoAuth   bool
	peerVersion  string
	helpVersion  string
	peerHost     string
	effectiveSrv string
	sessionID    string
	p2p          bool
	connectedAt  time.Time
	guardStarted bool
	upgradeMu    sync.Mutex
	upgrading    atomic.Bool
	previewCache string
	previewMu    sync.Mutex
	previewLoads map[string]*previewLoad

	// SSE 广播
	sseMu   sync.Mutex
	sseSubs map[chan string]struct{}
}

type previewLoad struct {
	done chan struct{}
	err  error
}

// NewServer 创建一个 GUI 后端。binPath 为 remote 可执行文件路径。
func NewServer(binPath, defaultServer string) *Server {
	return &Server{
		binPath:       binPath,
		defaultServer: defaultServer,
		token:         randomToken(),
		sseSubs:       make(map[chan string]struct{}),
		previewCache:  defaultPreviewCacheDir(),
		previewLoads:  make(map[string]*previewLoad),
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
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/upgrade", s.handleUpgrade)
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

func defaultPreviewCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "remote-assist-tool", "image-previews-v1")
}

var md5TextRE = regexp.MustCompile(`(?i)^[0-9a-f]{32}$`)
var md5InTextRE = regexp.MustCompile(`(?i)[0-9a-f]{32}`)

func normalizeMD5(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if md5TextRE.MatchString(value) {
		return value
	}
	return ""
}

func parseMD5Output(stdout string) string {
	for _, line := range strings.Split(strings.ReplaceAll(stdout, " ", ""), "\n") {
		if digest := normalizeMD5(strings.TrimSpace(line)); digest != "" {
			return digest
		}
		if digest := md5InTextRE.FindString(line); digest != "" {
			return strings.ToLower(digest)
		}
	}
	fields := strings.Fields(stdout)
	if len(fields) > 0 {
		return normalizeMD5(fields[0])
	}
	return ""
}

func remoteFileMD5(ctx context.Context, client *MCPClient, remotePath string) (string, error) {
	var result struct {
		MD5 string `json:"md5"`
	}
	toolErr := callRemoteJSON(ctx, client, "file_md5", map[string]any{"path": remotePath}, &result)
	if digest := normalizeMD5(result.MD5); toolErr == nil && digest != "" {
		return digest, nil
	}
	var lastErr = toolErr
	encodedPath := base64.StdEncoding.EncodeToString([]byte(remotePath))
	powerShellHash := fmt.Sprintf("$p=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'));(Get-FileHash -LiteralPath $p -Algorithm MD5).Hash", encodedPath)
	commands := [][]string{
		{"md5sum", "--", remotePath},
		{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", powerShellHash},
		{"md5", "-q", remotePath},
		{"certutil.exe", "-hashfile", remotePath, "MD5"},
	}
	for _, argv := range commands {
		var execResult remoteExecResult
		err := callRemoteJSON(ctx, client, "exec", map[string]any{
			"argv": argv, "timeout_ms": 5 * 60 * 1000, "max_output_bytes": 65536,
		}, &execResult)
		if err == nil && execResult.ExitCode == 0 {
			if digest := parseMD5Output(execResult.Stdout); digest != "" {
				return digest, nil
			}
			err = errors.New("MD5 command returned no digest")
		}
		if err == nil {
			err = errors.New(strings.TrimSpace(execResult.Error + " " + execResult.Stderr))
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("remote MD5 is unavailable")
	}
	return "", lastErr
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := md5.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func cachePreviewDownload(cacheDir, digest string, download func(string) error) (string, error) {
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return "", err
	}
	finalPath := filepath.Join(cacheDir, digest)
	if stat, err := os.Stat(finalPath); err == nil && stat.Mode().IsRegular() {
		return finalPath, nil
	}
	tmp, err := os.CreateTemp(cacheDir, ".preview-*.part")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	defer os.Remove(tmpPath)
	if err := download(tmpPath); err != nil {
		return "", fmt.Errorf("download preview: %w", err)
	}
	localDigest, err := fileMD5(tmpPath)
	if err != nil {
		return "", err
	}
	if localDigest != digest {
		return "", fmt.Errorf("preview MD5 mismatch: remote %s, downloaded %s", digest, localDigest)
	}
	_ = os.Chmod(tmpPath, 0600)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if stat, statErr := os.Stat(finalPath); statErr == nil && stat.Mode().IsRegular() {
			return finalPath, nil
		}
		return "", fmt.Errorf("commit preview cache: %w", err)
	}
	return finalPath, nil
}

func downloadAndCachePreview(ctx context.Context, client *MCPClient, cacheDir, remotePath, digest string) (string, error) {
	return cachePreviewDownload(cacheDir, digest, func(tmpPath string) error {
		_, err := client.CallTool(ctx, "download_file", map[string]any{"remote_path": remotePath, "local_path": tmpPath})
		return err
	})
}

func (s *Server) cachedPreview(ctx context.Context, client *MCPClient, remotePath, digest string) (string, bool, error) {
	cachePath := filepath.Join(s.previewCache, digest)
	if stat, err := os.Stat(cachePath); err == nil && stat.Mode().IsRegular() {
		return cachePath, true, nil
	}
	s.previewMu.Lock()
	if load := s.previewLoads[digest]; load != nil {
		s.previewMu.Unlock()
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-load.done:
			if load.err != nil {
				return "", false, load.err
			}
			return cachePath, true, nil
		}
	}
	load := &previewLoad{done: make(chan struct{})}
	s.previewLoads[digest] = load
	s.previewMu.Unlock()

	path, err := downloadAndCachePreview(ctx, client, s.previewCache, remotePath, digest)
	s.previewMu.Lock()
	load.err = err
	close(load.done)
	delete(s.previewLoads, digest)
	s.previewMu.Unlock()
	return path, false, err
}

// stageRemoteFile 通过裸 bridge 把远端文件分块拉到本地临时文件。下载和图片预览共用
// 这条路径，二进制、大文件以及传输重试/续传的行为保持一致。
func (s *Server) stageRemoteFile(w http.ResponseWriter, r *http.Request, remotePath string) (string, bool) {
	if s.rejectWhileUpgrading(w) {
		return "", false
	}
	s.mu.Lock()
	client := s.client
	connected := s.connected
	s.mu.Unlock()
	if !connected || client == nil {
		http.Error(w, "not connected", http.StatusBadRequest)
		return "", false
	}

	tmp, err := os.CreateTemp("", "ra-download-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return "", false
	}
	tmpPath := tmp.Name()
	tmp.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if _, err := client.CallTool(ctx, "download_file", map[string]any{
		"remote_path": remotePath,
		"local_path":  tmpPath,
	}); err != nil {
		os.Remove(tmpPath)
		http.Error(w, "download failed: "+err.Error(), http.StatusBadGateway)
		return "", false
	}
	return tmpPath, true
}

// handleDownload 把远端文件作为附件下载给浏览器。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	remotePath := r.URL.Query().Get("path")
	if remotePath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	tmpPath, ok := s.stageRemoteFile(w, r, remotePath)
	if !ok {
		return
	}
	defer os.Remove(tmpPath)

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

// previewImageContentType 只放行浏览器普遍支持的光栅图片。尤其不能把 SVG 当作同源内容
// 返回：SVG 可以包含脚本，若直接挂在本地 GUI 的 origin 下会扩大攻击面。
func previewImageContentType(header []byte) (string, bool) {
	contentType := http.DetectContentType(header)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return contentType, true
	default:
		return "", false
	}
}

// handlePreview 校验文件的实际内容类型后以内联图片返回。扩展名只供前端决定是否发起
// 预览，后端不信任扩展名，避免把改名后的 HTML/SVG 作为同源可执行内容送给浏览器。
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	remotePath := r.URL.Query().Get("path")
	if remotePath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	if s.rejectWhileUpgrading(w) {
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
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	digest, hashErr := remoteFileMD5(ctx, client, remotePath)
	cacheStatus := "bypass"
	previewPath := ""
	removeAfter := false
	if hashErr == nil {
		etag := `"` + digest + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		if r.URL.Query().Get("download") == "" && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		var cacheHit bool
		var err error
		previewPath, cacheHit, err = s.cachedPreview(ctx, client, remotePath, digest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if cacheHit {
			cacheStatus = "hit"
		} else {
			cacheStatus = "miss"
		}
	} else {
		var ok bool
		previewPath, ok = s.stageRemoteFile(w, r, remotePath)
		if !ok {
			return
		}
		removeAfter = true
		w.Header().Set("Cache-Control", "no-store")
	}
	if removeAfter {
		defer os.Remove(previewPath)
	}

	f, err := os.Open(previewPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	header := make([]byte, 512)
	n, err := f.Read(header)
	if err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	contentType, ok := previewImageContentType(header[:n])
	if !ok {
		http.Error(w, "unsupported image format", http.StatusUnsupportedMediaType)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	name := baseName(remotePath)
	w.Header().Set("Content-Type", contentType)
	disposition := "inline"
	if r.URL.Query().Get("download") != "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Preview-Cache", cacheStatus)
	http.ServeContent(w, r, name, time.Time{}, f)
}

// handleStatus 返回当前连接的稳定元数据，并通过一次小型远端调用测量当前通道 RTT。
// process_list 在旧 share 上也存在，因此连接 0.0.5 时仍能显示延迟。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	client := s.client
	connected := s.connected
	result := map[string]any{
		"ok": connected, "connected": connected, "server": s.effectiveSrv, "session_id": s.sessionID,
		"peer_host": s.peerHost, "peer_version": s.peerVersion, "help_version": s.helpVersion, "p2p": s.p2p,
	}
	if !s.connectedAt.IsZero() {
		result["connected_seconds"] = int64(time.Since(s.connectedAt).Seconds())
	}
	s.mu.Unlock()
	if !connected || client == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	started := time.Now()
	_, err := client.CallTool(ctx, "process_list", map[string]any{"max_count": 1})
	if err == nil {
		latency := time.Since(started).Milliseconds()
		if latency < 1 {
			latency = 1
		}
		result["latency_ms"] = latency
	} else {
		result["latency_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, result)
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
	if s.rejectWhileUpgrading(w) {
		return
	}
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

	// 关掉旧的子进程（如果有的话），再起新的。同时自增 connGen：用户发起了新的连接意图，
	// 任何正在退避/连接中的过期自动重连都应就此作废，不得回头覆盖这次连接。
	s.mu.Lock()
	if s.client != nil {
		s.client.Close()
		s.client = nil
		s.connected = false
	}
	s.connGen++
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
	s.recordConnectMetadata(res, req.Code, req.Server, req.NoAuth)
	if !s.guardStarted {
		s.guardStarted = true
		go s.guardLoop()
	}
	s.mu.Unlock()

	s.broadcast("event: connected\n")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": json.RawMessage(res)})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.rejectWhileUpgrading(w) {
		return
	}
	s.mu.Lock()
	// 无条件自增：即便守护线程已把 client 清成 nil，用户"保持断开"的意图也必须让正在
	// 途中的自动重连作废，否则重连会把连接凭空恢复。
	s.connGen++
	if s.client != nil {
		s.client.Close()
		s.client = nil
		s.connected = false
		s.peerVersion = ""
		s.helpVersion = ""
		s.peerHost = ""
		s.effectiveSrv = ""
		s.sessionID = ""
		s.p2p = false
		s.connectedAt = time.Time{}
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
		gen := s.connGen
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
		outcome := reconnectFailed
		backoff := []time.Duration{1, 2, 3, 4, 5, 5}
		for attempt, d := range backoff {
			time.Sleep(d * time.Second)
			outcome = s.tryReconnect(gen, code, server, noAuth)
			if outcome != reconnectFailed {
				break
			}
			s.broadcast(fmt.Sprintf("event: reconnect-fail\nretry %d/%d\n", attempt+1, len(backoff)))
		}
		// reconnectSuperseded：用户在退避/连接期间自己接管了连接（主动断开或改连别的
		// share）。别再广播 lost 盖掉用户当前看到的状态；仅在重连确实彻底失败时才报 lost。
		if outcome == reconnectFailed {
			s.broadcast("event: lost\n")
		}
	}
}

// reconnectOutcome 表示一次自动重连尝试的结果。
type reconnectOutcome int

const (
	reconnectFailed     reconnectOutcome = iota // 瞬时失败，可继续退避重试
	reconnectOK                                 // 已重连并安装为当前 client
	reconnectSuperseded                         // 用户已接管连接意图，放弃这次过期重连
)

// tryReconnect 用缓存参数重建子进程并完成 connect。gen 是守护线程发起重连时捕获的连接
// 代次：建好连接后若 s.connGen 已变，说明用户在此期间主动断开或改连了别的 share，这条
// 过期连接必须丢弃、绝不覆盖用户当前的 s.client（否则后续命令会发错机器、还会泄漏用户
// 刚建的连接）。
func (s *Server) tryReconnect(gen uint64, code, server string, noAuth bool) reconnectOutcome {
	s.mu.Lock()
	binPath := s.binPath
	defSrv := s.defaultServer
	superseded := s.connGen != gen
	s.mu.Unlock()
	// 用户已接管：连子进程都不必建。
	if superseded {
		return reconnectSuperseded
	}

	spawnArgs := []string{}
	if server != "" {
		spawnArgs = append(spawnArgs, "--server", server)
	} else if defSrv != "" {
		spawnArgs = append(spawnArgs, "--server", defSrv)
	}
	client, err := NewMCPClient(binPath, spawnArgs)
	if err != nil {
		return reconnectFailed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		client.Close()
		return reconnectFailed
	}
	res, err := client.CallTool(ctx, "connect", connectToolArgs(code, server, noAuth))
	if err != nil {
		client.Close()
		return reconnectFailed
	}
	s.mu.Lock()
	// connect 可能耗时数秒，其间用户可能已接管——代次变了就丢弃这条连接、关掉子进程，
	// 绝不覆盖 s.client。
	if s.connGen != gen {
		s.mu.Unlock()
		client.Close()
		return reconnectSuperseded
	}
	s.client = client
	s.connected = true
	s.recordConnectMetadata(res, code, server, noAuth)
	s.mu.Unlock()
	s.broadcast("event: connected\n")
	return reconnectOK
}

// handleCall 转发一次工具调用。body: {tool, args}
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	if s.rejectWhileUpgrading(w) {
		return
	}
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
	if s.rejectWhileUpgrading(w) {
		return
	}
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
