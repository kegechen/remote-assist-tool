package gui

import (
	"strings"
	"testing"
)

// 回归：SSE 事件必须带 data 行，否则浏览器按规范不派发（连接状态事件曾因此从未到达页面）。
func TestNormalizeSSEEventAddsData(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"event: connected\n", "event: connected\ndata: \n"},
		{"event: lost\n", "event: lost\ndata: \n"},
		{"event: reconnect-fail\nretry 1/3\n", "event: reconnect-fail\ndata: retry 1/3\n"},
		{"event: upgrade-progress\ndata: 50%\n", "event: upgrade-progress\ndata: 50%\n"},
		{"event: connect-fallback\ndata: 1\n", "event: connect-fallback\ndata: 1\n"},
	}
	for _, c := range cases {
		got := normalizeSSEEvent(c.in)
		if got != c.want {
			t.Errorf("normalizeSSEEvent(%q)=%q 期望 %q", c.in, got, c.want)
		}
		if !strings.Contains(got, "data:") {
			t.Errorf("事件缺少 data 行，浏览器不会派发: %q", got)
		}
	}
}
