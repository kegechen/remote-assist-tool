package p2p

import (
	"net"
	"testing"
)

// relayPeerCount 返回某个 relay 会话当前已登记的对端槽位数。
func (s *STUNServer) relayPeerCount(sessionID string) int {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	sess, ok := s.relaySessions[sessionID]
	if !ok {
		return 0
	}
	return sess.count
}

// TestRelayRejectsUnauthorizedSource 校验器拒绝的来源不得占用 relay 槽位。
//
// 这是「先到先得」那条路的封堵点：sessionID 会被主动喷洒到对端公网 IP 的一批端口上，
// 并非秘密，光凭它就放行等于任何第三方抢先发一个包就能当中间人。
func TestRelayRejectsUnauthorizedSource(t *testing.T) {
	const sessionID = "ses_source_check"
	allowed := net.ParseIP("203.0.113.10")

	s, err := NewSTUNServerWithValidator("127.0.0.1:0", func(_ string, srcIP net.IP) bool {
		return srcIP != nil && srcIP.Equal(allowed)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	pkt := buildRelayDatagramForID(sessionID, 16)

	// 第三方来源：既不得创建会话状态，也不得占槽。
	s.handleRelayPacket(pkt, &net.UDPAddr{IP: net.ParseIP("192.0.2.66"), Port: 40000})
	if got := s.relaySessionCount(); got != 0 {
		t.Fatalf("未授权来源不得创建 relay 状态，实际=%d", got)
	}

	// 合法来源：正常登记。
	s.handleRelayPacket(pkt, &net.UDPAddr{IP: allowed, Port: 40001})
	if got := s.relaySessionCount(); got != 1 {
		t.Fatalf("合法来源应创建 relay 状态，实际=%d", got)
	}
	if got := s.relayPeerCount(sessionID); got != 1 {
		t.Fatalf("合法来源应占 1 个槽位，实际=%d", got)
	}

	// 会话已存在后第三方再来，仍然不得挤进空着的第二个槽。
	s.handleRelayPacket(pkt, &net.UDPAddr{IP: net.ParseIP("192.0.2.77"), Port: 40002})
	if got := s.relayPeerCount(sessionID); got != 1 {
		t.Fatalf("未授权来源挤进了空槽，槽位数=%d", got)
	}
}

// TestRelayAllowsPortChangeFromAuthorizedSource 对称 NAT 换端口必须仍然可用：
// 45f72ff 的按 IP 重匹配是 UDP relay 回退的前提，收紧来源校验不能把它打回原形。
func TestRelayAllowsPortChangeFromAuthorizedSource(t *testing.T) {
	const sessionID = "ses_port_change"
	peerA := net.ParseIP("203.0.113.10")
	peerB := net.ParseIP("198.51.100.20")

	s, err := NewSTUNServerWithValidator("127.0.0.1:0", func(_ string, srcIP net.IP) bool {
		return srcIP != nil && (srcIP.Equal(peerA) || srcIP.Equal(peerB))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	pkt := buildRelayDatagramForID(sessionID, 16)
	s.handleRelayPacket(pkt, &net.UDPAddr{IP: peerA, Port: 40001})
	s.handleRelayPacket(pkt, &net.UDPAddr{IP: peerB, Port: 50001})
	if got := s.relayPeerCount(sessionID); got != 2 {
		t.Fatalf("两端应各占一槽，实际=%d", got)
	}

	// peerA 换了外部端口（对称 NAT 常态）：应更新已有槽位而不是被拒。
	s.handleRelayPacket(pkt, &net.UDPAddr{IP: peerA, Port: 40999})
	if got := s.relayPeerCount(sessionID); got != 2 {
		t.Fatalf("换端口不应新增槽位，实际=%d", got)
	}
	s.relayMu.Lock()
	gotPort := s.relaySessions[sessionID].peers[0].Port
	s.relayMu.Unlock()
	if gotPort != 40999 {
		t.Fatalf("换端口后槽位端口应更新为 40999，实际=%d", gotPort)
	}
}
