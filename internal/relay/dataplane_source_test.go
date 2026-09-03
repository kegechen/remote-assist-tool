package relay

import (
	"net"
	"testing"
	"time"
)

// newSessionWithAddrs 建立一个两端 TCP 源地址可控、数据面已激活的会话。
func newSessionWithAddrs(t *testing.T, shareAddr, helpAddr string) (*SessionManager, *TunnelSession) {
	t.Helper()
	sm := NewSessionManager()
	share := &ClientConn{ID: "share-src", Conn: &addrConn{addr: shareAddr}}
	help := &ClientConn{ID: "help-src", Conn: &addrConn{addr: helpAddr}}
	session, err := sm.CreateSession("SRCCODE", share, 30*time.Minute, "cid-src", "203.0.113.10", 100, 1000)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := sm.JoinSession("SRCCODE", help); err != nil {
		t.Fatalf("JoinSession: %v", err)
	}
	if !sm.ActivateDataPlane(session.ID, share.ID, help.ID) {
		t.Fatal("ActivateDataPlane 应成功")
	}
	return sm, session
}

// TestIsActiveDataSourceRejectsThirdParty 核心回归：会话活跃不等于任何人都能占 relay 槽位。
// 拿到 sessionID 的第三方从别的 IP 发包必须被拒，否则先到先得即可中间人。
func TestIsActiveDataSourceRejectsThirdParty(t *testing.T) {
	sm, session := newSessionWithAddrs(t, "203.0.113.10:5000", "198.51.100.20:6000")

	if !sm.IsActiveDataSession(session.ID) {
		t.Fatal("前置条件：会话应处于 active data session")
	}
	if sm.IsActiveDataSource(session.ID, net.ParseIP("192.0.2.66")) {
		t.Fatal("第三方 IP 必须被拒绝，否则 relay 槽位是先到先得")
	}
}

func TestIsActiveDataSourceAcceptsSessionEndpoints(t *testing.T) {
	sm, session := newSessionWithAddrs(t, "203.0.113.10:5000", "198.51.100.20:6000")

	for _, ip := range []string{"203.0.113.10", "198.51.100.20"} {
		if !sm.IsActiveDataSource(session.ID, net.ParseIP(ip)) {
			t.Errorf("会话端点 %s 应被接受（TCP 源 IP 即可作为凭据）", ip)
		}
	}
}

// TestIsActiveDataSourceIgnoresPort 对称 NAT 下每个目的地对应不同外部端口，
// 45f72ff 的"按 IP 重匹配、更新端口"依赖这一点；收紧来源校验不能把它打回原形。
func TestIsActiveDataSourceIgnoresPort(t *testing.T) {
	sm, session := newSessionWithAddrs(t, "203.0.113.10:5000", "198.51.100.20:6000")

	sm.UpdatePeerAddr("share-src", "203.0.113.10:41001", "192.168.1.5:41001", "symmetric")
	// 换了外部端口的同一个 IP 仍应放行。
	if !sm.IsActiveDataSource(session.ID, net.ParseIP("203.0.113.10")) {
		t.Fatal("同 IP 换端口必须仍然放行，否则对称 NAT 回退失效")
	}
}

// TestIsActiveDataSourceAcceptsAdvertisedPrivateAddr 两端同 LAN 时 relay 看到的
// UDP 源就是私网地址，必须认。
func TestIsActiveDataSourceAcceptsAdvertisedPrivateAddr(t *testing.T) {
	sm, session := newSessionWithAddrs(t, "203.0.113.10:5000", "198.51.100.20:6000")

	sm.UpdatePeerAddr("help-src", "198.51.100.20:7000", "10.1.2.3:7000", "cone")
	if !sm.IsActiveDataSource(session.ID, net.ParseIP("10.1.2.3")) {
		t.Fatal("已通告的私网地址应被接受")
	}
	if sm.IsActiveDataSource(session.ID, net.ParseIP("10.1.2.4")) {
		t.Fatal("未通告的邻近私网地址不应被接受")
	}
}

// TestIsActiveDataSourceInheritsActivityChecks 来源校验是附加条件，不能弱化原有的活跃判定。
func TestIsActiveDataSourceInheritsActivityChecks(t *testing.T) {
	sm := NewSessionManager()
	share := &ClientConn{ID: "share-src", Conn: &addrConn{addr: "203.0.113.10:5000"}}
	help := &ClientConn{ID: "help-src", Conn: &addrConn{addr: "198.51.100.20:6000"}}
	session, err := sm.CreateSession("SRCCODE2", share, 30*time.Minute, "", "203.0.113.10", 100, 1000)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := sm.JoinSession("SRCCODE2", help); err != nil {
		t.Fatalf("JoinSession: %v", err)
	}

	// 未 ActivateDataPlane：即便来源正确也不得放行。
	if sm.IsActiveDataSource(session.ID, net.ParseIP("203.0.113.10")) {
		t.Fatal("数据面未激活时不得放行，哪怕来源 IP 正确")
	}
	if !sm.ActivateDataPlane(session.ID, share.ID, help.ID) {
		t.Fatal("ActivateDataPlane 应成功")
	}
	if !sm.IsActiveDataSource(session.ID, net.ParseIP("203.0.113.10")) {
		t.Fatal("激活后正确来源应放行")
	}
}

func TestIsActiveDataSourceRejectsUnknownSessionAndNilIP(t *testing.T) {
	sm, session := newSessionWithAddrs(t, "203.0.113.10:5000", "198.51.100.20:6000")

	if sm.IsActiveDataSource("ses_nonexistent", net.ParseIP("203.0.113.10")) {
		t.Error("不存在的会话不应放行")
	}
	if sm.IsActiveDataSource(session.ID, nil) {
		t.Error("来源 IP 为 nil 时不应放行")
	}
}

// TestHostMatchesIPRejectsNonIPHost 通告字段是对端自报的，允许域名等于让校验可绕过。
func TestHostMatchesIPRejectsNonIPHost(t *testing.T) {
	cases := []struct {
		hostPort string
		ip       string
		want     bool
	}{
		{"203.0.113.10:5000", "203.0.113.10", true},
		{"203.0.113.10", "203.0.113.10", true},
		{"[2001:db8::1]:5000", "2001:db8::1", true},
		{"evil.example:5000", "203.0.113.10", false},
		{"", "203.0.113.10", false},
		{"203.0.113.11:5000", "203.0.113.10", false},
	}
	for _, tc := range cases {
		if got := hostMatchesIP(tc.hostPort, net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("hostMatchesIP(%q, %s) = %v, want %v", tc.hostPort, tc.ip, got, tc.want)
		}
	}
}
