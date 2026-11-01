package p2p

import (
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSTUN 起一个本地 UDP 服务；reply 决定对每个请求回什么（nil 表示不回）。
// 回包默认从同一个 socket 发出，replyFrom 非 nil 时改从它发（模拟伪造来源）。
func fakeSTUN(t *testing.T, ip string, reply func(n int, req *STUNMessage, from *net.UDPAddr) []byte, replyFrom *net.UDPConn) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Skipf("listen %s: %v", ip, err)
	}
	t.Cleanup(func() { conn.Close() })
	var count int32
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req, err := UnpackSTUN(buf[:n])
			if err != nil {
				continue
			}
			out := reply(int(atomic.AddInt32(&count, 1)), req, from)
			if out == nil {
				continue
			}
			sender := conn
			if replyFrom != nil {
				sender = replyFrom
			}
			_, _ = sender.WriteToUDP(out, from)
		}
	}()
	return conn.LocalAddr().String()
}

// bindingReply 按 RFC 5389 回一个 Binding Response，映射地址为 mapped（nil 时取请求来源）。
func bindingReply(req *STUNMessage, from, mapped *net.UDPAddr) []byte {
	if mapped == nil {
		mapped = from
	}
	resp := &STUNMessage{Type: STUNBindingResponse, Magic: req.Magic, TID: req.TID}
	resp.AddXorMappedAddress(mapped, req.Magic, req.TID)
	return resp.Pack()
}

func alwaysReply(_ int, req *STUNMessage, from *net.UDPAddr) []byte {
	return bindingReply(req, from, nil)
}
func neverReply(int, *STUNMessage, *net.UDPAddr) []byte { return nil }

func clientSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

var fastRace = RaceOptions{Deadline: 1500 * time.Millisecond, Retransmit: []time.Duration{300 * time.Millisecond, 700 * time.Millisecond},
	Grace: 100 * time.Millisecond, ResolveTimeout: 500 * time.Millisecond}

// 不通的服务器排在前面也不拖慢：旧实现对单台要等满 3+5+8=16 秒才试下一台。
func TestRaceDiscoverDoesNotWaitForDeadServer(t *testing.T) {
	dead := fakeSTUN(t, "127.0.0.1", neverReply, nil)
	live := fakeSTUN(t, "127.0.0.1", alwaysReply, nil)
	c := clientSocket(t)

	start := time.Now()
	probes, err := RaceDiscover(c, []string{dead, live}, DefaultRaceOptions)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if probes[0].Server != live {
		t.Fatalf("winner = %s, want %s", probes[0].Server, live)
	}
	if got, want := probes[0].Mapped.Port, c.LocalAddr().(*net.UDPAddr).Port; got != want {
		t.Fatalf("mapped port = %d, want %d（必须是主 socket 自己的映射）", got, want)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("race took %v; a dead first server must not delay the answer", elapsed)
	}
}

func TestRaceDiscoverAllDeadReturnsErrorByDeadline(t *testing.T) {
	a := fakeSTUN(t, "127.0.0.1", neverReply, nil)
	b := fakeSTUN(t, "127.0.0.1", neverReply, nil)
	start := time.Now()
	_, err := RaceDiscover(clientSocket(t), []string{a, b}, fastRace)
	if err == nil {
		t.Fatal("expected error when no server answers")
	}
	if elapsed := time.Since(start); elapsed < fastRace.Deadline || elapsed > fastRace.Deadline+500*time.Millisecond {
		t.Fatalf("returned after %v, want about %v", elapsed, fastRace.Deadline)
	}
}

// 丢包：第一次请求不回，重传后回——必须仍然成功，且重传沿用同一 TID（RFC 5389 §7.2.1）。
func TestRaceDiscoverRetransmitsSameTID(t *testing.T) {
	var firstTID [12]byte
	var sameTID atomic.Bool
	srv := fakeSTUN(t, "127.0.0.1", func(n int, req *STUNMessage, from *net.UDPAddr) []byte {
		if n == 1 {
			firstTID = req.TID
			return nil
		}
		sameTID.Store(req.TID == firstTID)
		return bindingReply(req, from, nil)
	}, nil)
	probes, err := RaceDiscover(clientSocket(t), []string{srv}, fastRace)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 || !sameTID.Load() {
		t.Fatalf("probes=%v sameTID=%v", probes, sameTID.Load())
	}
}

