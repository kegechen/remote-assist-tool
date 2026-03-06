package relay

import (
	"testing"
	"time"
)

// MockConn is a mock implementation of Conn interface
type MockConn struct {
	closed bool
}

func (m *MockConn) Read(p []byte) (int, error) {
	return 0, nil
}

func (m *MockConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (m *MockConn) Close() error {
	m.closed = true
	return nil
}

func (m *MockConn) RemoteAddr() string {
	return "127.0.0.1:0"
}

func TestSessionCreate(t *testing.T) {
	sm := NewSessionManager()

	share := &ClientConn{
		ID:   "test-share",
		Type: "share",
		Conn: &MockConn{},
	}

	session := sm.CreateSession("TESTCODE", share, 30*time.Minute, "")

	if session.ID == "" {
		t.Error("Session ID should not be empty")
	}
	if session.Code != "TESTCODE" {
		t.Error("Session code mismatch")
	}
	if session.Share != share {
		t.Error("Share client not set")
	}
}

func TestSessionGetByCode(t *testing.T) {
	sm := NewSessionManager()

	share := &ClientConn{ID: "test", Conn: &MockConn{}}
	session := sm.CreateSession("TESTCODE123", share, 30*time.Minute, "")

	found, err := sm.GetSessionByCode("TESTCODE123")
	if err != nil {
		t.Fatalf("GetSessionByCode failed: %v", err)
	}
	if found.ID != session.ID {
		t.Error("Session not found")
	}
}

func TestSessionJoin(t *testing.T) {
	sm := NewSessionManager()

	share := &ClientConn{ID: "share1", Conn: &MockConn{}}
	help := &ClientConn{ID: "help1", Conn: &MockConn{}}

	sm.CreateSession("TESTJOIN", share, 30*time.Minute, "")

	session, err := sm.JoinSession("TESTJOIN", help)
	if err != nil {
		t.Fatalf("JoinSession failed: %v", err)
	}
	if session.Help != help {
		t.Error("Help client not joined")
	}
}

func TestSessionJoinInvalidCode(t *testing.T) {
	sm := NewSessionManager()

	help := &ClientConn{ID: "help1", Conn: &MockConn{}}
	_, err := sm.JoinSession("INVALID", help)
	if err == nil {
		t.Error("Join with invalid code should fail")
	}
}

func TestSessionDoubleJoin(t *testing.T) {
	sm := NewSessionManager()

	share := &ClientConn{ID: "share1", Conn: &MockConn{}}
	help1 := &ClientConn{ID: "help1", Conn: &MockConn{}}
	help2 := &ClientConn{ID: "help2", Conn: &MockConn{}}

	sm.CreateSession("TESTDOUBLE", share, 30*time.Minute, "")
	_, _ = sm.JoinSession("TESTDOUBLE", help1)
	_, err := sm.JoinSession("TESTDOUBLE", help2)
	if err == nil {
		t.Error("Second join should fail")
	}
	if err != ErrSessionHasHelper {
		t.Errorf("Expected ErrSessionHasHelper, got %v", err)
	}
}

func TestSessionClose(t *testing.T) {
	sm := NewSessionManager()

	share := &ClientConn{ID: "share1", Conn: &MockConn{}}
	session := sm.CreateSession("TESTCLOSE", share, 30*time.Minute, "")

	if sm.GetActiveSessions() != 1 {
		t.Error("Session count should be 1")
	}

	sm.CloseSession(session.ID)

	if sm.GetActiveSessions() != 0 {
		t.Error("Session count should be 0 after close")
	}
}

func TestSessionCleanupExpired(t *testing.T) {
	sm := NewSessionManager()

	// 创建一个即将过期的会话
	share := &ClientConn{ID: "share1", Conn: &MockConn{}}
	sm.CreateSession("EXPIRE1", share, 1*time.Millisecond, "")

	// 创建一个长期会话
	share2 := &ClientConn{ID: "share2", Conn: &MockConn{}}
	sm.CreateSession("LONG1", share2, 1*time.Hour, "")

	if sm.GetActiveSessions() != 2 {
		t.Error("Should have 2 sessions")
	}

	// 等待过期
	time.Sleep(50 * time.Millisecond)

	expired := sm.CleanupExpired()
	if len(expired) == 0 {
		t.Error("Should have expired sessions")
	}
}

func TestSessionGetByExpiredCode(t *testing.T) {
	sm := NewSessionManager()

	share := &ClientConn{ID: "share1", Conn: &MockConn{}}
	sm.CreateSession("EXPIRETEST", share, 1*time.Millisecond, "")

	time.Sleep(50 * time.Millisecond)

	_, err := sm.GetSessionByCode("EXPIRETEST")
	if err == nil {
		t.Error("GetSessionByCode on expired should fail")
	}
}
