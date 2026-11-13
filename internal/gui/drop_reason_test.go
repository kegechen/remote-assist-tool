package gui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClassifyDropError(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"peer_gone: 被协助端已断开连接，请重新 connect", dropPeerGone},
		{"peer_gone: 被协助端连接已更新（重启或热升级），请重新 connect", dropPeerRestarted},
		{"tunnel_lost: 隧道已断开（EOF），请重新 connect", dropTunnelLost},
		// 会话拆除后的 not_connected 携带 last_drop，应还原出真实原因
		{"not_connected: call 'connect' with the assist code first (last_drop: peer_gone: 被协助端已断开连接，请重新 connect)", dropPeerGone},
		{"not_connected: call 'connect' with the assist code first (last_drop: tunnel_lost: 隧道已断开（EOF），请重新 connect)", dropTunnelLost},
		{"not_connected: call 'connect' with the assist code first (last_drop: peer_gone: 被协助端连接已更新（重启或热升级），请重新 connect)", dropPeerRestarted},
		{"not_connected: call 'connect' with the assist code first", dropGeneric},
	}
	for _, c := range cases {
		if got := classifyDropError(errors.New(c.msg)).Code; got != c.want {
			t.Errorf("%q -> %s，期望 %s", c.msg, got, c.want)
		}
	}
	if classifyDropError(nil).Code != dropGeneric {
		t.Error("nil 应为通用原因")
	}
}

func TestClassifyReconnectError(t *testing.T) {
	cases := []struct {
		msg  string
		want string
		ok   bool
	}{
		{"dial tcp 127.0.0.1:1: connectex: No connection could be made because the target machine actively refused it", reasonRefused, true},
		{"dial tcp: lookup x: no such host", reasonUnreach, true},
		{"context deadline exceeded", reasonTimeout, true},
		{"join failed: invalid code", reasonBadCode, true},
		{"something weird", "", false},
	}
	for _, c := range cases {
		info, ok := classifyReconnectError(errors.New(c.msg))
		if ok != c.ok || (ok && info.Code != c.want) {
			t.Errorf("%q -> (%s,%v)，期望 (%s,%v)", c.msg, info.Code, ok, c.want, c.ok)
		}
	}
}

// 每个原因码都必须有非空文案与建议动作，且文案互不相同（用户能区分）。
func TestDropByCodeTexts(t *testing.T) {
	seen := map[string]string{}
	for _, code := range []string{dropPeerGone, dropPeerRestarted, dropTunnelLost, dropLocalExit, dropGeneric,
		reasonUnreach, reasonRefused, reasonTimeout, reasonBadCode, reasonRejected, reasonPeerLegacy} {
		i := dropByCode(code)
		if i.Code != code || i.Text == "" || i.Hint == "" {
			t.Errorf("%s 文案不全: %+v", code, i)
		}
		if prev, dup := seen[i.Text]; dup {
			t.Errorf("%s 与 %s 文案重复: %s", code, prev, i.Text)
		}
		seen[i.Text] = code
	}
	if !strings.Contains(dropByCode(dropPeerGone).Hint, "确认对方程序仍在运行") {
		t.Error("对端断开的建议动作应提示确认对方程序仍在运行")
	}
}

// 事件必须是单个 data 行的 JSON，且经 normalizeSSEEvent 后保持不变。
func TestSSEEventWithInfo(t *testing.T) {
	msg := normalizeSSEEvent(sseEventWithInfo("reconnect-fail", dropByCode(dropTunnelLost), map[string]any{"retry": 2, "max": 6}))
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "event: reconnect-fail" || !strings.HasPrefix(lines[1], "data: ") {
		t.Fatalf("事件格式不对: %q", msg)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &m); err != nil {
		t.Fatalf("data 不是 JSON: %v", err)
	}
	if m["reason"] != dropTunnelLost || m["retry"] != float64(2) || m["text"] == "" {
		t.Errorf("字段不对: %v", m)
	}
}

// peerWatch 要记住连续失败中最具体的原因，不被后到的通用 not_connected 覆盖。
func TestPeerWatchKeepsSpecificReason(t *testing.T) {
	w := newPeerWatch(3)
	w.observe(errors.New("peer_gone: 被协助端已断开连接，请重新 connect"))
	w.observe(errors.New("not_connected: call 'connect' with the assist code first"))
	if !w.observe(errors.New("not_connected: call 'connect' with the assist code first")) {
		t.Fatal("第 3 次应判定已断")
	}
	if w.reason.Code != dropPeerGone {
		t.Errorf("reason=%s，期望 peer_gone", w.reason.Code)
	}
	w.observe(nil)
	if w.reason.Code != "" || w.count != 0 {
		t.Error("成功一次应清零")
	}
}

// guardLoop 取原因的约定：被 watchPeer 标注的 client 用标注原因，其余（子进程自退）为 local_exit。
func TestSetDropReasonPairing(t *testing.T) {
	s := &Server{}
	c := &MCPClient{}
	s.setDropReason(c, dropByCode(dropPeerRestarted))
	if s.dropClient != c || s.dropWhy.Code != dropPeerRestarted {
		t.Fatal("原因未登记")
	}
	s.noteReconnectFailure(errors.New("dial tcp: connection refused"))
	info, ok := s.takeReconnectFailure()
	if !ok || info.Code != reasonRefused {
		t.Fatalf("重连失败原因 %+v %v", info, ok)
	}
	if _, ok := s.takeReconnectFailure(); ok {
		t.Fatal("取走后应清空")
	}
}
