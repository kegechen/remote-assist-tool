package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// relay 只发全大写码（见 charset）。未声明 supports_canonical_code 的 help
// （0.0.x–v1.2.3 所有现存版本）保持精确比对：手打小写直接 join failed。
// 绝不能替它放行——会话密钥由两端各自用码派生，help 端用输入的小写派生就与
// share 端（用 relay 下发的原码）不一致，所有工具报认证失败，且会话被占住，
// 正确的协助端反而进不来。
func TestJoinCaseSensitiveWithoutDeclaration(t *testing.T) {
	srv, _ := NewServer(&Config{CodeTTL: 5 * time.Minute, CodeLength: 12})
	share := &ClientConn{ID: "share", Conn: &capturingConn{remoteAddr: "10.0.0.1:5000"}}
	if _, err := srv.sessions.CreateSession("ABCD2345EFGH", share, 30*time.Minute, "", "10.0.0.1", 100, 1000); err != nil {
		t.Fatal(err)
	}

	conn := &capturingConn{remoteAddr: "10.0.0.2:5000"}
	srv.handleJoin(&ClientConn{ID: "h0", Conn: conn}, proto.JoinRequest{Code: "abcd-2345-efgh"})
	if resp := parseLastJoinResponse(t, &conn.buf); resp.Success {
		t.Fatalf("未声明的 help 输入小写码必须被拒绝，得到 %+v", resp)
	}

	ok := &capturingConn{remoteAddr: "10.0.0.3:5000"}
	srv.handleJoin(&ClientConn{ID: "h1", Conn: ok}, proto.JoinRequest{Code: "ABCD-2345-EFGH"})
	resp := parseLastJoinResponse(t, &ok.buf)
	if !resp.Success {
		t.Fatalf("未声明的 help 输入原样大写码应能加入，得到 %+v", resp)
	}
	if resp.Code != "" {
		t.Fatalf("规范码不能发给未声明支持的 help，得到 %q", resp.Code)
	}
}

// 声明了 supports_canonical_code 的 help（新版）输入小写码：relay 按大写匹配并
// 返回实际匹配上的规范码，供其派生会话密钥。
func TestJoinCanonicalCodeDeclared(t *testing.T) {
	srv, _ := NewServer(&Config{CodeTTL: 5 * time.Minute, CodeLength: 12})
	share := &ClientConn{ID: "share", Conn: &capturingConn{remoteAddr: "10.0.0.1:5000"}}
	if _, err := srv.sessions.CreateSession("ABCD2345EFGH", share, 30*time.Minute, "", "10.0.0.1", 100, 1000); err != nil {
		t.Fatal(err)
	}
	conn := &capturingConn{remoteAddr: "10.0.0.2:5000"}
	srv.handleJoin(&ClientConn{ID: "h1", Conn: conn}, proto.JoinRequest{Code: "abcd-2345-efgh", SupportsCanonicalCode: true})
	resp := parseLastJoinResponse(t, &conn.buf)
	if !resp.Success {
		t.Fatalf("声明了支持的 help 输入小写码应能加入，得到 %+v", resp)
	}
	if resp.Code != "ABCD2345EFGH" {
		t.Fatalf("JoinResponse 应携带实际匹配上的规范码，得到 %q", resp.Code)
	}
}

// NoAuth 的固定码是小写的 proto.NoAuthCode，即使 help 声明了支持也不能转大写，
// 也不回规范码。
func TestJoinNoAuthCodeUnaffected(t *testing.T) {
	srv, _ := NewServer(&Config{CodeTTL: 5 * time.Minute, CodeLength: 12, NoAuth: true})
	share := &ClientConn{ID: "share", Conn: &capturingConn{remoteAddr: "10.0.0.1:5000"}}
	if _, err := srv.sessions.CreateSession(proto.NoAuthCode, share, 30*time.Minute, "", "10.0.0.1", 100, 1000); err != nil {
		t.Fatal(err)
	}
	conn := &capturingConn{remoteAddr: "10.0.0.2:5000"}
	srv.handleJoin(&ClientConn{ID: "h1", Conn: conn}, proto.JoinRequest{Code: proto.NoAuthCode, SupportsCanonicalCode: true})
	resp := parseLastJoinResponse(t, &conn.buf)
	if !resp.Success {
		t.Fatalf("NoAuth 固定码 %q 应能加入，得到 %+v", proto.NoAuthCode, resp)
	}
	if resp.Code != "" {
		t.Fatalf("NoAuth 不应回规范码，得到 %q", resp.Code)
	}
	// NoAuth 下唯一的合法码是小写的固定码：输入含小写的错码时不能给「请用大写」
	// 提示——那是永远不可能满足的指引。
	conn2 := &capturingConn{remoteAddr: "10.0.0.3:5000"}
	srv.handleJoin(&ClientConn{ID: "h2", Conn: conn2}, proto.JoinRequest{Code: "NoAuth"})
	if resp := parseLastJoinResponse(t, &conn2.buf); resp.Success || resp.Error != "join failed" {
		t.Fatalf("NoAuth 拒绝含小写的错码应保持通用消息，得到 %q", resp.Error)
	}
}

// 拒绝含小写的码时附带「请用大写」提示，让手打小写的旧 help 拿到可操作的信息
// （0.0.7 起就会原样显示 resp.Error）。只按输入本身的大小写判断，不泄露码是否存在；
// 全大写的错码保持通用消息，不扩大提示面。
func TestJoinRejectHintsUppercase(t *testing.T) {
	srv, _ := NewServer(&Config{CodeTTL: 5 * time.Minute, CodeLength: 12})
	share := &ClientConn{ID: "share", Conn: &capturingConn{remoteAddr: "10.0.0.1:5000"}}
	if _, err := srv.sessions.CreateSession("ABCD2345EFGH", share, 30*time.Minute, "", "10.0.0.1", 100, 1000); err != nil {
		t.Fatal(err)
	}

	conn := &capturingConn{remoteAddr: "10.0.0.2:5000"}
	srv.handleJoin(&ClientConn{ID: "h0", Conn: conn}, proto.JoinRequest{Code: "abcd-2345-efgh"})
	if resp := parseLastJoinResponse(t, &conn.buf); resp.Success || !strings.Contains(resp.Error, "大写") {
		t.Fatalf("拒绝含小写的码应提示用大写，得到 %q", resp.Error)
	}

	conn2 := &capturingConn{remoteAddr: "10.0.0.3:5000"}
	srv.handleJoin(&ClientConn{ID: "h1", Conn: conn2}, proto.JoinRequest{Code: "ABCD2345EFGX"})
	if resp := parseLastJoinResponse(t, &conn2.buf); resp.Success || resp.Error != "join failed" {
		t.Fatalf("拒绝全大写错码应保持通用消息，得到 %q", resp.Error)
	}
}
