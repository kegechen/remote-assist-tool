package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// share 重连时声明「P2P 直连一直通着」（RegisterRequest.KeepHelp）相关用例。

const keepHelpClientID = "keep-help-client"

// keepHelpRegister 以固定 ClientID 注册一个 share，可带 KeepHelp。返回连接与 RegisterResponse。
func keepHelpRegister(t *testing.T, srv *Server, id, addr, keepHelp string) (*ClientConn, *reuseProtocolConn, proto.RegisterResponse) {
	t.Helper()
	conn := &reuseProtocolConn{remoteAddr: addr}
	c := &ClientConn{ID: id, Conn: conn}
	msg, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{
		Version: "1.2.3", ClientID: keepHelpClientID, KeepHelp: keepHelp,
	})
	if srv.handleMessage(c, msg) {
		t.Fatalf("注册 %s 不应断开", id)
	}
	frames := conn.messages(t)
	if len(frames) == 0 || frames[0].Type != proto.MsgRegisterResponse {
		t.Fatalf("%s 应先收到 RegisterResponse: %+v", id, frames)
	}
	var resp proto.RegisterResponse
	if err := proto.DecodePayload(&frames[0], &resp); err != nil {
		t.Fatal(err)
	}
	return c, conn, resp
}

// keepHelpJoin 让一个 help 加入，返回连接与 share 收到的 SessionReady。
func keepHelpJoin(t *testing.T, srv *Server, id, addr, code string, shareConn *reuseProtocolConn) (*ClientConn, *reuseProtocolConn, proto.SessionReady) {
	t.Helper()
	conn := &reuseProtocolConn{remoteAddr: addr}
	c := &ClientConn{ID: id, Conn: conn}
	msg, _ := proto.NewMessage(proto.MsgJoinRequest, &proto.JoinRequest{Code: code, Version: "1.2.3"})
	if srv.handleMessage(c, msg) {
		t.Fatalf("join %s 不应断开", id)
	}
	frames := shareConn.messages(t)
	last := frames[len(frames)-1]
	if last.Type != proto.MsgSessionReady {
		t.Fatalf("share 最后一帧应是 SessionReady: %+v", frames)
	}
	var ready proto.SessionReady
	if err := proto.DecodePayload(&last, &ready); err != nil {
		t.Fatal(err)
	}
	return c, conn, ready
}

type keepHelpSetup struct {
	srv       *Server
	share     *ClientConn
	shareConn *reuseProtocolConn
	help      *ClientConn
	helpConn  *reuseProtocolConn
	code      string
	sessionID string
	helpTag   string
}

