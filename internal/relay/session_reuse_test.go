package relay

import (
	"net"
	"testing"
	"time"
)

// TestReuseSessionByClientIDAtomicity 验证 ReuseSessionByClientID 的原子性。
func TestReuseSessionByClientIDAtomicity(t *testing.T) {
	sm := NewSessionManager()
	share1 := &ClientConn{ID: "share1", Conn: &MockConn{}, ClientID: "cid-1"}
	session, _ := sm.CreateSession("CODE001", share1, 30*time.Minute, "cid-1", "127.0.0.1", 100, 1000)

	// 原子复用：应成功
	share2 := &ClientConn{ID: "share2", Conn: &MockConn{}, ClientID: "cid-1"}
	result, ok := sm.ReuseSessionByClientID("cid-1", share2)
	if !ok {
		t.Fatal("ReuseSessionByClientID 应成功")
	}
	if result.SessionID != session.ID {
		t.Error("复用的 session 应与原 session 一致")
	}
	if sm.FindPeer("share2") != nil {
		// 无 Help 时 peer 应为空；该断言同时触发 byConnID 查询。
		t.Error("无 Help 时新 Share 不应有 peer")
	}
	if result.OldShare != share1 {
		t.Error("Share 应已换绑到 share2")
	}
	if result.OldHelp != nil {
		t.Error("无 Help 时 OldHelp 应为空")
	}
	if sm.IsActiveDataSession(session.ID) {
		t.Error("复用后 dataPlaneReady 应为 false")
	}
}

// TestReuseSessionWithHelpDetachesOldHelp 验证复用时原子解绑旧 Help，要求其重新 Join。
func TestReuseSessionWithHelpDetachesOldHelp(t *testing.T) {
	sm := NewSessionManager()
	share1 := &ClientConn{ID: "share1", Conn: &MockConn{}, ClientID: "cid-1"}
	help := &ClientConn{ID: "help1", Conn: &MockConn{}}
	session, _ := sm.CreateSession("CODE002", share1, 30*time.Minute, "cid-1", "127.0.0.1", 100, 1000)

	// Join + Activate
	_, _ = sm.JoinSession("CODE002", help)
	sm.ActivateDataPlane(session.ID, share1.ID, help.ID)
	if !sm.IsActiveDataSession(session.ID) {
		t.Fatal("数据面应已激活")
	}

	// Share 重连复用
	share2 := &ClientConn{ID: "share2", Conn: &MockConn{}, ClientID: "cid-1"}
	result, ok := sm.ReuseSessionByClientID("cid-1", share2)
	if !ok {
		t.Fatal("ReuseSessionByClientID 应成功")
	}
	if result.OldHelp != help {
		t.Error("复用结果应返回待关闭的旧 Help")
	}
	if sm.IsActiveDataSession(session.ID) {
		t.Error("复用后数据面应失效")
	}
	if sm.FindPeer(share2.ID) != nil {
		t.Error("旧 Help 解绑后新 Share 不应有 peer")
	}
	if sm.FindPeer(help.ID) != nil {
		t.Error("旧 Help 的 byConnID 索引应被移除")
	}
	if sm.ActivateDataPlane(session.ID, share2.ID, help.ID) {
		t.Fatal("旧 Help 已解绑，不得重新激活")
	}
}

func TestReuseSessionClearsStaleEndpointIdentity(t *testing.T) {
	sm, session := newSessionWithAddrs(t, "203.0.113.10:5000", "198.51.100.20:6000")
	sm.UpdatePeerAddr("share-src", "203.0.113.10:41001", "192.168.1.5:41001", "symmetric")
	sm.UpdatePeerAddr("help-src", "198.51.100.20:41002", "192.168.1.6:41002", "cone")

	newShare := &ClientConn{ID: "share-new", ClientID: "cid-src", Conn: &addrConn{addr: "198.51.100.77:7000"}}
	if _, ok := sm.ReuseSessionByClientID("cid-src", newShare); !ok {
		t.Fatal("复用应成功")
	}
	if !sm.MarkShareReady(session.ID, newShare.ID) {
		t.Fatal("新 Share 发布失败")
	}
	newHelp := &ClientConn{ID: "help-new", Conn: &addrConn{addr: "198.51.100.88:8000"}}
	if _, err := sm.JoinSession("SRCCODE", newHelp); err != nil {
		t.Fatalf("新 Help 加入失败: %v", err)
	}
	if !sm.ActivateDataPlane(session.ID, newShare.ID, newHelp.ID) {
		t.Fatal("新端点应可激活数据面")
	}

	for _, stale := range []string{"203.0.113.10", "192.168.1.5", "198.51.100.20", "192.168.1.6"} {
		if sm.IsActiveDataSource(session.ID, net.ParseIP(stale)) {
			t.Fatalf("复用后旧端点 %s 仍被当作 UDP 来源接受", stale)
		}
	}
	for _, current := range []string{"198.51.100.77", "198.51.100.88"} {
		if !sm.IsActiveDataSource(session.ID, net.ParseIP(current)) {
			t.Fatalf("新端点 %s 应被接受", current)
		}
	}
}

// TestReuseSessionNotFoundReturnsFalse 验证不存在的 ClientID 返回 false。
func TestReuseSessionNotFoundReturnsFalse(t *testing.T) {
	sm := NewSessionManager()
	share := &ClientConn{ID: "share1", Conn: &MockConn{}, ClientID: "cid-999"}
	_, ok := sm.ReuseSessionByClientID("cid-999", share)
	if ok {
		t.Error("不存在的 ClientID 应返回 false")
	}
}

// TestReuseSessionExpiredReturnsFalse 验证过期会话不能复用。
func TestReuseSessionExpiredReturnsFalse(t *testing.T) {
	sm := NewSessionManager()
	share1 := &ClientConn{ID: "share1", Conn: &MockConn{}, ClientID: "cid-expired"}
	sm.CreateSession("CODE003", share1, 1*time.Millisecond, "cid-expired", "127.0.0.1", 100, 1000)
	time.Sleep(5 * time.Millisecond)

	share2 := &ClientConn{ID: "share2", Conn: &MockConn{}, ClientID: "cid-expired"}
	_, ok := sm.ReuseSessionByClientID("cid-expired", share2)
	if ok {
		t.Error("过期会话不应复用")
	}
}
