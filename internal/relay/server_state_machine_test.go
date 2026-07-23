package relay

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

const ttl = 30 * time.Minute

// TestRegisterOnlyOncePerConnection 单连接只能 register 一次，第二次必须关闭连接。
func TestRegisterOnlyOncePerConnection(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	conn := &capturingConn{remoteAddr: "10.1.1.1:5000"}
	client := &ClientConn{ID: "c1", Conn: conn, ClientID: "cid-1"}

	// 第一次 register：应成功
	req := &proto.RegisterRequest{ClientID: "cid-1", Version: "1.0", Host: "host1"}
	msg, _ := proto.NewMessage(proto.MsgRegisterRequest, req)
	if srv.handleMessage(client, msg) {
		t.Fatal("第一次 register 不应关闭连接")
	}
	if client.Type != connStateShare {
		t.Fatalf("第一次 register 后 client.Type 应为 %q，得到 %q", connStateShare, client.Type)
	}

	// 记录第一次 register 后的会话数
	firstCount := srv.sessions.GetActiveSessions()

	// 第二次 register：必须返回 closeConn=true
	req2 := &proto.RegisterRequest{ClientID: "cid-2", Version: "2.0", Host: "host2"}
	msg2, _ := proto.NewMessage(proto.MsgRegisterRequest, req2)
	if !srv.handleMessage(client, msg2) {
		t.Fatal("第二次 register 必须返回 closeConn=true")
	}

	// 断言：第二次 register 不应创建新会话
	secondCount := srv.sessions.GetActiveSessions()
	if secondCount != firstCount {
		t.Fatalf("第二次 register 不应增加会话数，期望 %d 得到 %d", firstCount, secondCount)
	}
}

// TestJoinOnlyOncePerConnection 单连接只能 join 一次，第二次必须关闭连接。
func TestJoinOnlyOncePerConnection(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	// 创建一个 Share 会话
	share := &ClientConn{ID: "share1", Conn: &capturingConn{remoteAddr: "10.2.2.2:5000"}}
	srv.sessions.CreateSession("CODE0001", share, ttl, "", "10.2.2.2", 100, 1000)

	conn := &capturingConn{remoteAddr: "10.2.2.3:5001"}
	client := &ClientConn{ID: "help1", Conn: conn}

	// 第一次 join：应成功
	req := &proto.JoinRequest{Code: "CODE0001", Version: "1.0", Host: "host1"}
	msg, _ := proto.NewMessage(proto.MsgJoinRequest, req)
	if srv.handleMessage(client, msg) {
		t.Fatal("第一次 join 不应关闭连接")
	}
	if client.Type != connStateHelp {
		t.Fatalf("第一次 join 后 client.Type 应为 %q，得到 %q", connStateHelp, client.Type)
	}

	// 第二次 join：必须返回 closeConn=true
	req2 := &proto.JoinRequest{Code: "CODE0002", Version: "2.0", Host: "host2"}
	msg2, _ := proto.NewMessage(proto.MsgJoinRequest, req2)
	if !srv.handleMessage(client, msg2) {
		t.Fatal("第二次 join 必须返回 closeConn=true")
	}
}

// TestRegisterAfterJoinRejected join 后不能再 register。
func TestRegisterAfterJoinRejected(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	share := &ClientConn{ID: "share1", Conn: &capturingConn{remoteAddr: "10.3.3.3:5000"}}
	srv.sessions.CreateSession("CODE0002", share, ttl, "", "10.3.3.3", 100, 1000)

	conn := &capturingConn{remoteAddr: "10.3.3.4:5001"}
	client := &ClientConn{ID: "help1", Conn: conn}

	// 先 join
	jreq := &proto.JoinRequest{Code: "CODE0002", Version: "1.0", Host: "host1"}
	jmsg, _ := proto.NewMessage(proto.MsgJoinRequest, jreq)
	srv.handleMessage(client, jmsg)

	// 再 register：必须被拒绝
	rreq := &proto.RegisterRequest{ClientID: "cid-1", Version: "2.0", Host: "host2"}
	rmsg, _ := proto.NewMessage(proto.MsgRegisterRequest, rreq)
	if !srv.handleMessage(client, rmsg) {
		t.Fatal("join 后 register 必须返回 closeConn=true")
	}
}

