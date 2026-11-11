package p2p

import (
	"net"
	"testing"
	"time"
)

// replyOnlyFrom 只应答来自 allowed 的请求：模拟「这块网卡的 UDP 出不去」（如全隧道 VPN
// 不放行到公网 STUN 的 UDP），另一块网卡正常。
func replyOnlyFrom(allowed string) func(int, *STUNMessage, *net.UDPAddr) []byte {
	return func(_ int, req *STUNMessage, from *net.UDPAddr) []byte {
		if from.IP.String() != allowed {
			return nil
		}
		return bindingReply(req, from, nil)
	}
}

func hostOf(t *testing.T, addr string) string {
	t.Helper()
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("bad addr %q: %v", addr, err)
	}
	return h
}

func startNICManager(t *testing.T, stun string, ips []string) (*P2PManager, *captureRelay, time.Duration) {
	t.Helper()
	mgr := NewP2PManager(P2PModeAuto, stun, "")
	mgr.fallbackSTUN = nil // 测试不碰公网
	mgr.raceOpts = fastRace
	mgr.localIPs = func() []net.IP {
		var out []net.IP
		for _, s := range ips {
			out = append(out, net.ParseIP(s).To4())
		}
		return out
	}
	relay := &captureRelay{}
	mgr.SetRelayConn(relay)
	start := time.Now()
	if _, err := mgr.Start("sess-nic", false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return mgr, relay, time.Since(start)
}

// 默认首选网卡（全隧道 VPN 接管了默认路由时就是 VPN 网卡）上 STUN 全部无应答、另一块
// 网卡能通：应改绑到能通的那块并上报它拿到的公网地址，而不是带着空公网地址退回中转。
func TestManagerSwitchesToNICWhereSTUNAnswers(t *testing.T) {
	good := nonLoopbackIPv4(t)
	bad := "127.0.0.1"
	live := fakeSTUN(t, good, replyOnlyFrom(good), nil)

	mgr, relay, _ := startNICManager(t, live, []string{bad, good})

	a := relay.advertise(t)
	if a.PublicAddr == "" || a.STUNServer != live {
		t.Fatalf("advertise = %+v, want public address via %s", a, live)
	}
	if got := hostOf(t, a.PrivateAddr); got != good {
		t.Fatalf("private addr host = %s, want %s (the NIC where STUN answered)", got, good)
	}
	if got := mgr.localConn.LocalAddr().(*net.UDPAddr).IP.String(); got != good {
		t.Fatalf("socket bound to %s, want %s", got, good)
	}
	if a.NATType == "" {
		t.Fatal("nat_type empty")
	}
}

// 首选网卡能通时保持原行为：用首选，且不等其它网卡的竞速跑满时限。
func TestManagerKeepsPreferredNICWhenItAnswers(t *testing.T) {
	good := nonLoopbackIPv4(t)
	live := fakeSTUN(t, good, replyOnlyFrom(good), nil)

	mgr, relay, elapsed := startNICManager(t, live, []string{good, "127.0.0.1"})

	if elapsed > time.Second {
		t.Fatalf("Start took %v; must not wait for the silent NIC's deadline (%v)", elapsed, fastRace.Deadline)
	}
	a := relay.advertise(t)
	if a.STUNServer != live || hostOf(t, a.PrivateAddr) != good {
		t.Fatalf("advertise = %+v, want preferred NIC %s via %s", a, good, live)
	}
	if got := mgr.localConn.LocalAddr().(*net.UDPAddr).IP.String(); got != good {
		t.Fatalf("socket bound to %s, want %s", got, good)
	}
}

// 所有网卡都不通：保持首选网卡、不上报公网地址（与原来一样回落中转）。
func TestManagerAllNICsSilentKeepsPreferred(t *testing.T) {
	good := nonLoopbackIPv4(t)
	dead := fakeSTUN(t, good, neverReply, nil)

	mgr, relay, _ := startNICManager(t, dead, []string{"127.0.0.1", good})

	a := relay.advertise(t)
	if a.PublicAddr != "" || a.STUNServer != "" {
		t.Fatalf("advertise = %+v, want no public address", a)
	}
	if got := mgr.localConn.LocalAddr().(*net.UDPAddr).IP.String(); got != "127.0.0.1" {
		t.Fatalf("socket bound to %s, want preferred 127.0.0.1", got)
	}
}

// 用户用 --bind-ip 指定时只用那一块，不去探测其它网卡。
func TestManagerBindIPSkipsMultiNICProbe(t *testing.T) {
	good := nonLoopbackIPv4(t)
	live := fakeSTUN(t, good, replyOnlyFrom(good), nil)

	mgr := NewP2PManager(P2PModeAuto, live, "127.0.0.1")
	mgr.fallbackSTUN = nil
	mgr.raceOpts = fastRace
	mgr.localIPs = func() []net.IP {
		t.Error("localIPs must not be consulted when bind IP is set")
		return nil
	}
	relay := &captureRelay{}
	mgr.SetRelayConn(relay)
	if _, err := mgr.Start("sess-bind", false); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if got := mgr.localConn.LocalAddr().(*net.UDPAddr).IP.String(); got != "127.0.0.1" {
		t.Fatalf("socket bound to %s, want user-specified 127.0.0.1", got)
	}
	if a := relay.advertise(t); a.PublicAddr != "" {
		t.Fatalf("advertise = %+v, want no public address (bound NIC cannot reach STUN)", a)
	}
}

// listenOrSkip 确认本机能绑 ip（127.0.0.2 在 macOS 上默认没有配置）。
func listenOrSkip(t *testing.T, ip string) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	c.Close()
}

