package relay

import (
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// addrConn is a Conn whose RemoteAddr is configurable. MockConn
// (session_test.go) has a fixed addr and must not be redeclared here.
type addrConn struct{ addr string }

func (c *addrConn) Read([]byte) (int, error)   { return 0, nil }
func (c *addrConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *addrConn) Close() error                { return nil }
func (c *addrConn) RemoteAddr() string          { return c.addr }

// sanitizePeerString runs at ingestion on peer-controlled display strings:
// control chars (log/JSON injection) and Unicode format chars (bidi/zero-width
// display spoofing) must be dropped and the length capped; normal identity
// strings must pass through unchanged.
func TestSanitizePeerString(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"normal", "alice@pc1 Windows 11 amd64", "alice@pc1 Windows 11 amd64"},
		{"empty (old client)", "", ""},
		{"trims whitespace", "  bob@pc2 uos arm64\n", "bob@pc2 uos arm64"},
		{"strips control chars", "a\x00b\nc\x1bd\x7fe", "abcde"},
		{"strips bidi override", "evil‮gnp.exe", "evilgnp.exe"},
		{"strips zero-width chars", "a​b‍c⁦d", "abcd"},
		{"unicode kept", "张三@工作机 kylin arm64", "张三@工作机 kylin arm64"},
	}
	for _, tc := range cases {
		if got := sanitizePeerString(tc.in); got != tc.want {
			t.Errorf("%s: sanitizePeerString(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	long := strings.Repeat("x", maxPeerStringLen+50)
	if got := sanitizePeerString(long); len([]rune(got)) != maxPeerStringLen {
		t.Errorf("long input: got %d runes, want cap %d", len([]rune(got)), maxPeerStringLen)
	}
}

// TestHandleMessageIngestsHost covers the wire path inside the relay:
// RegisterRequest/JoinRequest carrying a peer-controlled host string must land
// in ClientConn.Host sanitized (whitespace trimmed, control chars stripped).
func TestHandleMessageIngestsHost(t *testing.T) {
	srv, err := NewServer(&Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	share := &ClientConn{ID: "share1", Conn: &addrConn{addr: "203.0.113.7:1"}}
	regMsg, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{
		Version: "v1",
		Host:    " alice@pc1 Windows 11 amd64\n",
	})
	srv.handleMessage(share, regMsg)

	if share.Host != "alice@pc1 Windows 11 amd64" {
		t.Errorf("share.Host = %q, want sanitized %q", share.Host, "alice@pc1 Windows 11 amd64")
	}

	// 拿到 share 注册出的 code 供 help 加入
	var code string
	for c := range srv.sessions.byCode {
		code = c
	}
	if code == "" {
		t.Fatal("no session registered")
	}

	help := &ClientConn{ID: "help1", Conn: &addrConn{addr: "198.51.100.9:2"}}
	joinMsg, _ := proto.NewMessage(proto.MsgJoinRequest, &proto.JoinRequest{
		Code: code,
		Host: "bob@pc2 uos arm64\x00",
	})
	srv.handleMessage(help, joinMsg)

	if help.Host != "bob@pc2 uos arm64" {
		t.Errorf("help.Host = %q, want sanitized %q", help.Host, "bob@pc2 uos arm64")
	}
	if help.Type != "help" {
		t.Errorf("help.Type = %q, want %q (join must succeed)", help.Type, "help")
	}
}

// TestHandleMessageOldClientNoHost pins the backward-compat wire contract:
// old clients omit the host field entirely and Host must stay empty.
func TestHandleMessageOldClientNoHost(t *testing.T) {
	srv, err := NewServer(&Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	share := &ClientConn{ID: "share1", Conn: &addrConn{addr: "203.0.113.7:1"}}
	regMsg, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "v0"})
	srv.handleMessage(share, regMsg)

	if share.Host != "" {
		t.Errorf("share.Host = %q, want empty for old client (no host field)", share.Host)
	}
	if share.Version != "v0" {
		t.Errorf("share.Version = %q, want %q", share.Version, "v0")
	}
}

// TestHandleMessagePeerFieldsFirstFrameOnly pins two hardening behaviors:
// (1) Version passes through sanitizePeerString like Host; (2) identity
// fields are written only on the connection's FIRST register/join frame —
// once the ClientConn is published into a session other goroutines may read
// the fields concurrently and a re-sent frame must not rewrite them.
func TestHandleMessagePeerFieldsFirstFrameOnly(t *testing.T) {
	srv, err := NewServer(&Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	share := &ClientConn{ID: "share1", Conn: &addrConn{addr: "203.0.113.7:1"}}
	regMsg, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{
		Version: " v1\x00",
		Host:    "alice@pc1 Windows 11 amd64",
	})
	srv.handleMessage(share, regMsg)

	if share.Version != "v1" {
		t.Errorf("share.Version = %q, want sanitized %q", share.Version, "v1")
	}

	// 异常客户端在同一连接重发 register：不得改写已发布的身份字段。
	reg2, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{
		Version: "v2",
		Host:    "mallory@evil uos amd64",
	})
	srv.handleMessage(share, reg2)

	if share.Host != "alice@pc1 Windows 11 amd64" {
		t.Errorf("share.Host = %q, want first-frame value preserved", share.Host)
	}
	if share.Version != "v1" {
		t.Errorf("share.Version = %q, want first-frame value preserved", share.Version)
	}
}