// newKeepHelpSetup share 注册、help 加入，然后 share 的 relay 连接断开（help 那条还在）。
func newKeepHelpSetup(t *testing.T) *keepHelpSetup {
	t.Helper()
	srv, err := NewServer(&Config{CodeTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	share, shareConn, reg := keepHelpRegister(t, srv, "share-1", "203.0.113.7:1", "")
	help, helpConn, ready := keepHelpJoin(t, srv, "help-1", "198.51.100.9:2", reg.Code, shareConn)
	if len(ready.HelpTag) != 26 {
		t.Fatalf("SessionReady 应带 26 字符的协助端标识，实际=%q", ready.HelpTag)
	}
	// 标识是独立生成的随机串，不从连接 ID 派生（不同连接的标识互不相同另见
	// TestShareReconnectDoesNotKeepReplacedHelp）。
	if strings.Trim(ready.HelpTag, "abcdefghijklmnopqrstuvwxyz234567") != "" {
		t.Fatalf("协助端标识应是小写 base32 随机串，实际=%q", ready.HelpTag)
	}
	res := srv.sessions.DisconnectClient(share.ID)
	if res == nil || res.PeerToNotify != help {
		t.Fatalf("share 断开应通知在线的 help: %+v", res)
	}
	return &keepHelpSetup{
		srv: srv, share: share, shareConn: shareConn, help: help, helpConn: helpConn,
		code: reg.Code, sessionID: ready.SessionID, helpTag: ready.HelpTag,
	}
}

// 场景 b（share 与 relay 闪断、P2P 仍通）：share 带着原协助端的标识重连，relay 必须保留配对，
// 不能再踢 help 要它重新加入。
func TestShareReconnectKeepsHelpWhenP2PAlive(t *testing.T) {
	s := newKeepHelpSetup(t)
	helpFramesBefore := len(s.helpConn.messages(t))

	share2, share2Conn, resp := keepHelpRegister(t, s.srv, "share-2", "203.0.113.8:3", s.helpTag)
	if !resp.HelpKept {
		t.Fatal("标识匹配且 help 在线时 relay 应保留配对（HelpKept=true）")
	}
	if resp.Code != s.code {
		t.Fatalf("复用会话码应不变: %q → %q", s.code, resp.Code)
	}
	if frames := share2Conn.messages(t); len(frames) != 1 {
		t.Fatalf("保留配对时新 share 只应收到 RegisterResponse（不会再有 SessionReady）: %+v", frames)
	}
	if got := s.helpConn.messages(t); len(got) != helpFramesBefore {
		t.Fatalf("保留配对时不得通知 help 重新加入: %+v", got[helpFramesBefore:])
	}
	if s.helpConn.isClosed() {
		t.Fatal("保留配对时不得关闭 help 连接")
	}
	if !s.srv.sessions.IsPaired(s.sessionID, share2.ID, s.help.ID) {
		t.Fatal("新 share 应与原 help 配对")
	}
	if s.srv.sessions.FindPeer(s.help.ID) != share2 || s.srv.sessions.FindPeer(share2.ID) != s.help {
		t.Fatal("两端的消息应互相转发到对方的新连接")
	}
	// 保留下来的只有 P2P 直连：UDP 中继数据面不随之复活（端点地址已随 share 换绑清空）。
	if s.srv.sessions.IsActiveDataSession(s.sessionID) {
		t.Fatal("保留配对不应重新激活 UDP 中继数据面")
	}
}

// 没声明、标识对不上、超长：都走旧行为——踢掉 help（PEER_RECONNECTED）并关连接，等它重新加入。
func TestShareReconnectDetachesHelpUnlessTagMatches(t *testing.T) {
	for name, keep := range map[string]func(tag string) string{
		"未声明":  func(string) string { return "" },
		"标识不符": func(tag string) string { return strings.Repeat("a", len(tag)) },
		"超长":   func(tag string) string { return tag + strings.Repeat("a", maxKeepHelpLen) },
	} {
		t.Run(name, func(t *testing.T) {
			s := newKeepHelpSetup(t)
			share2, _, resp := keepHelpRegister(t, s.srv, "share-2", "203.0.113.8:3", keep(s.helpTag))
			if resp.HelpKept {
				t.Fatal("不应保留配对")
			}
			if len(share2.keepHelp) > maxKeepHelpLen {
				t.Fatalf("超长的 KeepHelp 不得存进连接状态: %d 字节", len(share2.keepHelp))
			}
			assertHelpDetached(t, s, share2)
		})
	}
}

// help 正在断连去抖中（它那条 relay 连接也断了）：保留一个已经不在的连接没有意义，走旧行为。
func TestShareReconnectDoesNotKeepHelpInDebounce(t *testing.T) {
	s := newKeepHelpSetup(t)
	s.srv.sessions.DisconnectClient(s.help.ID)
	share2, _, resp := keepHelpRegister(t, s.srv, "share-2", "203.0.113.8:3", s.helpTag)
	if resp.HelpKept {
		t.Fatal("help 在断连去抖中时不应保留配对")
	}
	if s.srv.sessions.FindPeer(share2.ID) != nil {
		t.Fatal("新 share 不应配到正在断开的 help")
	}
}

// 场景 d 的 relay 一侧：share 掉线前已经换了协助端（A 去抖中 B 加入，B 的 SessionReady 没送到
// share），share 拿着 A 的标识重连。在线的是 B，绝不能把 B 配给 A 的隧道。
func TestShareReconnectDoesNotKeepReplacedHelp(t *testing.T) {
	srv, err := NewServer(&Config{CodeTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	share, shareConn, reg := keepHelpRegister(t, srv, "share-1", "203.0.113.7:1", "")
	helpA, _, readyA := keepHelpJoin(t, srv, "help-a", "198.51.100.9:2", reg.Code, shareConn)
	srv.sessions.DisconnectClient(helpA.ID)
	helpB, helpBConn, readyB := keepHelpJoin(t, srv, "help-b", "198.51.100.10:4", reg.Code, shareConn)
	if readyA.HelpTag == readyB.HelpTag {
		t.Fatal("不同协助端连接的标识必须不同")
	}
	srv.sessions.DisconnectClient(share.ID)

	share2, _, resp := keepHelpRegister(t, srv, "share-2", "203.0.113.8:3", readyA.HelpTag)
	if resp.HelpKept {
		t.Fatal("在线的已是另一个协助端，不应保留配对")
	}
	assertHelpDetached(t, &keepHelpSetup{srv: srv, help: helpB, helpConn: helpBConn}, share2)
}

// assertHelpDetached 旧行为：help 收到一次 PEER_RECONNECTED、连接被关，新 share 没有配对。
func assertHelpDetached(t *testing.T, s *keepHelpSetup, share2 *ClientConn) {
	t.Helper()
	frames := s.helpConn.messages(t)
	last := frames[len(frames)-1]
	var errMsg proto.ErrorMessage
	if last.Type != proto.MsgError || proto.DecodePayload(&last, &errMsg) != nil || errMsg.Code != proto.ErrCodePeerReconnected {
		t.Fatalf("help 应收到 PEER_RECONNECTED: %+v", frames)
	}
	if !s.helpConn.isClosed() {
		t.Fatal("help 连接应被关闭")
	}
	if s.srv.sessions.FindPeer(share2.ID) != nil {
		t.Fatal("新 share 在 help 重新加入前不应有配对")
	}
}
