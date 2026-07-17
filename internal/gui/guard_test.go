package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GUI 能在远端机器上执行任意命令，却是个本地 HTTP 服务——「只监听 127.0.0.1」在浏览器
// 场景下不是防护：用户随便打开的一个网页就能往 127.0.0.1 发跨站请求。这一组测试钉住
// guard 的三道关，任何一条被摘掉都会立刻失败。

func testServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := NewServer("nonexistent-remote-bin", "")
	return s, s.Routes()
}

// TestGuardRejectsRequestWithoutToken 无令牌 = 403。这是挡 CSRF 的主锁：跨站页面
// 拿不到令牌（它只出现在启动 URL 里），所以发不出合法请求。
func TestGuardRejectsRequestWithoutToken(t *testing.T) {
	_, h := testServer(t)
	for _, path := range []string{"/api/call", "/api/connect", "/api/exec/stream", "/api/disconnect", "/api/events", "/api/download?path=x"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Host = "127.0.0.1:8731"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s 无令牌应 403，实际 %d", path, w.Code)
		}
	}
}

// TestGuardRejectsCsrfWithSafelistedContentType 复现真实攻击：恶意网页用
// Content-Type: text/plain（CORS 安全列表类型，**不触发预检**，浏览器会真投递）
// POST 过来。后端若不校验，命令就在远端执行了——攻击者读不到响应，但副作用已发生。
func TestGuardRejectsCsrfWithSafelistedContentType(t *testing.T) {
	_, h := testServer(t)
	body := `{"tool":"exec","args":{"argv":["cmd","/c","calc"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/call", strings.NewReader(body))
	req.Host = "127.0.0.1:8731"
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("跨站免预检请求应 403，实际 %d（远端命令可能已被执行）", w.Code)
	}
}

// TestGuardRejectsForeignOrigin 带外部 Origin 的请求一律拒，即便令牌对（防御纵深）。
func TestGuardRejectsForeignOrigin(t *testing.T) {
	s, h := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/call", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8731"
	req.Header.Set("X-Auth-Token", s.Token())
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("外部 Origin 应 403，实际 %d", w.Code)
	}
}

// TestGuardRejectsRebindingHost DNS rebinding：攻击者把自己的域名重绑到 127.0.0.1，
// 此后浏览器认为同源、连响应都能读走。此时请求的 Host 是攻击者域名——只认 loopback
// 即可挡掉。连首页都要挡，否则重绑后能先拿页面再找别的口子。
func TestGuardRejectsRebindingHost(t *testing.T) {
	s, h := testServer(t)
	for _, path := range []string{"/", "/api/call"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Host = "attacker-rebound.example" // 重绑到 127.0.0.1 的域名
		req.Header.Set("X-Auth-Token", s.Token())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s rebinding Host 应 403，实际 %d", path, w.Code)
		}
	}
}

// TestGuardAllowsLegitimateRequest 正常前端请求（本机 Host + 正确令牌）必须放行——
// 别把锁修成谁都进不来。
func TestGuardAllowsLegitimateRequest(t *testing.T) {
	s, h := testServer(t)
	for _, host := range []string{"127.0.0.1:8731", "localhost:8731", "[::1]:8731"} {
		req := httptest.NewRequest(http.MethodPost, "/api/call", strings.NewReader(`{"tool":"stat","args":{}}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Auth-Token", s.Token())
		req.Header.Set("Origin", "http://"+host)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// 未连接远端 → 400；关键是别被 guard 拦成 403
		if w.Code == http.StatusForbidden {
			t.Errorf("Host=%s 的正常请求被误拒: %s", host, w.Body.String())
		}
	}
}

// TestGuardAcceptsQueryToken EventSource 与下载用的 <a> 设不了自定义头，只能靠 query
// 带令牌，这条路必须通。
func TestGuardAcceptsQueryToken(t *testing.T) {
	s, h := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/download?path=/tmp/x&token="+s.Token(), nil)
	req.Host = "127.0.0.1:8731"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Fatalf("query 令牌应放行，实际 403: %s", w.Body.String())
	}
}

// TestGuardServesIndexWithoutToken 首页不校验令牌：令牌要靠首页 URL 的 query 交给前端。
// 首页 HTML 本身不含机密，被看到无所谓。
func TestGuardServesIndexWithoutToken(t *testing.T) {
	_, h := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:8731"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("首页应可直接打开，实际 %d", w.Code)
	}
	if strings.Contains(w.Body.String(), NewServer("x", "").Token()) {
		t.Fatal("首页 HTML 里不该内嵌令牌")
	}
}

// TestTokensAreUnique 每个进程一个随机令牌；若退化成固定值，攻击者猜到即可绕过全部防线。
func TestTokensAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tok := NewServer("x", "").Token()
		if len(tok) != 32 {
			t.Fatalf("令牌长度 %d，期望 32", len(tok))
		}
		if seen[tok] {
			t.Fatal("令牌重复——不是随机生成的")
		}
		seen[tok] = true
	}
}