// 伪造来源（换一个端口回包）与 TID 对不上的响应都必须丢弃。
func TestRaceDiscoverRejectsSpoofedAndWrongTID(t *testing.T) {
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	spoofed := fakeSTUN(t, "127.0.0.1", alwaysReply, other)
	wrongTID := fakeSTUN(t, "127.0.0.1", func(_ int, req *STUNMessage, from *net.UDPAddr) []byte {
		bad := *req
		bad.TID[0] ^= 0xff
		return bindingReply(&bad, from, &net.UDPAddr{IP: net.ParseIP("203.0.113.9"), Port: 1})
	}, nil)
	if probes, err := RaceDiscover(clientSocket(t), []string{spoofed, wrongTID}, fastRace); err == nil {
		t.Fatalf("spoofed / wrong-TID responses accepted: %+v", probes)
	}
}

// 解析不了的域名不能卡住竞速。
func TestRaceDiscoverUnresolvableHostDoesNotBlock(t *testing.T) {
	live := fakeSTUN(t, "127.0.0.1", alwaysReply, nil)
	start := time.Now()
	probes, err := RaceDiscover(clientSocket(t), []string{"no-such-host.invalid:3478", "bad-entry", live}, fastRace)
	if err != nil {
		t.Fatal(err)
	}
	if probes[0].Server != live || time.Since(start) > time.Second {
		t.Fatalf("probes=%v elapsed=%v", probes, time.Since(start))
	}
}

// 第一个应答之后再等 Grace，收集更多应答供 NAT 判定；第一个仍排在最前。
func TestRaceDiscoverCollectsAnswersWithinGrace(t *testing.T) {
	fast := fakeSTUN(t, "127.0.0.1", alwaysReply, nil)
	slow := fakeSTUN(t, "127.0.0.2", func(_ int, req *STUNMessage, from *net.UDPAddr) []byte {
		time.Sleep(40 * time.Millisecond)
		return bindingReply(req, from, nil)
	}, nil)
	probes, err := RaceDiscover(clientSocket(t), []string{slow, fast}, fastRace)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 || probes[0].Server != fast || probes[1].Server != slow {
		t.Fatalf("probes = %+v", probes)
	}
}

func TestMergeSTUNCandidates(t *testing.T) {
	got := MergeSTUNCandidates("relay.example:3478",
		[]string{"23.95.78.14:3478", " relay.example:3478 ", "", "stun.chat.bilibili.com:3478"},
		[]string{"stun.chat.bilibili.com:3478", "a:1", "b:1", "c:1", "d:1", "e:1"})
	want := []string{"relay.example:3478", "23.95.78.14:3478", "stun.chat.bilibili.com:3478", "a:1", "b:1", "c:1", "d:1", "e:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := MergeSTUNCandidates("", nil, []string{"a:1", "b:1", "c:1", "d:1", "e:1", "f:1", "g:1", "h:1", "i:1"}); len(got) != maxSTUNCandidates {
		t.Fatalf("len = %d, want cap %d", len(got), maxSTUNCandidates)
	}
}

func TestNATFromProbes(t *testing.T) {
	u := func(ip string, port int) *net.UDPAddr { return &net.UDPAddr{IP: net.ParseIP(ip), Port: port} }
	local := net.ParseIP("192.168.1.5")
	cases := []struct {
		name   string
		probes []STUNProbe
		want   string
	}{
		{"open", []STUNProbe{{Server: "1.1.1.1:3478", ServerIP: "1.1.1.1", Mapped: u("192.168.1.5", 5000)}}, NATOpen},
		{"cone", []STUNProbe{{Server: "a", ServerIP: "1.1.1.1", Mapped: u("36.7.69.115", 5000)}, {Server: "b", ServerIP: "2.2.2.2", Mapped: u("36.7.69.115", 5000)}}, NATCone},
		{"symmetric", []STUNProbe{{Server: "a", ServerIP: "1.1.1.1", Mapped: u("36.7.69.115", 5000)}, {Server: "b", ServerIP: "2.2.2.2", Mapped: u("36.7.69.115", 5001)}}, NATSymmetric},
		{"same server IP is not enough", []STUNProbe{{Server: "a", ServerIP: "1.1.1.1", Mapped: u("36.7.69.115", 5000)}, {Server: "b", ServerIP: "1.1.1.1", Mapped: u("36.7.69.115", 5001)}}, ""},
		{"single", []STUNProbe{{Server: "a", ServerIP: "1.1.1.1", Mapped: u("36.7.69.115", 5000)}}, ""},
		// 内网 STUN 看到的是内网映射，与公网映射不可比（实测会被误判成 symmetric）。
		{"different mapped IPs are not comparable", []STUNProbe{{Server: "a", ServerIP: "1.1.1.1", Mapped: u("36.7.69.115", 5000)}, {Server: "b", ServerIP: "10.0.0.2", Mapped: u("10.0.0.5", 49230)}}, ""},
	}
	for _, c := range cases {
		if got := NATFromProbes(local, c.probes); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