// 首选网卡发得出去但一直没有应答（不是一发就失败）：备选应答后只再等 preferredGrace，
// 不等首选跑满竞速时限。
func TestManagerSilentPreferredNICSwitchesAfterGrace(t *testing.T) {
	listenOrSkip(t, "127.0.0.2")
	live := fakeSTUN(t, "127.0.0.1", replyOnlyFrom("127.0.0.2"), nil)

	mgr := NewP2PManager(P2PModeAuto, live, "")
	mgr.fallbackSTUN = nil
	mgr.raceOpts = RaceOptions{Deadline: 5 * time.Second, Retransmit: fastRace.Retransmit,
		Grace: fastRace.Grace, ResolveTimeout: fastRace.ResolveTimeout}
	mgr.preferredGrace = 200 * time.Millisecond
	mgr.localIPs = func() []net.IP { return []net.IP{net.IPv4(127, 0, 0, 1).To4(), net.IPv4(127, 0, 0, 2).To4()} }
	relay := &captureRelay{}
	mgr.SetRelayConn(relay)

	start := time.Now()
	if _, err := mgr.Start("sess-silent", false); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Start took %v; should switch after grace, not wait for the %v deadline", elapsed, mgr.raceOpts.Deadline)
	}
	a := relay.advertise(t)
	if a.STUNServer != live || hostOf(t, a.PrivateAddr) != "127.0.0.2" {
		t.Fatalf("advertise = %+v, want switch to 127.0.0.2 via %s", a, live)
	}
}

// Close 与 Start 并发（help 在 Start 之前就把 manager 登记给 teardown）：不能在 Close 之后
// 换上一个没人回收的新 socket。
func TestManagerCloseDuringDiscoveryDoesNotLeakSwitchedSocket(t *testing.T) {
	listenOrSkip(t, "127.0.0.2")
	replied := make(chan struct{}, 1)
	reply := replyOnlyFrom("127.0.0.2")
	live := fakeSTUN(t, "127.0.0.1", func(n int, req *STUNMessage, from *net.UDPAddr) []byte {
		out := reply(n, req, from)
		if out != nil {
			select {
			case replied <- struct{}{}:
			default:
			}
		}
		return out
	}, nil)

	mgr := NewP2PManager(P2PModeAuto, live, "")
	mgr.fallbackSTUN = nil
	mgr.raceOpts = RaceOptions{Deadline: 5 * time.Second, Retransmit: fastRace.Retransmit,
		Grace: fastRace.Grace, ResolveTimeout: fastRace.ResolveTimeout}
	mgr.preferredGrace = 3 * time.Second
	mgr.localIPs = func() []net.IP { return []net.IP{net.IPv4(127, 0, 0, 1).To4(), net.IPv4(127, 0, 0, 2).To4()} }
	mgr.SetRelayConn(&captureRelay{})

	gotReply := make(chan bool, 1) // 结果交回主 goroutine 断言，不在子 goroutine 里调 t.Error
	go func() {
		// 等备选确实拿到应答、其竞速也收完 Grace 交出结果，此时首选正处在 3s 宽限期里。
		// Close 关掉首选 socket → 首选竞速返回 ErrClosed → 循环选中备选 → 改绑前须发现已 Close。
		ok := true
		select {
		case <-replied:
		case <-time.After(3 * time.Second):
			ok = false
		}
		time.Sleep(mgr.raceOpts.Grace + 150*time.Millisecond)
		mgr.Close()
		gotReply <- ok
	}()
	if _, err := mgr.Start("sess-close", false); err != nil {
		t.Fatal(err)
	}
	if !<-gotReply {
		t.Fatal("alternate NIC never got a STUN answer; test did not exercise the close-during-grace path")
	}
	mgr.tunnelMu.Lock()
	conn := mgr.localConn
	mgr.tunnelMu.Unlock()
	if got := conn.LocalAddr().(*net.UDPAddr).IP.String(); got != "127.0.0.1" {
		t.Fatalf("localConn switched to %s after Close; that socket would never be closed", got)
	}
	if _, err := conn.WriteToUDP([]byte("x"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}); err == nil {
		t.Fatal("localConn still open after Close")
	}
}
