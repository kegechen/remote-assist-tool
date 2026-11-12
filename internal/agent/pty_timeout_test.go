package agent

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultToolTimeoutPTY(t *testing.T) {
	if got := defaultToolTimeout("pty_open"); got != PTYOpenTimeout || got < 12*time.Hour {
		t.Errorf("pty_open 超时=%v", got)
	}
	for _, n := range []string{"pty_write", "pty_resize", "pty_close"} {
		if got := defaultToolTimeout(n); got != 30*time.Second {
			t.Errorf("%s 超时=%v", n, got)
		}
	}
}

// 输入内容绝不能进入日志摘要。
func TestSummarizeArgsDoesNotLeakPTYInput(t *testing.T) {
	got := summarizeArgs("pty_write", []byte(`{"session":"s","data":"cGFzc3dvcmQ="}`))
	if got != "pty_write" || strings.Contains(got, "cGFz") {
		t.Errorf("摘要泄漏了输入: %q", got)
	}
}
