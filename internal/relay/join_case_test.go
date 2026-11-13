package relay

import (
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// 协助码只含大写字母和数字：手打成小写（电话里念码、输入法改了大小写）也应能加入，
// 否则失败还会计入 IP 失败计数，几次后被临时封禁。
func TestJoinCodeIsCaseInsensitive(t *testing.T) {
	srv, _ := NewServer(&Config{CodeTTL: 5 * time.Minute, CodeLength: 12})
	share := &ClientConn{ID: "share", Conn: &capturingConn{remoteAddr: "10.0.0.1:5000"}}
	if _, err := srv.sessions.CreateSession("ABCD2345EFGH", share, 30*time.Minute, "", "10.0.0.1", 100, 1000); err != nil {
		t.Fatal(err)
	}

	bad := &capturingConn{remoteAddr: "10.0.0.2:5000"}
	srv.handleJoin(&ClientConn{ID: "h0", Conn: bad}, "abcd-2345-efgx")
	if parseLastJoinResponse(t, &bad.buf).Success {
		t.Fatal("码不对必须仍然失败")
	}

	conn := &capturingConn{remoteAddr: "10.0.0.3:5000"}
	srv.handleJoin(&ClientConn{ID: "h1", Conn: conn}, "abcd-2345-efgh")
	if resp := parseLastJoinResponse(t, &conn.buf); !resp.Success {
		t.Fatalf("小写输入的协助码应能加入，得到 %+v", resp)
	}
}

// NoAuth 模式的固定码是小写的 proto.NoAuthCode，不能被转成大写。
func TestJoinNoAuthCodeUnaffected(t *testing.T) {
	srv, _ := NewServer(&Config{CodeTTL: 5 * time.Minute, CodeLength: 12, NoAuth: true})
	share := &ClientConn{ID: "share", Conn: &capturingConn{remoteAddr: "10.0.0.1:5000"}}
	if _, err := srv.sessions.CreateSession(proto.NoAuthCode, share, 30*time.Minute, "", "10.0.0.1", 100, 1000); err != nil {
		t.Fatal(err)
	}
	conn := &capturingConn{remoteAddr: "10.0.0.2:5000"}
	srv.handleJoin(&ClientConn{ID: "h1", Conn: conn}, proto.NoAuthCode)
	if resp := parseLastJoinResponse(t, &conn.buf); !resp.Success {
		t.Fatalf("NoAuth 固定码 %q 应能加入，得到 %+v", proto.NoAuthCode, resp)
	}
}
