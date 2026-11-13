package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/gui/assets"
)

func connectHang(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	t.Setenv("RAT_GUI_FAKE_MCP_LOG", t.TempDir()+"/unused.json")
	t.Setenv("RAT_GUI_FAKE_MCP_HANG", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(exe, "")
	return s, http.HandlerFunc(s.handleConnect)
}

func postConnect(t *testing.T, s *Server, h http.Handler, ctx context.Context) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(`{"code":"ABCD-EFGHIJ","server":"192.0.2.1:8443"}`)).WithContext(ctx)
	req.Header.Set("X-Auth-Token", s.Token())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v %q", err, rec.Body.String())
	}
	return out
}

// 后端总时限到点（对端黑洞、connect 一直不返回）必须归为“连接超时”，而不是“已取消”。
func TestConnectTotalTimeoutReportedAsTimeoutNotCanceled(t *testing.T) {
	old := connectTotalTimeout
	connectTotalTimeout = 1500 * time.Millisecond
	defer func() { connectTotalTimeout = old }()
	s, h := connectHang(t)
	out := postConnect(t, s, h, context.Background())
	if out["code"] != reasonTimeout || out["reason"] != "连接超时" {
		t.Fatalf("总时限超时应报 timeout/连接超时，实际 %v", out)
	}
}

// 用户主动取消（请求被取消）仍是“已取消”。
func TestConnectUserCancelStillCanceled(t *testing.T) {
	s, h := connectHang(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	out := postConnect(t, s, h, ctx)
	if out["code"] != reasonCanceled || out["reason"] != "已取消" {
		t.Fatalf("用户取消应报 canceled/已取消，实际 %v", out)
	}
}

// 前端兜底超时 abort 不得被当成用户取消：要区分 timedOut。端到端行为由 Playwright 验证，这里锁住接线。
func TestConnectFrontendDistinguishesTimeoutFromCancel(t *testing.T) {
	html := string(assets.IndexHTML)
	for _, want := range []string{
		"let timedOut=false",
		"setTimeout(()=>{timedOut=true;ctrl.abort()},CONNECT_ABORT_MS)",
		"timedOut?{ok:false,code:'timeout',reason:'连接超时'}:{ok:false,code:'canceled'}",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}
}
