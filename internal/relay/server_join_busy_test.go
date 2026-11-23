package relay

import (
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// TestJoinSameSourceExistingHelperGetsExplicitError 验证：
//   - 已有协助端时，同源地址再次加入拿到专用错误（客户端可提示「先断开已有协助端」）；
//   - 异源地址仍返回统一的 "join failed"，不泄露会话状态。
func TestJoinSameSourceExistingHelperGetsExplicitError(t *testing.T) {
	cfg := &Config{CodeTTL: 5 * time.Minute, CodeLength: 10, NoAuth: false, LegacyCodeExpiry: true}
	srv, _ := NewServer(cfg)

	const code = "BUSY000001"
	share := &ClientConn{ID: "share-busy", Conn: &capturingConn{remoteAddr: "10.20.0.1:5000"}}
	sess, err := srv.sessions.CreateSession(code, share, 30*time.Minute, "cid-share", "10.20.0.1", 100, 1000)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first := &ClientConn{ID: "help-first", Conn: &capturingConn{remoteAddr: "10.20.0.9:5100"}}
	if _, err := srv.sessions.JoinSession(code, first); err != nil {
		t.Fatalf("JoinSession: %v", err)
	}
	if !srv.sessions.ActivateDataPlane(sess.ID, share.ID, first.ID) {
		t.Fatal("ActivateDataPlane 失败")
	}

	// 同源地址（同 IP，不同端口）再次加入：应拿到专用错误。
	sameConn := &capturingConn{remoteAddr: "10.20.0.9:5200"}
	same := &ClientConn{ID: "help-again", Conn: sameConn}
	srv.handleJoin(same, code)
	resp := parseLastJoinResponse(t, &sameConn.buf)
	if resp.Success {
		t.Fatal("同源重复加入不应成功")
	}
	if resp.Error != proto.ErrJoinSessionBusySameAddr {
		t.Fatalf("同源重复加入应返回 %q，实际 %q", proto.ErrJoinSessionBusySameAddr, resp.Error)
	}

	// 异源地址加入：仍走统一 "join failed"。
	otherConn := &capturingConn{remoteAddr: "10.20.0.77:5300"}
	other := &ClientConn{ID: "help-other", Conn: otherConn}
	srv.handleJoin(other, code)
	respOther := parseLastJoinResponse(t, &otherConn.buf)
	if respOther.Success {
		t.Fatal("异源加入不应成功")
	}
	if respOther.Error != "join failed" {
		t.Fatalf("异源加入不应泄露会话状态，期望 \"join failed\"，实际 %q", respOther.Error)
	}
}