// TestJoinAfterRegisterRejected register 后不能再 join。
func TestJoinAfterRegisterRejected(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	conn := &capturingConn{remoteAddr: "10.4.4.4:5000"}
	client := &ClientConn{ID: "share1", Conn: conn, ClientID: "cid-1"}

	// 先 register
	rreq := &proto.RegisterRequest{ClientID: "cid-1", Version: "1.0", Host: "host1"}
	rmsg, _ := proto.NewMessage(proto.MsgRegisterRequest, rreq)
	srv.handleMessage(client, rmsg)

	// 再 join：必须被拒绝
	jreq := &proto.JoinRequest{Code: "SOMECODE", Version: "2.0", Host: "host2"}
	jmsg, _ := proto.NewMessage(proto.MsgJoinRequest, jreq)
	if !srv.handleMessage(client, jmsg) {
		t.Fatal("register 后 join 必须返回 closeConn=true")
	}
}

// TestBusinessMessageBeforeRegisterRejected 业务消息在注册前必须被拒绝。
func TestBusinessMessageBeforeRegisterRejected(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	conn := &capturingConn{remoteAddr: "10.5.5.5:5000"}
	client := &ClientConn{ID: "c1", Conn: conn}

	// 未 register/join 就发业务消息
	msg := &proto.Message{Type: proto.MsgHeartbeat}
	if !srv.handleMessage(client, msg) {
		t.Fatal("注册前的业务消息必须返回 closeConn=true")
	}

	msg2 := &proto.Message{Type: proto.MsgTunnelData, Payload: json.RawMessage(`{}`)}
	client2 := &ClientConn{ID: "c2", Conn: &capturingConn{remoteAddr: "10.5.5.6:5001"}}
	if !srv.handleMessage(client2, msg2) {
		t.Fatal("注册前的 TunnelData 必须返回 closeConn=true")
	}
}

// TestMalformedRegisterPayloadClosesConnection register payload 解码失败必须立即关闭。
func TestMalformedRegisterPayloadClosesConnection(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	conn := &capturingConn{remoteAddr: "10.6.6.6:5000"}
	client := &ClientConn{ID: "c1", Conn: conn}

	// 畸形 payload
	msg := &proto.Message{Type: proto.MsgRegisterRequest, Payload: json.RawMessage(`{malformed`)}
	if !srv.handleMessage(client, msg) {
		t.Fatal("畸形 register payload 必须返回 closeConn=true")
	}
}

// TestCreateSessionRespectPerIPLimit 单 IP 会话上限生效。
func TestCreateSessionRespectPerIPLimit(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	ip := "10.7.7.7"
	// 创建 maxActiveSessionsPerIP 个会话
	for i := 0; i < maxActiveSessionsPerIP; i++ {
		share := &ClientConn{ID: genID(), Conn: &capturingConn{remoteAddr: ip + ":5000"}}
		srv.sessions.CreateSession(genID(), share, ttl, "", ip, maxActiveSessionsPerIP, maxActiveSessionsTotal)
	}

	// 再创建一个应失败
	share := &ClientConn{ID: "overflow", Conn: &capturingConn{remoteAddr: ip + ":5001"}}
	_, err := srv.sessions.CreateSession("overflow", share, ttl, "", ip, maxActiveSessionsPerIP, maxActiveSessionsTotal)
	if err == nil {
		t.Fatal("超出 per-IP 上限的 CreateSession 应返回 error")
	}
}

// TestCreateSessionRespectGlobalLimit 全局会话上限生效。
func TestCreateSessionRespectGlobalLimit(t *testing.T) {
	cfg := &Config{CodeTTL: ttl, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	limit := 50
	// 创建 limit 个会话（用不同 IP）
	for i := 0; i < limit; i++ {
		ip := genIP(i)
		share := &ClientConn{ID: genID(), Conn: &capturingConn{remoteAddr: ip + ":5000"}}
		srv.sessions.CreateSession(genID(), share, ttl, "", ip, 100, limit)
	}

	// 再创建一个应失败
	share := &ClientConn{ID: "overflow", Conn: &capturingConn{remoteAddr: "200.0.0.1:5000"}}
	_, err := srv.sessions.CreateSession("overflow", share, ttl, "", "200.0.0.1", 100, limit)
	if err == nil {
		t.Fatal("超出全局上限的 CreateSession 应返回 error")
	}
}

// 辅助函数
func genID() string {
	return randomString(8)
}

func genIP(i int) string {
	return fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
}
