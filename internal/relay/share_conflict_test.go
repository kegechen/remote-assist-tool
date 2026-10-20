package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// registerShare 模拟一次 share 注册：返回连接、handleRegister 是否要求关闭、收到的首条消息。
func registerShare(t *testing.T, srv *Server, id, clientID, version string) (*ClientConn, *reuseProtocolConn, bool, proto.Message) {
	t.Helper()
	conn := &reuseProtocolConn{remoteAddr: "10.9.0.1:4000"}
	c := &ClientConn{ID: id, ClientID: clientID, Version: version, Host: "u@pc kylin arm64", Conn: conn}
	closeConn := srv.handleRegister(c)
	msgs := conn.messages(t)
	if len(msgs) == 0 {
		t.Fatalf("%s 注册后没收到任何消息", id)
	}
	return c, conn, closeConn, msgs[0]
}

func errorCodeOf(t *testing.T, msg proto.Message) string {
	t.Helper()
	if msg.Type != proto.MsgError {
		return ""
	}
	var e proto.ErrorMessage
	if err := proto.DecodePayload(&msg, &e); err != nil {
		t.Fatal(err)
	}
	return e.Code
}

// TestSameClientIDPingPongIsStopped 复现线上事故：同一台机器上跑着两个使用同一持久
// ClientID 的 share（旧版 0.0.6 + 新版），relay 每接入一个就把另一个顶掉，被顶掉的立即
// 重连，40s 内建连 20+ 次被 fail2ban 封 IP。relay 必须识别这种互踢，拒绝后来者而不是
// 继续顶掉在线的那个，否则两边会无限循环。
func TestSameClientIDPingPongIsStopped(t *testing.T) {
	srv, err := NewServer(&Config{CodeTTL: time.Hour, CodeLength: 10, DisableSourceIPLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	const clientID = "cid-same-machine"

	first, _, closeConn, resp := registerShare(t, srv, "share-0", clientID, "0.0.6")
	if closeConn || resp.Type != proto.MsgRegisterResponse {
		t.Fatalf("首次注册应成功: close=%v type=%s", closeConn, resp.Type)
	}
	session, ok := srv.sessions.GetSessionByClientID(clientID)
	if !ok {
		t.Fatal("首次注册后应存在会话")
	}
	sessionID, code := session.ID, session.Code

	versions := []string{"v1.1.0", "0.0.6"}
	online := first
	var conflicted int
	for i := 1; i <= 20; i++ {
		c, conn, closeConn, resp := registerShare(t, srv, "share-"+string(rune('a'+i)), clientID, versions[i%2])
		if errorCodeOf(t, resp) == proto.ErrCodeClientIDConflict {
			conflicted++
			if !closeConn {
				t.Fatalf("第 %d 次：被判冲突的连接应关闭", i)
			}
			var e proto.ErrorMessage
			_ = proto.DecodePayload(&resp, &e)
			if !strings.Contains(e.Message, online.Version) {
				t.Fatalf("冲突提示应带上在线进程的版本 %q，实际=%q", online.Version, e.Message)
			}
			_ = conn
			continue
		}
		if resp.Type != proto.MsgRegisterResponse {
			t.Fatalf("第 %d 次：意外响应 %s", i, resp.Type)
		}
		if conflicted > 0 {
			t.Fatalf("第 %d 次：进入冲突冷却后不得再顶掉在线 share", i)
		}
		// 模拟被顶掉的旧连接读循环结束：DisconnectClient 对已换绑的连接应是 no-op。
		srv.sessions.DisconnectClient(online.ID)
		online = c
	}
	if conflicted == 0 {
		t.Fatal("20 次互踢后 relay 仍未识别冲突")
	}
	if conflicted < 15 {
		t.Fatalf("冲突识别过晚：只拒绝了 %d 次，互踢仍持续太久", conflicted)
	}

	s, ok := srv.sessions.GetSessionByClientID(clientID)
	if !ok || s.ID != sessionID || s.Code != code {
		t.Fatal("冲突拒绝不得新建会话或更换协助码")
	}
	if s.Share != online {
		t.Fatal("在线的 share 应保持不变")
	}
}

// TestConflictCooldownReleasedWhenOnlineShareLeaves 冷却期内在线 share 真断开后，
// 后来者应能正常接管同一会话（不能把这台机器永久锁死）。
func TestConflictCooldownReleasedWhenOnlineShareLeaves(t *testing.T) {
	srv, err := NewServer(&Config{CodeTTL: time.Hour, CodeLength: 10, DisableSourceIPLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	const clientID = "cid-release"
	online, _, _, _ := registerShare(t, srv, "s0", clientID, "0.0.6")
	for i := 1; i <= 10; i++ {
		c, _, _, resp := registerShare(t, srv, "s"+string(rune('a'+i)), clientID, "v1.1.0")
		if resp.Type == proto.MsgRegisterResponse {
			srv.sessions.DisconnectClient(online.ID)
			online = c
		}
	}
	// 在线 share 真正断开
	srv.sessions.DisconnectClient(online.ID)

	_, _, closeConn, resp := registerShare(t, srv, "s-final", clientID, "v1.1.0")
	if closeConn || resp.Type != proto.MsgRegisterResponse {
		t.Fatalf("在线 share 离开后应允许接管: close=%v type=%s code=%s", closeConn, resp.Type, errorCodeOf(t, resp))
	}
}

// TestOccasionalReconnectStillReplaces 单进程偶发重连（旧连接半开、relay 尚未感知）仍按原
// 语义顶掉旧连接，不能被误判为冲突。
func TestOccasionalReconnectStillReplaces(t *testing.T) {
	srv, err := NewServer(&Config{CodeTTL: time.Hour, CodeLength: 10, DisableSourceIPLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	const clientID = "cid-flaky"
	online, _, _, _ := registerShare(t, srv, "f0", clientID, "v1.1.0")
	for i := 1; i <= shareConflictMaxReplaces; i++ {
		c, _, closeConn, resp := registerShare(t, srv, "f"+string(rune('a'+i)), clientID, "v1.1.0")
		if closeConn || resp.Type != proto.MsgRegisterResponse {
			t.Fatalf("第 %d 次偶发重连被误拒: code=%s", i, errorCodeOf(t, resp))
		}
		srv.sessions.DisconnectClient(online.ID)
		online = c
	}
}
