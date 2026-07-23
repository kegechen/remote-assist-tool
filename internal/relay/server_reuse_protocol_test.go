package relay

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

type reuseProtocolConn struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	closed     bool
	remoteAddr string
}

func (c *reuseProtocolConn) Read([]byte) (int, error) { return 0, nil }
func (c *reuseProtocolConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}
func (c *reuseProtocolConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *reuseProtocolConn) RemoteAddr() string               { return c.remoteAddr }
func (c *reuseProtocolConn) SetWriteDeadline(time.Time) error { return nil }

func (c *reuseProtocolConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *reuseProtocolConn) messages(t *testing.T) []proto.Message {
	t.Helper()
	c.mu.Lock()
	data := append([]byte(nil), c.buf.Bytes()...)
	c.mu.Unlock()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(data) == 0 {
		return nil
	}
	msgs := make([]proto.Message, 0, len(lines))
	for _, line := range lines {
		var msg proto.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("解析消息 %q: %v", line, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestRegisterReuseClosesOldPeersAndRequiresFreshJoin(t *testing.T) {
	const (
		code     = "SECRET1234"
		clientID = "persistent-client-secret"
	)
	srv, err := NewServer(&Config{CodeTTL: time.Minute, CodeLength: 10})
	if err != nil {
		t.Fatal(err)
	}
	oldShareConn := &reuseProtocolConn{remoteAddr: "10.1.0.1:1001"}
	oldShare := &ClientConn{ID: "share-old", ClientID: clientID, Conn: oldShareConn}
	session, err := srv.sessions.CreateSession(code, oldShare, time.Minute, clientID, "10.1.0.1", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	oldHelpConn := &reuseProtocolConn{remoteAddr: "10.1.0.2:1002"}
	oldHelp := &ClientConn{ID: "help-old", Conn: oldHelpConn}
	if _, err := srv.sessions.JoinSession(code, oldHelp); err != nil {
		t.Fatal(err)
	}
	if !srv.sessions.ActivateDataPlane(session.ID, oldShare.ID, oldHelp.ID) {
		t.Fatal("测试前置激活失败")
	}

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousOutput)

	newShareConn := &reuseProtocolConn{remoteAddr: "10.2.0.1:2001"}
	newShare := &ClientConn{ID: "share-new", ClientID: clientID, Conn: newShareConn}
	if srv.handleRegister(newShare) {
		t.Fatal("复用注册不应关闭新 Share")
	}
	if !oldShareConn.isClosed() || !oldHelpConn.isClosed() {
		t.Fatal("复用后旧 Share 与旧 Help 连接都必须关闭")
	}
	if srv.sessions.FindPeer(newShare.ID) != nil || srv.sessions.IsActiveDataSession(session.ID) {
		t.Fatal("新 Help Join 前不得存在 peer 或活跃数据面")
	}

	newMessages := newShareConn.messages(t)
	if len(newMessages) != 1 || newMessages[0].Type != proto.MsgRegisterResponse {
		t.Fatalf("新 Share 应只收到 RegisterResponse，实际=%v", newMessages)
	}
	helpMessages := oldHelpConn.messages(t)
	if len(helpMessages) != 1 || helpMessages[0].Type != proto.MsgError {
		t.Fatalf("旧 Help 应收到一次重连错误，实际=%v", helpMessages)
	}
	var peerErr proto.ErrorMessage
	if err := proto.DecodePayload(&helpMessages[0], &peerErr); err != nil {
		t.Fatal(err)
	}
	if peerErr.Code != "PEER_RECONNECTED" {
		t.Fatalf("旧 Help 错误码=%q", peerErr.Code)
	}
	if strings.Contains(logs.String(), code) || strings.Contains(logs.String(), clientID) {
		t.Fatalf("普通日志泄露原始凭据: %s", logs.String())
	}
}
