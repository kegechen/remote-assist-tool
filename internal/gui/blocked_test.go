package gui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/gui/assets"
	"github.com/remote-assist/tool/internal/proto"
)

// 自动重连时被 relay 封禁：第一次失败就停，不再退避重试、不广播 reconnect-fail，返回封禁
// 原因交给 lost 展示。普通失败照旧重试满次数。
func TestAutoReconnectStopsWhenBlocked(t *testing.T) {
	orig := reconnectBackoff
	reconnectBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { reconnectBackoff = orig })
	ch := make(chan string, 16)
	s := &Server{sseSubs: map[chan string]struct{}{ch: {}}}

	tries := 0
	outcome, why := s.autoReconnect(dropByCode(dropTunnelLost), func() reconnectOutcome {
		tries++
		s.noteReconnectFailure(fmt.Errorf("relay connect failed: failed to connect: %w", proto.ErrBlocked))
		return reconnectFailed
	})
	if tries != 1 || outcome != reconnectFailed || why.Code != reasonBlocked {
		t.Fatalf("被封后应立即停止：tries=%d outcome=%v why=%+v", tries, outcome, why)
	}
	if len(ch) != 0 {
		t.Fatalf("被封时不应广播重连失败：%q", <-ch)
	}

	tries = 0
	outcome, why = s.autoReconnect(dropByCode(dropTunnelLost), func() reconnectOutcome {
		tries++
		s.noteReconnectFailure(errors.New("dial tcp: connection refused"))
		return reconnectFailed
	})
	if tries != len(reconnectBackoff) || outcome != reconnectFailed || why.Code != reasonRefused || len(ch) != len(reconnectBackoff) {
		t.Fatalf("普通失败应重试满次数并逐次广播：tries=%d why=%+v broadcasts=%d", tries, why, len(ch))
	}
}

// 前端：封禁时只说那一句话。lost 不附「请处理后重新点击连接」，首次连接失败不把带
// access_denied 前缀的原始错误打进终端。
func TestBlockedNoticeWiring(t *testing.T) {
	html := strings.ReplaceAll(string(assets.IndexHTML), "\r\n", "\n")
	i := strings.Index(html, "es.addEventListener('lost',")
	if i < 0 {
		t.Fatal("找不到 lost 事件处理")
	}
	seg := html[i:]
	seg = seg[:strings.Index(seg, "});")]
	b, g := strings.Index(seg, "d.reason==='blocked'"), strings.Index(seg, "请处理后重新点击连接")
	if b < 0 || g < 0 || b > g {
		t.Error("lost 事件必须先截住 blocked，再走附带「请处理后重新点击连接」的通用文案")
	}
	if !strings.Contains(html, "if(r.error&&r.code!=='blocked')termLog(") {
		t.Error("连接失败时被封禁的原始错误不应打进终端")
	}
}
