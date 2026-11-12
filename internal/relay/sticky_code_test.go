package relay

import (
	"fmt"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// 协助码在 share 进程存活期间固定：relay 层用例。
// 驱动方式与 server_reuse_protocol_test 一致：直接调 handleRegister/handleJoin，用内存连接取响应。

func newStickyServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.CodeLength == 0 {
		cfg.CodeLength = 12
	}
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = time.Minute
	}
	srv, err := NewServer(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// stickyRegister 模拟一次 share 注册，返回连接与解析后的响应。
func stickyRegister(t *testing.T, srv *Server, id, clientID, requestCode string, sticky bool) (*ClientConn, *proto.RegisterResponse) {
	t.Helper()
	conn := &reuseProtocolConn{remoteAddr: "10.1.0.1:1001"}
	c := &ClientConn{ID: id, ClientID: clientID, Conn: conn, requestCode: requestCode, supportsStickyCode: sticky}
	if srv.handleRegister(c) {
		t.Fatalf("注册 %s 被要求关闭连接", id)
	}
	msgs := conn.messages(t)
	if len(msgs) == 0 || msgs[0].Type != proto.MsgRegisterResponse {
		t.Fatalf("注册响应缺失: %v", msgs)
	}
	var resp proto.RegisterResponse
	if err := proto.DecodePayload(&msgs[0], &resp); err != nil {
		t.Fatal(err)
	}
	return c, &resp
}

func stickyJoin(srv *Server, id, code string) (*ClientConn, bool) {
	c := &ClientConn{ID: id, Conn: &reuseProtocolConn{remoteAddr: "10.9.0.1:2001"}}
	_, err := srv.sessions.JoinSession(code, c)
	return c, err == nil
}

// 控制端断开重连 5 次，码始终可用且不变。
func TestStickyHelpReconnectFiveTimesKeepsCode(t *testing.T) {
	old := helpDisconnectDebounce
	helpDisconnectDebounce = 10 * time.Millisecond
	defer func() { helpDisconnectDebounce = old }()
	srv := newStickyServer(t, Config{})
	_, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	for i := 0; i < 5; i++ {
		h, ok := stickyJoin(srv, fmt.Sprintf("h%d", i), resp.Code)
		if !ok {
			t.Fatalf("第 %d 次加入失败", i)
		}
		srv.sessions.DisconnectClient(h.ID)
		time.Sleep(40 * time.Millisecond) // 等去抖清槽
	}
	if _, ok := stickyJoin(srv, "h-last", resp.Code); !ok {
		t.Fatal("5 次断开重连后码应仍可加入")
	}
}

// 超过 TTL，share 在线时码仍可 join，且清理不回收。
func TestStickyCodeSurvivesTTLWhileShareOnline(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 30 * time.Millisecond})
	_, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	if !resp.CodeSticky || resp.ExpiresAt != 0 {
		t.Fatalf("sticky 客户端应收到 code_sticky 且 expires_at=0，得 %+v", resp)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(srv.sessions.CleanupExpired()); n != 0 {
		t.Fatalf("share 在线时不应被清理，清掉 %d", n)
	}
	if _, ok := stickyJoin(srv, "h1", resp.Code); !ok {
		t.Fatal("超过 TTL 后 share 在线时应仍可加入")
	}
}

// share 掉线后在宽限内以同 ClientID 重连得到同码；超过宽限则回收。
func TestStickyShareReconnectWithinGraceKeepsCode(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 200 * time.Millisecond})
	s1, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	srv.sessions.DisconnectClient(s1.ID)
	time.Sleep(50 * time.Millisecond)
	_, resp2 := stickyRegister(t, srv, "s2", "cid-1", resp.Code, true)
	if resp2.Code != resp.Code {
		t.Fatalf("宽限内重连应同码: %s vs %s", resp.Code, resp2.Code)
	}
	// 重连后再等过原 TTL，仍可 join
	time.Sleep(300 * time.Millisecond)
	if _, ok := stickyJoin(srv, "h1", resp.Code); !ok {
		t.Fatal("重连后超过 TTL 仍应可加入")
	}
}

func TestStickyShareOfflineBeyondGraceReclaimed(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 50 * time.Millisecond})
	s1, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	srv.sessions.DisconnectClient(s1.ID)
	time.Sleep(120 * time.Millisecond)
	if n := len(srv.sessions.CleanupExpired()); n != 1 {
		t.Fatalf("share 离线超宽限应回收 1 个会话，得 %d", n)
	}
	if _, ok := stickyJoin(srv, "h1", resp.Code); ok {
		t.Fatal("回收后码不应可加入")
	}
}

