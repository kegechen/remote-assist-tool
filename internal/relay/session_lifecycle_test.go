package relay

import (
	"testing"
	"time"
)

func TestSessionQuotaReleasedAfterShareDisconnectAndExpiry(t *testing.T) {
	sm := NewSessionManager()
	share := &ClientConn{ID: "share-expired", Conn: &MockConn{}}
	session, err := sm.CreateSession("EXPIRED", share, time.Millisecond, "", "10.0.0.1", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	sm.DisconnectClient(share.ID)
	time.Sleep(5 * time.Millisecond)
	if expired := sm.CleanupExpired(); len(expired) != 1 || expired[0] != session.ID {
		t.Fatalf("过期清理结果=%v，期望 [%s]", expired, session.ID)
	}
	if got := sm.sessionCountPerIP["10.0.0.1"]; got != 0 {
		t.Fatalf("过期清理后 per-IP 计数=%d，期望 0", got)
	}
	if _, err := sm.CreateSession("REUSED", &ClientConn{ID: "share-new", Conn: &MockConn{}}, time.Minute, "", "10.0.0.1", 1, 10); err != nil {
		t.Fatalf("释放配额后应允许创建: %v", err)
	}
}

func TestCloseSessionWithoutShareReleasesQuotaOnce(t *testing.T) {
	sm := NewSessionManager()
	share := &ClientConn{ID: "share-close", Conn: &MockConn{}}
	session, err := sm.CreateSession("CLOSE", share, time.Minute, "", "10.0.0.2", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	sm.DisconnectClient(share.ID)
	sm.CloseSession(session.ID)
	sm.CloseSession(session.ID)
	if got := sm.sessionCountPerIP["10.0.0.2"]; got != 0 {
		t.Fatalf("重复关闭后 per-IP 计数=%d，期望 0", got)
	}
}

func TestCrossIPReuseKeepsOriginalQuotaOwner(t *testing.T) {
	sm := NewSessionManager()
	original, err := sm.CreateSession("ORIGINAL", &ClientConn{ID: "share-old", Conn: &MockConn{}}, time.Minute, "cid", "10.0.0.3", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	other, err := sm.CreateSession("OTHER", &ClientConn{ID: "share-other", Conn: &MockConn{}}, time.Minute, "", "10.0.0.4", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	newShare := &ClientConn{ID: "share-new-ip", Conn: &remoteMockConn{MockConn: MockConn{}, addr: "10.0.0.4:9000"}}
	if _, ok := sm.ReuseSessionByClientID("cid", newShare); !ok {
		t.Fatal("跨 IP 复用应成功")
	}
	sm.CloseSession(original.ID)
	if got := sm.sessionCountPerIP["10.0.0.3"]; got != 0 {
		t.Fatalf("原计费 IP 计数=%d，期望 0", got)
	}
	if got := sm.sessionCountPerIP["10.0.0.4"]; got != 1 {
		t.Fatalf("新连接 IP 上其它会话计数被误减，实际=%d", got)
	}
	sm.CloseSession(other.ID)
}

func TestByConnIDLifecycle(t *testing.T) {
	sm := NewSessionManager()
	share1 := &ClientConn{ID: "share-1", Conn: &MockConn{}}
	session, err := sm.CreateSession("INDEX", share1, time.Minute, "cid-index", "10.0.0.5", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if sm.byConnID[share1.ID] != session {
		t.Fatal("CreateSession 未建立 Share 索引")
	}
	help1 := &ClientConn{ID: "help-1", Conn: &MockConn{}}
	if _, err := sm.JoinSession("INDEX", help1); err != nil {
		t.Fatal(err)
	}
	if sm.byConnID[help1.ID] != session {
		t.Fatal("JoinSession 未建立 Help 索引")
	}
	sm.RollbackJoin(session.ID, help1.ID)
	if sm.byConnID[help1.ID] != nil {
		t.Fatal("RollbackJoin 未移除 Help 索引")
	}
	help2 := &ClientConn{ID: "help-2", Conn: &MockConn{}}
	if _, err := sm.JoinSession("INDEX", help2); err != nil {
		t.Fatal(err)
	}
	share2 := &ClientConn{ID: "share-2", Conn: &MockConn{}}
	if _, ok := sm.ReuseSessionByClientID("cid-index", share2); !ok {
		t.Fatal("ReuseSessionByClientID 应成功")
	}
	if sm.byConnID[share1.ID] != nil || sm.byConnID[help2.ID] != nil || sm.byConnID[share2.ID] != session {
		t.Fatal("复用后连接索引不一致")
	}
	sm.DisconnectClient(share2.ID)
	if sm.byConnID[share2.ID] != nil {
		t.Fatal("DisconnectClient 未移除 Share 索引")
	}
	sm.CloseSession(session.ID)
	if len(sm.byConnID) != 0 {
		t.Fatalf("删除会话后仍有连接索引: %v", sm.byConnID)
	}
}

type remoteMockConn struct {
	MockConn
	addr string
}

func (c *remoteMockConn) RemoteAddr() string { return c.addr }
