package relay

import (
	"testing"
	"time"
)

// newPairedSession 建立一个 Share+Help 都在线、数据面已激活的会话，返回 sm 与 session。
func newPairedSession(t *testing.T) (*SessionManager, *TunnelSession, *ClientConn, *ClientConn) {
	t.Helper()
	sm := NewSessionManager()
	share := &ClientConn{ID: "share1", Conn: &MockConn{}}
	help := &ClientConn{ID: "help1", Conn: &MockConn{}}
	session, _ := sm.CreateSession("DPCODE", share, 30*time.Minute, "cid-1", "127.0.0.1", 100, 1000)
	if _, err := sm.JoinSession("DPCODE", help); err != nil {
		t.Fatalf("JoinSession: %v", err)
	}
	if !sm.ActivateDataPlane(session.ID, share.ID, help.ID) {
		t.Fatal("ActivateDataPlane 应成功")
	}
	if !sm.IsActiveDataSession(session.ID) {
		t.Fatal("激活后应为 active data session")
	}
	return sm, session, share, help
}

// TestJoinLeavesDataPlaneInactiveUntilActivate Join 后未激活前数据面不就绪。
func TestJoinLeavesDataPlaneInactiveUntilActivate(t *testing.T) {
	sm := NewSessionManager()
	share := &ClientConn{ID: "share1", Conn: &MockConn{}}
	help := &ClientConn{ID: "help1", Conn: &MockConn{}}
	session, _ := sm.CreateSession("DPCODE2", share, 30*time.Minute, "", "127.0.0.1", 100, 1000)
	if _, err := sm.JoinSession("DPCODE2", help); err != nil {
		t.Fatalf("JoinSession: %v", err)
	}
	if sm.IsActiveDataSession(session.ID) {
		t.Fatal("Join 后未 ActivateDataPlane，不应 active")
	}
	if !sm.ActivateDataPlane(session.ID, share.ID, help.ID) {
		t.Fatal("ActivateDataPlane 应成功")
	}
	if !sm.IsActiveDataSession(session.ID) {
		t.Fatal("激活后应 active")
	}
}

func TestJoinWaitsUntilShareRegistrationPublished(t *testing.T) {
	sm := NewSessionManager()
	share := &ClientConn{ID: "share-pending", Conn: &MockConn{}}
	session, err := sm.createPendingSession("PENDING", share, time.Minute, "", "127.0.0.1", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm.JoinSession("PENDING", &ClientConn{ID: "help-too-early", Conn: &MockConn{}}); err != ErrSessionNotFound {
		t.Fatalf("注册响应发布前 Join 错误=%v，期望 ErrSessionNotFound", err)
	}
	if !sm.MarkShareReady(session.ID, share.ID) {
		t.Fatal("发布 Share 失败")
	}
	if _, err := sm.JoinSession("PENDING", &ClientConn{ID: "help-ready", Conn: &MockConn{}}); err != nil {
		t.Fatalf("发布后 Join 应成功: %v", err)
	}
}

// TestHelpDisconnectResetsDataPlane Help 断连立即置数据面不就绪并要求失效 relay。
func TestHelpDisconnectResetsDataPlane(t *testing.T) {
	sm, session, _, help := newPairedSession(t)

	res := sm.DisconnectClient(help.ID)
	if res == nil || !res.ResetDataPlane || res.SessionID != session.ID {
		t.Fatalf("Help 断连应返回 ResetDataPlane=true 且 SessionID=%s，得到 %+v", session.ID, res)
	}
	if sm.IsActiveDataSession(session.ID) {
		t.Fatal("Help 断连后数据面必须立即不就绪")
	}
	if _, err := sm.JoinSession("DPCODE", &ClientConn{ID: "help-rejoined", Conn: &MockConn{}}); err != nil {
		t.Fatalf("Help 断开不得撤销 Share 发布状态，重新 Join 应成功: %v", err)
	}
}

// TestShareDisconnectResetsDataPlane Share 断连立即置数据面不就绪并要求失效 relay。
func TestShareDisconnectResetsDataPlane(t *testing.T) {
	sm, session, share, _ := newPairedSession(t)

	res := sm.DisconnectClient(share.ID)
	if res == nil || !res.ResetDataPlane || !res.WasShare || res.SessionID != session.ID {
		t.Fatalf("Share 断连应返回 ResetDataPlane=true WasShare=true，得到 %+v", res)
	}
	if sm.IsActiveDataSession(session.ID) {
		t.Fatal("Share 断连后数据面必须立即不就绪")
	}
}

// TestReuseSessionResetsDataPlane Share 复用换绑后数据面不就绪，需 Help 重新 Join。
func TestReuseSessionResetsDataPlane(t *testing.T) {
	sm, session, _, help := newPairedSession(t)

	newShare := &ClientConn{ID: "share2", Conn: &MockConn{}}
	if _, ok := sm.ReuseSessionByClientID("cid-1", newShare); !ok {
		t.Fatal("ReuseSessionByClientID 应成功")
	}
	if sm.IsActiveDataSession(session.ID) {
		t.Fatal("Share 复用后数据面必须不就绪，直到重新激活")
	}
	// 用旧 Share ID 激活应失败（配对已换代）
	if sm.ActivateDataPlane(session.ID, "share1", help.ID) {
		t.Fatal("用旧 Share ID 激活应失败")
	}
	// 旧 Help 已解绑，即使用新 Share ID 也不能直接激活。
	if sm.ActivateDataPlane(session.ID, newShare.ID, help.ID) {
		t.Fatal("旧 Help 未重新 Join 前不得激活")
	}

	if !sm.MarkShareReady(session.ID, newShare.ID) {
		t.Fatal("新 Share 发布失败")
	}
	newHelp := &ClientConn{ID: "help2", Conn: &MockConn{}}
	if _, err := sm.JoinSession("DPCODE", newHelp); err != nil {
		t.Fatalf("新 Help 重新 Join: %v", err)
	}
	if !sm.ActivateDataPlane(session.ID, newShare.ID, newHelp.ID) {
		t.Fatal("新 Share 与重新 Join 的 Help 应可激活")
	}
}

// TestActivateDataPlaneRejectsPendingDisconnect pending 断连窗口内不得激活。
func TestActivateDataPlaneRejectsPendingDisconnect(t *testing.T) {
	sm, session, share, help := newPairedSession(t)

	// Help 断连进入 pending（去抖计时器 5s 内 pendingHelpID != ""）
	sm.DisconnectClient(help.ID)
	if sm.ActivateDataPlane(session.ID, share.ID, help.ID) {
		t.Fatal("pending 断连窗口内不得激活数据面")
	}
}

// TestActivateDataPlaneRejectsUnknownSession 未知会话激活失败。
func TestActivateDataPlaneRejectsUnknownSession(t *testing.T) {
	sm := NewSessionManager()
	if sm.ActivateDataPlane("ses_nonexistent", "s", "h") {
		t.Fatal("未知会话不应激活成功")
	}
	if sm.IsActiveDataSession("ses_nonexistent") {
		t.Fatal("未知会话不应 active")
	}
}
