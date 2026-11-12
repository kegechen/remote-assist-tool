package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readCall 读 client 发来的一条 tools/call，返回 id、工具名、参数。
func (s *fakeServer) readCall(t *testing.T) (int64, string, map[string]any) {
	t.Helper()
	if !s.fromClient.Scan() {
		t.Fatal("client 没发出请求")
	}
	var r struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(s.fromClient.Bytes(), &r); err != nil {
		t.Fatalf("解析请求: %v", err)
	}
	return r.ID, r.Params.Name, r.Params.Arguments
}

func ptyTestServer(t *testing.T) (*Server, http.Handler, *fakeServer) {
	t.Helper()
	c, fs := newFakeClient()
	s := NewServer("nonexistent-remote-bin", "")
	s.client = c
	s.connected = true
	return s, s.Routes(), fs
}

// 并发安全的 ResponseRecorder 替代：用真实 httptest.Server 读 NDJSON 流。
func TestPTYOpenStreamsChunksAndResult(t *testing.T) {
	s, h, fs := ptyTestServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/pty/open", strings.NewReader(`{"cols":100,"rows":30}`))
	req.Header.Set("X-Auth-Token", s.Token())
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("open: %v", err)
		}
		respCh <- resp
	}()

	id, name, args := fs.readCall(t)
	if name != "pty_open" || args["stream"] != true || args["cols"].(float64) != 100 || args["rows"].(float64) != 30 {
		t.Fatalf("pty_open 参数不对: %s %v", name, args)
	}
	sess, _ := args["session"].(string)
	if sess == "" {
		t.Fatal("应带 session id")
	}
	fs.notify(t, id, "stdout", "hello\x03\xff")
	fs.respond(t, id, `{"content":[{"type":"text","text":"{\"exit_code\":0,\"reason\":\"exited\"}"}]}`)

	resp := <-respCh
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("Content-Type=%s", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var types []string
	var got []byte
	for sc.Scan() {
		var m struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Data []byte `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		types = append(types, m.Type)
		if m.Type == "session" && m.ID != sess {
			t.Fatalf("session id 不一致 %q vs %q", m.ID, sess)
		}
		got = append(got, m.Data...)
	}
	if strings.Join(types, ",") != "session,chunk,result" {
		t.Fatalf("事件序列=%v", types)
	}
	if string(got) != "hello\x03\xff" {
		t.Fatalf("块内容应字节原样: %q", got)
	}
	// 流结束后应补一次幂等 pty_close
	_, name2, args2 := fs.readCall(t)
	if name2 != "pty_close" || args2["session"] != sess {
		t.Fatalf("结束后应补 pty_close: %s %v", name2, args2)
	}
}

func TestPTYBrowserDisconnectCancelsRemote(t *testing.T) {
	s, h, fs := ptyTestServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/pty/open", strings.NewReader(`{}`))
	req.Header.Set("X-Auth-Token", s.Token())
	go func() { http.DefaultClient.Do(req) }()
	id, name, _ := fs.readCall(t)
	if name != "pty_open" {
		t.Fatal(name)
	}
	_ = id
	time.Sleep(100 * time.Millisecond)
	cancel() // 浏览器关页
	// 期望：向子进程发 notifications/cancelled，随后补发 pty_close
	var sawCancel, sawClose bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2 && fs.fromClient.Scan(); i++ {
			line := fs.fromClient.Text()
			if strings.Contains(line, "notifications/cancelled") {
				sawCancel = true
			}
			if strings.Contains(line, `"pty_close"`) {
				sawClose = true
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("浏览器断开后未向远端发出取消/关闭")
	}
	if !sawCancel || !sawClose {
		t.Fatalf("cancel=%v close=%v", sawCancel, sawClose)
	}
}

func TestPTYWriteResizeCloseRoutes(t *testing.T) {
	s, h, fs := ptyTestServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	// 手工登记一个会话（避免依赖 open 流）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ptySessions.Store("sess-test", &ptyEntry{client: s.client, cancel: cancel})
	defer ptySessions.Delete("sess-test")

	do := func(path, body string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Auth-Token", s.Token())
		return http.DefaultClient.Do(req)
	}
	// write：二进制安全 + 输入不应进日志（这里只验证透传）
	go func() {
		id, name, args := fs.readCall(t)
		if name != "pty_write" || args["session"] != "sess-test" {
			t.Errorf("write 调用不对: %s %v", name, args)
		}
		fs.respond(t, id, `{"content":[]}`)
	}()
	resp, err := do("/api/pty/write", `{"id":"sess-test","data":"AwEC"}`)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("write: %v %v", err, resp)
	}
	// 未知会话 404
	resp, _ = do("/api/pty/write", `{"id":"nope","data":"AA=="}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知会话应 404: %d", resp.StatusCode)
	}
	// resize 参数校验
	for _, bad := range []string{`{"id":"sess-test","cols":0,"rows":10}`, `{"id":"sess-test","cols":10,"rows":99999}`} {
		resp, _ = do("/api/pty/resize", bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d", bad, resp.StatusCode)
		}
	}
	go func() {
		id, name, args := fs.readCall(t)
		if name != "pty_resize" || args["cols"].(float64) != 120 {
			t.Errorf("resize 调用不对: %s %v", name, args)
		}
		fs.respond(t, id, `{"content":[]}`)
	}()
	resp, _ = do("/api/pty/resize", `{"id":"sess-test","cols":120,"rows":40}`)
	if resp.StatusCode != 200 {
		t.Fatalf("resize: %d", resp.StatusCode)
	}
	// close 幂等：未知会话也 200
	resp, _ = do("/api/pty/close", `{"id":"nope"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("重复 close 应 200: %d", resp.StatusCode)
	}
	go func() {
		id, name, _ := fs.readCall(t)
		if name != "pty_close" {
			t.Errorf("close 调用不对: %s", name)
		}
		fs.respond(t, id, `{"content":[]}`)
	}()
	resp, _ = do("/api/pty/close", `{"id":"sess-test"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("close: %d", resp.StatusCode)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("close 应结束 open 流（取消其 ctx）")
	}
}

func TestPTYRoutesRequireToken(t *testing.T) {
	_, h := testServer(t)
	for _, p := range []string{"/api/pty/open", "/api/pty/write", "/api/pty/resize", "/api/pty/close"} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`))
		req.Host = "127.0.0.1:8731"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s 无令牌应 403: %d", p, w.Code)
		}
	}
}

func TestAssetsServedLocallyWithGuard(t *testing.T) {
	_, h := testServer(t)
	get := func(path, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for path, ct := range map[string]string{
		"/assets/xterm/xterm.js":      "text/javascript",
		"/assets/xterm/xterm.css":     "text/css",
		"/assets/xterm/addon-fit.js":  "text/javascript",
		"/assets/xterm/LICENSE-xterm": "text/plain",
		"/assets/pty/pty_term.js":     "text/javascript",
	} {
		w := get(path, "127.0.0.1:8731")
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), ct) || w.Body.Len() == 0 {
			t.Errorf("%s: code=%d ct=%s len=%d", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
	if w := get("/assets/xterm/xterm.js", "evil.example.com"); w.Code != http.StatusForbidden {
		t.Errorf("非本机 Host 应 403: %d", w.Code)
	}
	for _, p := range []string{"/assets/../server.go", "/assets/nope.js", "/assets/%2e%2e/assets.go"} {
		if w := get(p, "127.0.0.1:8731"); w.Code == 200 {
			t.Errorf("%s 不应 200", p)
		}
	}
	// 静态资源放行不应连带放行 API
	if w := get("/api/status", "127.0.0.1:8731"); w.Code != http.StatusForbidden {
		t.Errorf("/api/status 无令牌应仍 403: %d", w.Code)
	}
}
