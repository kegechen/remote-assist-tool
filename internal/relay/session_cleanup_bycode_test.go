package relay

import (
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// 回归：no-auth 模式下所有会话共用同一个 code（NoAuthCode），byCode 里后注册的会顶掉
// 先注册的。此时若先注册的那个会话过期，CleanupExpired 无条件 delete(byCode, code)
// 抹掉的其实是指向后者的条目——后者在线、未过期，却从此 ErrCodeInvalid，且没有任何
// 日志能指向真正的原因。
//
// deleteSessionLocked 早就做了指针复核（commit c60d28a），CleanupExpired 是漏网的那条
// 路径。这里断言 B 在 A 过期清理后仍能按 code 查到。
func TestCleanupExpiredKeepsByCodeEntryOwnedByNewerSession(t *testing.T) {
	sm := NewSessionManager()
	const code = proto.NoAuthCode

	a, err := sm.CreateSession(code, &ClientConn{ID: "share-a", Conn: &MockConn{}}, time.Millisecond, "", "10.9.0.1", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	// B 用同一个 code 注册，byCode[code] 现在指向 B
	b, err := sm.CreateSession(code, &ClientConn{ID: "share-b", Conn: &MockConn{}}, time.Minute, "", "10.9.0.2", 10, 10)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)
	expired := sm.CleanupExpired()
	if len(expired) != 1 || expired[0] != a.ID {
		t.Fatalf("过期清理结果=%v，期望只清掉 A(%s)", expired, a.ID)
	}

	got, err := sm.GetSessionByCode(code)
	if err != nil {
		t.Fatalf("A 过期后 B 应仍可按 code 查到，实际 err=%v", err)
	}
	if got.ID != b.ID {
		t.Fatalf("byCode 指向 %s，期望 B(%s)", got.ID, b.ID)
	}
}

// 反向断言：code 各不相同（有鉴权的正常路径）时，过期会话自己的 byCode 条目必须被删除，
// 否则过期码还能继续拉起新的 help。指针复核不能把这条也一并放过。
func TestCleanupExpiredRemovesOwnByCodeEntry(t *testing.T) {
	sm := NewSessionManager()
	s, err := sm.CreateSession("OWNCODE", &ClientConn{ID: "share-own", Conn: &MockConn{}}, time.Millisecond, "", "10.9.0.3", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if expired := sm.CleanupExpired(); len(expired) != 1 || expired[0] != s.ID {
		t.Fatalf("过期清理结果=%v，期望 [%s]", expired, s.ID)
	}
	if _, err := sm.GetSessionByCode("OWNCODE"); err == nil {
		t.Fatal("过期会话的 code 仍可查到，过期码应立即失效")
	}
}
