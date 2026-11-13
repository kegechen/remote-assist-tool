package gui

import (
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/gui/assets"
)

// 终端页“会话中断”红字：只有意外断开才显示，且文案与状态栏的断线原因一致；
// 主动断开/取消/改连时必须先停会话（gen++），走不到提示分支。行为由 Playwright 端到端验证，
// 这里锁住关键接线，防止回归成无条件红字。
func TestPtyDropNoticeWiring(t *testing.T) {
	b, err := assets.StaticFS.ReadFile("pty/pty_term.js")
	if err != nil {
		t.Fatal(err)
	}
	js := strings.ReplaceAll(string(b), "\r\n", "\n")
	for _, want := range []string{
		"isConnDropError(errText)", // 连接级错误走分类文案
		"[连接中断: ",                  // 新文案
		"dropGen !== gen",          // 延迟提示前确认会话未被主动停止/重开
		"if (state === 'live' || state === 'opening') stopSession();", // 后端 disconnected 事件静默收会话
	} {
		if !strings.Contains(js, want) {
			t.Errorf("pty_term.js 缺少 %q", want)
		}
	}
	if i, j := strings.Index(js, "isConnDropError(errText)"), strings.Index(js, "[会话中断: "); i < 0 || j < i {
		t.Error("通用会话中断文案必须位于连接级分支之后，连接级错误不得落入其中")
	}
}

// 主动断开/发起新连接前先停终端会话；断线事件携带原因并分别展示。
func TestIndexDropWiring(t *testing.T) {
	html := strings.ReplaceAll(string(assets.IndexHTML), "\r\n", "\n")
	i := strings.Index(html, "async function doDisconnect(){")
	if i < 0 {
		t.Fatal("找不到 doDisconnect")
	}
	seg := html[i : i+400]
	if p, a := strings.Index(seg, "PtyTerm.disconnected()"), strings.Index(seg, "api('/api/disconnect'"); p < 0 || a < 0 || p > a {
		t.Error("doDisconnect 必须先 PtyTerm.disconnected() 再调 /api/disconnect")
	}
	for _, want := range []string{"es.addEventListener('reconnect-fail'", "parseDrop(e)", "function dropFromErr"} {
		if !strings.Contains(html, want) {
			t.Errorf("index 缺少 %q", want)
		}
	}
}

// 被控端因空闲回收会话时，result.reason=idle 要给出明确文案，而不是笼统的“进程已退出”。
func TestPtyIdleNoticeWiring(t *testing.T) {
	b, err := assets.StaticFS.ReadFile("pty/pty_term.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{"endReason === 'idle'", "终端因长时间空闲已关闭"} {
		if !strings.Contains(js, want) {
			t.Errorf("pty_term.js 缺少 %q", want)
		}
	}
}

// Ctrl+C 等控制字符必须立即单独发送，不能并入 6ms 批次排在大块写入后面。
func TestPtyUrgentInputWiring(t *testing.T) {
	b, err := assets.StaticFS.ReadFile("pty/pty_term.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{"function isUrgentInput(bytes)", "if (sessionId && isUrgentInput(bytes))"} {
		if !strings.Contains(js, want) {
			t.Errorf("pty_term.js 缺少 %q", want)
		}
	}
}
