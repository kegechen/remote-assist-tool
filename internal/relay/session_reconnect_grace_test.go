package relay

import (
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// 使用中的会话（share、help 都在）过了 TTL 不清理，只删码映射（c70b7d0）。但此后任何一端
// 抖断一下，会话就不再「使用中」，下一轮清理直接删掉，重连只能拿到新码。
// reconnectGrace 让使用中会话的断开把有效期顺延到「断开时刻 + 宽限」，并恢复码映射。

// pairedExpiredSession 建一个已配对且已过原 TTL 的会话，并跑过一轮清理（码映射已被删）。
func pairedExpiredSession(t *testing.T, sm *SessionManager, code, clientID string) (*TunnelSession, *ClientConn, *ClientConn) {
	t.Helper()
	share := &ClientConn{ID: "share-" + code, Conn: &MockConn{}}
	help := &ClientConn{ID: "help-" + code, Conn: &MockConn{}}
	s, err := sm.CreateSession(code, share, 100*time.Millisecond, clientID, "10.8.0.1", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm.JoinSession(code, help); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if expired := sm.CleanupExpired(); len(expired) != 0 {
		t.Fatalf("使用中会话过期后不应被清理，实际清掉 %v", expired)
	}
	if _, err := sm.GetSessionByCode(code); err == nil {
		t.Fatal("使用中会话过期后码映射应被删除，防止新 help 加入")
	}
	return s, share, help
}

func TestShareBlipAfterTTLKeepsCode(t *testing.T) {
	sm := NewSessionManager()
	sm.reconnectGrace = time.Minute
	s, share, _ := pairedExpiredSession(t, sm, "GRACESHARE", "client-grace-share")

	sm.DisconnectClient(share.ID)
	if expired := sm.CleanupExpired(); len(expired) != 0 {
		t.Fatalf("宽限期内会话不应被清理，实际清掉 %v", expired)
	}

	share2 := &ClientConn{ID: "share-GRACESHARE-2", Conn: &MockConn{}}
	res, ok := sm.ReuseSessionByClientID("client-grace-share", share2)
	if !ok {
		t.Fatal("宽限期内 share 重连应复用原会话")
	}
	if res.Code != "GRACESHARE" || res.SessionID != s.ID {
		t.Fatalf("复用结果 code=%s session=%s，期望原码 GRACESHARE / %s", res.Code, res.SessionID, s.ID)
	}
	if !sm.MarkShareReady(s.ID, share2.ID) {
		t.Fatal("MarkShareReady 失败")
	}
	if _, err := sm.JoinSession("GRACESHARE", &ClientConn{ID: "help-GRACESHARE-2", Conn: &MockConn{}}); err != nil {
		t.Fatalf("宽限期内 help 应能用原码重新加入，err=%v", err)
	}
}

func TestHelpBlipAfterTTLKeepsCode(t *testing.T) {
	shrinkHelpDebounce(t, 10*time.Millisecond)
	sm := NewSessionManager()
	sm.reconnectGrace = time.Minute
	s, _, help := pairedExpiredSession(t, sm, "GRACEHELP", "client-grace-help")

	sm.DisconnectClient(help.ID)
	waitHelpCleared(t, sm, s)
	if expired := sm.CleanupExpired(); len(expired) != 0 {
		t.Fatalf("宽限期内会话不应被清理，实际清掉 %v", expired)
	}
	res, err := sm.JoinSession("GRACEHELP", &ClientConn{ID: "help-GRACEHELP-2", Conn: &MockConn{}})
	if err != nil {
		t.Fatalf("宽限期内 help 应能用原码重新加入，err=%v", err)
	}
	if res.SessionID != s.ID {
		t.Fatalf("加入了 %s，期望原会话 %s", res.SessionID, s.ID)
	}
}

// waitHelpCleared 轮询等去抖回调真正清空 Help 槽（回调在独立 goroutine 里跑）。
func waitHelpCleared(t *testing.T, sm *SessionManager, s *TunnelSession) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		sm.mu.Lock()
		cleared := s.Help == nil
		sm.mu.Unlock()
		if cleared {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("去抖回调 1s 内未清空 Help 槽")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 去抖本来就是给秒级抖动兜底的：过了原 TTL 的会话，help 在去抖窗口内用原码回来也必须成功，
// 不能等去抖结束、码映射恢复之后才行。
func TestHelpRejoinWithinDebounceAfterTTL(t *testing.T) {
	shrinkHelpDebounce(t, time.Minute)
	sm := NewSessionManager()
	sm.reconnectGrace = time.Minute
	s, _, help := pairedExpiredSession(t, sm, "GRACEDEB", "client-grace-deb")

	sm.DisconnectClient(help.ID)
	res, err := sm.JoinSession("GRACEDEB", &ClientConn{ID: "help-GRACEDEB-2", Conn: &MockConn{}})
	if err != nil {
		t.Fatalf("去抖窗口内 help 应能用原码回来，err=%v", err)
	}
	if res.SessionID != s.ID {
		t.Fatalf("加入了 %s，期望原会话 %s", res.SessionID, s.ID)
	}
}

// 宽限从断开时刻起算：TTL 之内发生的使用中断开，同样保证码在断开后还有完整的一个宽限期。
func TestInUseDisconnectWithinTTLExtendsFromDisconnect(t *testing.T) {
	sm := NewSessionManager()
	sm.reconnectGrace = time.Hour
	share := &ClientConn{ID: "share-inttl", Conn: &MockConn{}}
	s, err := sm.CreateSession("INTTL", share, time.Minute, "client-inttl", "10.8.0.4", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm.JoinSession("INTTL", &ClientConn{ID: "help-inttl", Conn: &MockConn{}}); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	sm.DisconnectClient(share.ID)
	sm.mu.Lock()
	expires := s.ExpiresAt
	sm.mu.Unlock()
	if expires.Before(before.Add(time.Hour)) {
		t.Fatalf("ExpiresAt=%v，期望不早于断开时刻+宽限 %v", expires, before.Add(time.Hour))
	}
}

func TestReconnectGraceElapsesThenCleanup(t *testing.T) {
	sm := NewSessionManager()
	sm.reconnectGrace = 20 * time.Millisecond
	s, share, _ := pairedExpiredSession(t, sm, "GRACEEND", "client-grace-end")

	sm.DisconnectClient(share.ID)
	time.Sleep(40 * time.Millisecond)
	if expired := sm.CleanupExpired(); len(expired) != 1 || expired[0] != s.ID {
		t.Fatalf("宽限期满应清理会话，实际 %v", expired)
	}
	if _, ok := sm.ReuseSessionByClientID("client-grace-end", &ClientConn{ID: "share-GRACEEND-2", Conn: &MockConn{}}); ok {
		t.Fatal("宽限期满后不应再复用旧会话")
	}
}

// 从没配对过的会话不享受宽限：share 断开后按原 TTL 过期清理。
func TestUnpairedShareDisconnectGetsNoGrace(t *testing.T) {
	sm := NewSessionManager()
	sm.reconnectGrace = time.Minute
	share := &ClientConn{ID: "share-unpaired", Conn: &MockConn{}}
	s, err := sm.CreateSession("UNPAIRED", share, 20*time.Millisecond, "client-unpaired", "10.8.0.2", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	sm.DisconnectClient(share.ID)
	time.Sleep(40 * time.Millisecond)
	if expired := sm.CleanupExpired(); len(expired) != 1 || expired[0] != s.ID {
		t.Fatalf("未配对会话应按原 TTL 清理，实际 %v", expired)
	}
}

// no-auth 下码恒为 NoAuthCode：宽限恢复码映射时不得抢走别的在线会话的条目。
func TestReconnectGraceDoesNotStealSharedCode(t *testing.T) {
	sm := NewSessionManager()
	sm.reconnectGrace = time.Minute
	const code = proto.NoAuthCode
	_, shareA, _ := pairedExpiredSession(t, sm, code, "client-noauth-a")

	b, err := sm.CreateSession(code, &ClientConn{ID: "share-noauth-b", Conn: &MockConn{}}, time.Minute, "client-noauth-b", "10.8.0.3", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	sm.DisconnectClient(shareA.ID)

	got, err := sm.GetSessionByCode(code)
	if err != nil || got.ID != b.ID {
		t.Fatalf("byCode 应仍指向 B(%s)，实际 got=%v err=%v", b.ID, got, err)
	}
}

func TestNewServerReconnectGraceEqualsCodeTTL(t *testing.T) {
	srv, err := NewServer(&Config{CodeTTL: 7 * time.Minute, CodeLength: 10})
	if err != nil {
		t.Fatal(err)
	}
	if srv.sessions.reconnectGrace != 7*time.Minute {
		t.Fatalf("reconnectGrace=%v，期望等于 CodeTTL 7m", srv.sessions.reconnectGrace)
	}
}
