package proto

import "testing"

const testPunchCode = "ABCD-2345"

// newSignedPunch 造一个由 share 端签好名的打洞包。
func newSignedPunch(code string, isShare bool) *P2PTestPacket {
	pkt := &P2PTestPacket{SessionID: "ses_punch_1", Random: "r0"}
	SignPunchPacket(pkt, code, isShare)
	return pkt
}

func TestPunchRoundTrip(t *testing.T) {
	// share 发、help 收：help 校验时 peerIsShare = true。
	pkt := newSignedPunch(testPunchCode, true)
	if pkt.MAC == "" {
		t.Fatal("SignPunchPacket 未填 MAC")
	}
	if !VerifyPunchPacket(pkt, testPunchCode, true) {
		t.Error("同码同方向应验通过")
	}
}

// TestPunchRejectsWrongCode 这是整条修复的核心断言：不知道协助码就伪造不出打洞包，
// 于是「拿到 sessionID 就能冒充对端」这条路被堵死。
func TestPunchRejectsWrongCode(t *testing.T) {
	pkt := newSignedPunch(testPunchCode, true)
	if VerifyPunchPacket(pkt, "WXYZ-6789", true) {
		t.Fatal("换一个协助码竟然验通过了")
	}
}

// TestPunchRejectsReflection 把我发出的包原样打回来不能算对端应答，否则攻击者不用
// 知道协助码，只要能看到一个包就能"回声"成对端。
func TestPunchRejectsReflection(t *testing.T) {
	// share 端发出的包，share 端自己收到：校验时 peerIsShare = false（期待对端是 help）。
	pkt := newSignedPunch(testPunchCode, true)
	if VerifyPunchPacket(pkt, testPunchCode, false) {
		t.Fatal("反射回来的自己的包被当成了对端应答")
	}
}

func TestPunchRejectsTamperedFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*P2PTestPacket)
	}{
		{"改会话 ID", func(p *P2PTestPacket) { p.SessionID = "ses_other" }},
		{"改随机串", func(p *P2PTestPacket) { p.Random = "r1" }},
		{"清空 MAC", func(p *P2PTestPacket) { p.MAC = "" }},
		{"改 MAC", func(p *P2PTestPacket) { p.MAC = "AAAAAAAAAAAAAAAAAAAAAA" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkt := newSignedPunch(testPunchCode, true)
			tc.mutate(pkt)
			if VerifyPunchPacket(pkt, testPunchCode, true) {
				t.Error("篡改后仍验通过")
			}
		})
	}
}

// TestPunchEmptyCodeFailsClosed 本端拿不到协助码时既签不出也验不过，绝不能退化成放行。
func TestPunchEmptyCodeFailsClosed(t *testing.T) {
	pkt := &P2PTestPacket{SessionID: "ses_punch_1", Random: "r0"}
	SignPunchPacket(pkt, "", true)
	if pkt.MAC != "" {
		t.Error("空码不应产生 MAC")
	}
	signed := newSignedPunch(testPunchCode, true)
	if VerifyPunchPacket(signed, "", true) {
		t.Error("本端无码时必须一律拒绝")
	}
	if VerifyPunchPacket(nil, testPunchCode, true) {
		t.Error("nil 包必须拒绝")
	}
}

// TestPunchMACVariesPerPacket random 进 MAC，否则同一会话的所有包 MAC 相同，
// 抓到一个就能无限重放。
func TestPunchMACVariesPerPacket(t *testing.T) {
	a := &P2PTestPacket{SessionID: "ses_punch_1", Random: "r0"}
	b := &P2PTestPacket{SessionID: "ses_punch_1", Random: "r1"}
	SignPunchPacket(a, testPunchCode, true)
	SignPunchPacket(b, testPunchCode, true)
	if a.MAC == b.MAC {
		t.Error("不同 random 应产生不同 MAC")
	}
}

// TestPunchKeyIsDomainSeparated 打洞密钥不得等于工具通道会话密钥，否则打洞包会变成
// 针对会话密钥的预言机。
func TestPunchKeyIsDomainSeparated(t *testing.T) {
	punchKey := derivePunchKey(testPunchCode)
	sessionKey := DeriveSessionKey(testPunchCode, "n1", "n2")
	if len(punchKey) != len(sessionKey) {
		t.Fatalf("长度不一致: %d vs %d", len(punchKey), len(sessionKey))
	}
	same := true
	for i := range punchKey {
		if punchKey[i] != sessionKey[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("打洞密钥与会话密钥相同，域分离失效")
	}
}
