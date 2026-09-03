package p2p

import (
	"net"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

func newAuthTestManager(isShare bool, code string) *P2PManager {
	p := &P2PManager{sessionID: "ses_auth_1", isShare: isShare}
	p.SetAuthCode(code)
	return p
}

var punchFrom = &net.UDPAddr{IP: net.ParseIP("203.0.113.9"), Port: 41000}

// TestAcceptPunchRejectsUnsigned 回归核心：sessionID 会被主动喷洒到对端公网 IP 的
// 一批端口上，本来就不是秘密。只比对 sessionID 就放行，等于任何收到过杂散打洞包的
// 第三方回一份同样的 JSON 就能被认成对端。
func TestAcceptPunchRejectsUnsigned(t *testing.T) {
	p := newAuthTestManager(false, "ABCD-2345")
	pkt := &proto.P2PTestPacket{SessionID: p.sessionID, Random: "r0"} // 不带 MAC
	if p.acceptPunch(pkt, punchFrom) {
		t.Fatal("无 MAC 的打洞包被接受了")
	}
}

func TestAcceptPunchAcceptsPeerSigned(t *testing.T) {
	const code = "ABCD-2345"
	help := newAuthTestManager(false, code)

	// share 端签名 -> help 端应接受。
	pkt := &proto.P2PTestPacket{SessionID: help.sessionID, Random: "r0"}
	proto.SignPunchPacket(pkt, code, true)
	if !help.acceptPunch(pkt, punchFrom) {
		t.Fatal("对端合法签名的打洞包应被接受")
	}
}

func TestAcceptPunchRejectsWrongCodeAndReflection(t *testing.T) {
	const code = "ABCD-2345"
	share := newAuthTestManager(true, code)

	wrong := &proto.P2PTestPacket{SessionID: share.sessionID, Random: "r0"}
	proto.SignPunchPacket(wrong, "WXYZ-6789", false)
	if share.acceptPunch(wrong, punchFrom) {
		t.Error("用别的协助码签的包不应被接受")
	}

	// share 自己发出的包被原样打回：方向对不上，不能算对端应答。
	echo := &proto.P2PTestPacket{SessionID: share.sessionID, Random: "r1"}
	proto.SignPunchPacket(echo, code, true)
	if share.acceptPunch(echo, punchFrom) {
		t.Error("反射回来的自己的包不应被接受")
	}
}

// TestAcceptPunchStillChecksSession MAC 校验是附加条件，不能弱化原有的会话比对。
func TestAcceptPunchStillChecksSession(t *testing.T) {
	const code = "ABCD-2345"
	help := newAuthTestManager(false, code)

	other := &proto.P2PTestPacket{SessionID: "ses_other", Random: "r0"}
	proto.SignPunchPacket(other, code, true)
	if help.acceptPunch(other, punchFrom) {
		t.Fatal("别的会话的包不应被接受")
	}
}

// TestNewTestPacketIsSigned 所有发包路径都必须经由 newTestPacket，漏签会让对端
// 一律丢弃、P2P 静默失效。
func TestNewTestPacketIsSigned(t *testing.T) {
	const code = "ABCD-2345"
	share := newAuthTestManager(true, code)
	help := newAuthTestManager(false, code)

	pkt := share.newTestPacket()
	if pkt.SessionID != share.sessionID {
		t.Fatalf("SessionID = %q", pkt.SessionID)
	}
	if pkt.Random == "" {
		t.Fatal("Random 为空")
	}
	if !help.acceptPunch(pkt, punchFrom) {
		t.Fatal("newTestPacket 产出的包对端验不过")
	}
}
