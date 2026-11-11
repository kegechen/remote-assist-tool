package p2p

import (
	"net"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// 打洞失败后经 relay:3478 转发的 UDP 中继不是 P2P 直连：隧道要能如实报出走的是哪条路，
// 用户提示与上报都据此区分，不能一律写成「P2P 直连」。
func TestTunnelPathLabel(t *testing.T) {
	conn := clientSocket(t)
	peer := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 40000}
	relay := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 3478}

	direct := NewUDPTunnel(conn, peer)
	defer direct.Close()
	if direct.Path() != proto.P2PPathDirect || direct.Describe() != "P2P 直连" {
		t.Errorf("direct tunnel: path=%q describe=%q", direct.Path(), direct.Describe())
	}

	viaRelay := NewUDPRelayTunnel(conn, relay, "sess")
	defer viaRelay.Close()
	if viaRelay.Path() != proto.P2PPathUDPRelay || viaRelay.Describe() != "UDP 中继（经 203.0.113.9:3478）" {
		t.Errorf("relay tunnel: path=%q describe=%q", viaRelay.Path(), viaRelay.Describe())
	}
}

// p2p_connected 上报带上实际路径，供 relay 区分直连与 UDP 中继（UDP 中继走的不是本 relay
// 的 3478 时，relay 自己看不到这股流量，只能靠上报）。master 的 relay 目前不解码该消息，
// 字段对它透明；旧 relay 同样忽略。
func TestP2PConnectedReportsPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		viaRelay bool
		want     string
	}{
		{"direct", false, proto.P2PPathDirect},
		{"udp relay", true, proto.P2PPathUDPRelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stun := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 3478}
			peer := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 40000}
			mgr := NewP2PManager(P2PModeAuto, "", "")
			relay := &captureRelay{}
			mgr.SetRelayConn(relay)
			mgr.sessionID = "sess-path"
			mgr.localConn = clientSocket(t)
			mgr.stunAddr = stun
			mgr.resultCh = make(chan P2PResult, 2)

			from := peer
			if tc.viaRelay {
				from = stun
			}
			mgr.onP2PConnected(from)
			res := <-mgr.resultCh
			if res.Tunnel == nil {
				t.Fatal("no tunnel delivered")
			}
			defer res.Tunnel.Close()

			relay.mu.Lock()
			defer relay.mu.Unlock()
			var got *proto.P2PConnected
			for _, m := range relay.msgs {
				if m.Type == proto.MsgP2PConnected {
					var pc proto.P2PConnected
					if err := proto.DecodePayload(m, &pc); err != nil {
						t.Fatal(err)
					}
					got = &pc
				}
			}
			if got == nil {
				t.Fatal("no p2p_connected sent")
			}
			if got.SessionID != "sess-path" || got.Path != tc.want {
				t.Fatalf("p2p_connected = %+v, want session sess-path path %q", *got, tc.want)
			}
		})
	}
}