// relay 丢状态（重启）后 share 带 request_code 重新注册，应拿回同一个码。
func TestStickyRequestCodeRestoredAfterRelayRestart(t *testing.T) {
	srv1 := newStickyServer(t, Config{})
	_, resp := stickyRegister(t, srv1, "s1", "cid-1", "", true)

	srv2 := newStickyServer(t, Config{}) // 全新 relay，无任何状态
	_, resp2 := stickyRegister(t, srv2, "s2", "cid-1", resp.Code, true)
	if resp2.Code != resp.Code {
		t.Fatalf("relay 重启后应复用请求的码: %s vs %s", resp.Code, resp2.Code)
	}
	if _, ok := stickyJoin(srv2, "h1", resp.Code); !ok {
		t.Fatal("复用的码应可加入")
	}
}

// 复用冲突：请求的码被别的 share 占用时换新码，被占用者不受影响。
func TestStickyRequestCodeTakenGetsNewCode(t *testing.T) {
	srv := newStickyServer(t, Config{})
	_, a := stickyRegister(t, srv, "sA", "cid-A", "", true)
	_, b := stickyRegister(t, srv, "sB", "cid-B", a.Code, true)
	if b.Code == a.Code {
		t.Fatal("被占用的码不得发给另一个 share")
	}
	if !b.CodeSticky {
		t.Fatal("新 relay 回复应带 code_sticky，供客户端区分「码被占」与「relay 太旧」")
	}
	// A 仍然有效并指向 A 的会话
	h, ok := stickyJoin(srv, "h1", a.Code)
	if !ok {
		t.Fatal("原占用者的码必须仍可加入")
	}
	_ = h
}

// 非法的请求码（长度/字符集不对）被忽略，生成新码。
func TestStickyInvalidRequestCodeIgnored(t *testing.T) {
	srv := newStickyServer(t, Config{})
	for _, bad := range []string{"SHORT", "abcdefghijkl", "AAAAAAAAAAA0", "AAAAAAAAAAAAA"} {
		_, resp := stickyRegister(t, srv, "s-"+bad, "cid-"+bad, bad, true)
		if resp.Code == bad {
			t.Fatalf("非法请求码 %q 不应被采用", bad)
		}
	}
}

// 旧客户端（不声明 supports_sticky_code、不发 request_code）：仍得到滚动到期时间，重连经 ClientID 得同码。
func TestStickyLegacyClientGetsRollingExpiry(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: time.Minute})
	s1, resp := stickyRegister(t, srv, "s1", "cid-1", "", false)
	if resp.ExpiresAt == 0 || time.Until(time.Unix(resp.ExpiresAt, 0)) < 30*time.Second {
		t.Fatalf("旧客户端应收到约 now+TTL 的到期时间，得 %d", resp.ExpiresAt)
	}
	srv.sessions.DisconnectClient(s1.ID)
	_, resp2 := stickyRegister(t, srv, "s2", "cid-1", "", false)
	if resp2.Code != resp.Code {
		t.Fatal("旧客户端经 ClientID 重连应同码")
	}
}

// legacy 开关：恢复硬 TTL，share 在线也过期；且不声明 code_sticky。
func TestLegacyCodeExpiryFlag(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 30 * time.Millisecond, LegacyCodeExpiry: true})
	_, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	if resp.CodeSticky {
		t.Fatal("legacy 模式不应声明 code_sticky")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := stickyJoin(srv, "h1", resp.Code); ok {
		t.Fatal("legacy 模式下 TTL 到期后不应可加入")
	}
}

// --code-max-age：share 在线也到期，到期时刻固定下发。
func TestStickyMaxAge(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: time.Minute, CodeMaxAge: 60 * time.Millisecond})
	_, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	if resp.ExpiresAt == 0 {
		t.Fatal("设置 max-age 时应下发固定到期时刻")
	}
	if _, ok := stickyJoin(srv, "h0", resp.Code); !ok {
		t.Fatal("寿命内应可加入")
	}
	srv.sessions.DisconnectClient("h0")
	time.Sleep(120 * time.Millisecond)
	if _, ok := stickyJoin(srv, "h1", resp.Code); ok {
		t.Fatal("超过 max-age 后不应可加入")
	}
}

// 安全保护不放宽：已有控制端时第二个控制端仍被拒。
func TestStickySingleHelperStillEnforced(t *testing.T) {
	srv := newStickyServer(t, Config{})
	_, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	if _, ok := stickyJoin(srv, "h1", resp.Code); !ok {
		t.Fatal("第一个控制端应可加入")
	}
	if _, ok := stickyJoin(srv, "h2", resp.Code); ok {
		t.Fatal("第二个控制端必须被拒")
	}
}

// no-auth 下不处理 request_code，也不声明 sticky。
func TestStickyNoAuthIgnoresRequestCode(t *testing.T) {
	srv := newStickyServer(t, Config{NoAuth: true})
	_, resp := stickyRegister(t, srv, "s1", "cid-1", "ABCDEFGHJKMN", true)
	if resp.Code != proto.NoAuthCode || resp.CodeSticky {
		t.Fatalf("no-auth 应沿用固定码且不声明 sticky，得 %+v", resp)
	}
}
