package p2p

import (
	"net"
	"testing"
	"time"
)

func udp(s string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		panic(err)
	}
	return a
}

// 只有 relay 判定「同出口 IP」才走只打私网的 LAN 快速通道；私网同 /16 只加快私网打洞，
// 不能跳过公网打洞——跨地域办公网复用 10.x 段时私网地址互不可达（实测苏州 10.11.115.x
// 与合肥 10.11.24.x），跳过后连公网打洞、端口预测、UDP 中继都没机会。
func TestPlanHolePunch(t *testing.T) {
	cases := []struct {
		name         string
		sameEgress   bool
		local, peer  *net.UDPAddr
		wantLANOnly  bool
		wantInterval time.Duration
	}{
		{"同出口", true, udp("192.168.1.2:1000"), udp("192.168.1.3:2000"), true, lanPunchInterval},
		{"同出口、私网段不同", true, udp("192.168.1.2:1000"), udp("10.0.0.3:2000"), true, lanPunchInterval},
		{"不同出口、私网同 /16", false, udp("10.11.24.148:1000"), udp("10.11.115.104:2000"), false, lanPunchInterval},
		{"不同出口、私网不同段", false, udp("192.168.1.2:1000"), udp("10.0.0.3:2000"), false, privatePunchInterval},
		{"缺本地地址", false, nil, udp("10.0.0.3:2000"), false, privatePunchInterval},
		{"缺对端私网地址", false, udp("10.0.0.2:1000"), nil, false, privatePunchInterval},
	}
	for _, c := range cases {
		got := planHolePunch(c.sameEgress, c.local, c.peer)
		if got.lanOnly != c.wantLANOnly || got.privateInterval != c.wantInterval {
			t.Errorf("%s: plan = %+v, want lanOnly=%v interval=%v", c.name, got, c.wantLANOnly, c.wantInterval)
		}
	}
}

// backgroundRetry 在另一个 goroutine 里调 startHolePunching，可能看到 HandlePeerAddrReady
// 已写入的 peerInfo、却还没看到 punchPlan（零值，间隔为 0）。time.NewTicker(0) 会 panic、
// 整个进程崩掉；零值策略必须退回低频私网打洞。
func TestStartHolePunchingZeroPlanDoesNotPanic(t *testing.T) {
	mgr := NewP2PManager(P2PModeAuto, "", "")
	mgr.localConn = newTestUDPConn(t)
	mgr.resultCh = make(chan P2PResult, 2)
	peer := newTestUDPConn(t)
	mgr.peerInfo = &PeerInfo{PrivateAddr: peer.LocalAddr().(*net.UDPAddr)}

	mgr.startHolePunching()
	time.Sleep(3 * privatePunchInterval)
	mgr.Close()
}
