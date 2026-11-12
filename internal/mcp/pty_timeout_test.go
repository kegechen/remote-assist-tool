package mcp

import (
	"testing"
	"time"
)

// pty_open 是无终止的长流，不能被 10 分钟兜底误杀；其余 pty 工具是快工具。
func TestFallbackTimeoutForPTY(t *testing.T) {
	if got := fallbackTimeoutFor("pty_open"); got < 12*time.Hour {
		t.Errorf("pty_open 兜底=%v，应为长会话", got)
	}
	for _, n := range []string{"pty_write", "pty_resize", "pty_close"} {
		if got := fallbackTimeoutFor(n); got != callToolQuickFallbackTimeout {
			t.Errorf("%s 兜底=%v，应为快工具", n, got)
		}
	}
}

func TestPTYSchemasRegistered(t *testing.T) {
	want := map[string]bool{"pty_open": false, "pty_write": false, "pty_resize": false, "pty_close": false}
	for _, s := range AllSchemas() {
		if _, ok := want[s.Name]; ok {
			want[s.Name] = true
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("schema 缺少 %s", n)
		}
	}
}
