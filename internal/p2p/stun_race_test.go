package p2p

import (
	"errors"
	"net"
	"reflect"
	"strings"
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

// TID 对不上的应答必须丢弃（96 位随机 TID 是认领应答的依据）。
func TestRaceDiscoverRejectsWrongTID(t *testing.T) {
	wrongTID := fakeSTUN(t, "127.0.0.1", func(_ int, req *STUNMessage, from *net.UDPAddr) []byte {
		bad := *req
		bad.TID[0] ^= 0xff
		return bindingReply(&bad, from, &net.UDPAddr{IP: net.ParseIP("203.0.113.9"), Port: 1})
	}, nil)
	if probes, err := RaceDiscover(clientSocket(t), []string{wrongTID}, fastRace); err == nil {
		t.Fatalf("wrong-TID response accepted: %+v", probes)
	}
}

// 多 IP 的 STUN 主机可能从另一个地址回包：TID 对得上就接受（旧实现同样不校验来源）。
func TestRaceDiscoverAcceptsOtherSourceWithMatchingTID(t *testing.T) {
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	srv := fakeSTUN(t, "127.0.0.1", alwaysReply, other)
	probes, err := RaceDiscover(clientSocket(t), []string{srv}, fastRace)
	if err != nil || probes[0].Server != srv {
		t.Fatalf("probes=%+v err=%v", probes, err)
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
	slow := fakeSTUN(t, "127.0.0.1", func(_ int, req *STUNMessage, from *net.UDPAddr) []byte {
		time.Sleep(40 * time.Millisecond)
		return bindingReply(req, from, nil)
	}, nil)
	opts := fastRace
	opts.Grace = 400 * time.Millisecond
	probes, err := RaceDiscover(clientSocket(t), []string{slow, fast}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 || probes[0].Server != fast || probes[1].Server != slow {
		t.Fatalf("probes = %+v", probes)
	}
}

// 两个名字指向同一地址只发一份请求、只算一个应答。
func TestRaceDiscoverDedupesSameAddress(t *testing.T) {
	var count atomic.Int32
	srv := fakeSTUN(t, "127.0.0.1", func(_ int, req *STUNMessage, from *net.UDPAddr) []byte {
		count.Add(1)
		return bindingReply(req, from, nil)
	}, nil)
	_, port, _ := net.SplitHostPort(srv)
	probes, err := RaceDiscover(clientSocket(t), []string{srv, "[::ffff:127.0.0.1]:" + port}, fastRace)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 || count.Load() != 1 {
		t.Fatalf("probes=%d requests=%d, want 1/1", len(probes), count.Load())
	}
}

// 竞速中 socket 被关闭（P2P manager 被 Close）时立刻返回，不空转到 deadline。
func TestRaceDiscoverReturnsWhenConnClosed(t *testing.T) {
	dead := fakeSTUN(t, "127.0.0.1", neverReply, nil)
	c := clientSocket(t)
	time.AfterFunc(100*time.Millisecond, func() { c.Close() })
	start := time.Now()
	if _, err := RaceDiscover(c, []string{dead}, DefaultRaceOptions); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("err = %v, want net.ErrClosed", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("returned after %v", time.Since(start))
	}
}

// 全部失败时错误里列出每个候选的结局，现场能区分解析失败与无应答。
func TestRaceDiscoverFailureSummary(t *testing.T) {
	dead := fakeSTUN(t, "127.0.0.1", neverReply, nil)
	_, err := RaceDiscover(clientSocket(t), []string{dead, "no-such-host.invalid:3478", "bad-entry"}, fastRace)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{dead + "=no-reply", "no-such-host.invalid:3478=unresolved", "bad-entry=unresolved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestMergeSTUNCandidates(t *testing.T) {
	got := MergeSTUNCandidates("relay.example:3478",
		[]string{"23.95.78.14:3478", " relay.example:3478 ", "", "stun.chat.bilibili.com:3478"},
		[]string{"stun.chat.bilibili.com:3478", "a:1", "b:1", "c:1"})
	want := []string{"relay.example:3478", "23.95.78.14:3478", "stun.chat.bilibili.com:3478", "a:1", "b:1", "c:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// relay 下发的列表客户端自己也设防：不合格的丢弃（回环、未指定、IPv6、带怪字符），最多
// 采用 4 项，内置兜底始终保留。
func TestMergeSTUNCandidatesDistrustsAdvertised(t *testing.T) {
	advertised := []string{"127.0.0.1:3478", "0.0.0.0:3478", "169.254.1.1:3478", "[2001:db8::1]:3478",
		"evil\x1b[31m:1", "x:0", "a.example:1", "b.example:1", "c.example:1", "d.example:1", "e.example:1", "f.example:1"}
	got := MergeSTUNCandidates("relay.example:3478", advertised, FallbackSTUNServers)
	want := append([]string{"relay.example:3478", "a.example:1", "b.example:1", "c.example:1", "d.example:1"}, FallbackSTUNServers...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
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

// unsendableAddr 返回一个从 127.0.0.1 上的 socket 发不出去的地址（本机无路由），找不到就跳过。
func unsendableAddr(t *testing.T) string {
	t.Helper()
	c := clientSocket(t)
	for _, a := range []string{"192.0.2.1:9", "0.0.0.0:9", "127.0.0.1:0"} {
		u, _ := net.ResolveUDPAddr("udp4", a)
		if _, err := c.WriteToUDP([]byte{0}, u); err != nil {
			return a
		}
	}
	t.Skip("no destination makes sendto fail on this host")
	return ""
}

// 首包就发不出去的目标不会有应答：全部如此时立即失败，不等满 deadline，诊断里标 send-failed。
func TestRaceDiscoverAllSendFailedReturnsImmediately(t *testing.T) {
	bad := unsendableAddr(t)
	start := time.Now()
	_, err := RaceDiscover(clientSocket(t), []string{bad}, DefaultRaceOptions)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), bad+"=send-failed") {
		t.Fatalf("error %q lacks send-failed", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("returned after %v; nothing can answer, must not wait for the deadline", elapsed)
	}
}

// 发不出去的目标不挡住其它候选：另一台照常应答。
func TestRaceDiscoverSendFailedDoesNotBlockOthers(t *testing.T) {
	live := fakeSTUN(t, "127.0.0.1", alwaysReply, nil)
	probes, err := RaceDiscover(clientSocket(t), []string{unsendableAddr(t), live}, fastRace)
	if err != nil {
		t.Fatal(err)
	}
	if probes[0].Server != live {
		t.Fatalf("winner = %s, want %s", probes[0].Server, live)
	}
}

// 被去重合并的名字出现在失败诊断里，名字取候选列表里靠前的那个，与解析先后无关。
func TestRaceDiscoverSummaryNamesAliases(t *testing.T) {
	dead := fakeSTUN(t, "127.0.0.1", neverReply, nil)
	_, port, _ := net.SplitHostPort(dead)
	alias := "[::ffff:127.0.0.1]:" + port
	_, err := RaceDiscover(clientSocket(t), []string{dead, alias}, fastRace)
	if err == nil {
		t.Fatal("expected error")
	}
	if want := dead + "=no-reply(also " + alias + ")"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q lacks %q", err, want)
	}
}

// relay 启动校验与客户端过滤共用的规则。
func TestCheckSTUNServer(t *testing.T) {
	for _, ok := range []string{"23.95.78.14:3478", "10.0.0.5:3478", "stun.chat.bilibili.com:3478", "stun.l.google.com:19302"} {
		if err := CheckSTUNServer(ok); err != nil {
			t.Errorf("CheckSTUNServer(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"no-port", ":3478", "host:0", "host:70000", "host:+80", "host:abc", "a b:3478", "a_b:3478",
		"127.0.0.1:3478", "0.0.0.0:3478", "0.1.2.3:3478", "169.254.1.1:3478", "224.0.0.1:3478", "255.255.255.255:3478",
		"[2001:db8::1]:3478", "[::ffff:1.2.3.4]:3478", "localhost:3478", "LOCALHOST.:3478", "x.localhost:3478", "evil\x1b[31m:1"} {
		if err := CheckSTUNServer(bad); err == nil {
			t.Errorf("CheckSTUNServer(%q) accepted", bad)
		}
	}
}
