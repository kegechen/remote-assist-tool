package gui

import (
	"errors"
	"testing"
)

// 被控端被杀/网络断开后，MCP 子进程不会退出，只会把会话拆掉并对后续调用返回
// not_connected / peer_gone / tunnel_lost。后端据此判定对端已断，主动结束子进程，
// 让 guardLoop 走既有的 reconnecting → 重连 → lost 流程。
func TestIsPeerDeadError(t *testing.T) {
	dead := []string{
		"not_connected: call 'connect' with the assist code first",
		"peer_gone: 被协助端已断开连接，请重新 connect",
		"peer_gone: 被协助端连接已更新（重启或热升级），请重新 connect",
		"tunnel_lost: 隧道已断开（EOF），请重新 connect",
	}
	for _, m := range dead {
		if !isPeerDeadError(errors.New(m)) {
			t.Errorf("应判定为对端已断: %q", m)
		}
	}
	alive := []string{
		"file_not_found: no such file",
		"permission_denied: access is denied",
		"deadline_exceeded: context deadline exceeded",
		"exec_denied: command not allowed",
		"desktop_disabled: share 端未启用远程桌面",
		"",
	}
	for _, m := range alive {
		if isPeerDeadError(errors.New(m)) {
			t.Errorf("不应判定为对端已断（普通工具错误）: %q", m)
		}
	}
	if isPeerDeadError(nil) {
		t.Error("nil 不是断线")
	}
}

// 连续失败才判定，避免单次抖动误杀连接；成功一次即清零。
func TestPeerWatchThreshold(t *testing.T) {
	w := newPeerWatch(3)
	dead := errors.New("not_connected: x")
	if w.observe(dead) || w.observe(dead) {
		t.Fatal("未达阈值不应触发")
	}
	if w.observe(nil) {
		t.Fatal("成功不应触发")
	}
	if w.observe(dead) || w.observe(dead) {
		t.Fatal("成功后应清零重新计数")
	}
	if !w.observe(dead) {
		t.Fatal("连续 3 次应触发")
	}
	if w.observe(errors.New("file_not_found: x")) {
		t.Fatal("普通错误不计入，也不触发")
	}
}
