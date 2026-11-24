package relay

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/relay/blocklist"
)

// dialLineFrom 从指定本地回环地址发起连接（Windows/Linux 都能绑定 127.0.0.x）。
func dialLineFrom(t *testing.T, src, addr string) *lineClient {
	t.Helper()
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}}
	c, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("dial from %s: %v", src, err)
	}
	return wrapLine(c)
}

// pair 在 shareSrc 注册 share、helpSrc 用协助码加入，返回两端连接（调用方负责关闭）。
func pair(t *testing.T, addr, shareSrc, helpSrc string) (*lineClient, *lineClient) {
	t.Helper()
	share, help, _ := pairWithCode(t, addr, shareSrc, helpSrc)
	return share, help
}

func pairWithCode(t *testing.T, addr, shareSrc, helpSrc string) (*lineClient, *lineClient, string) {
	t.Helper()
	share := dialLineFrom(t, shareSrc, addr)
	share.send(t, proto.MsgRegisterRequest, proto.RegisterRequest{ClientID: "cid-" + shareSrc, Version: "v1.1.0", Host: "u@pc kylin arm64"})
	m := share.recv(t)
	if m == nil || m.Type != proto.MsgRegisterResponse {
		t.Fatal("注册失败")
	}
	var reg proto.RegisterResponse
	if err := json.Unmarshal(m.Payload, &reg); err != nil {
		t.Fatal(err)
	}
	help := dialLineFrom(t, helpSrc, addr)
	help.send(t, proto.MsgJoinRequest, proto.JoinRequest{Code: reg.Code, Version: "v1.1.0"})
	if m := help.recv(t); m == nil || m.Type != proto.MsgJoinResponse || !strings.Contains(string(m.Payload), `"success":true`) {
		t.Fatalf("加入失败: %+v", m)
	}
	return share, help, reg.Code
}

// rejoin 用同一协助码从 helpSrc 重新加入。
func rejoin(t *testing.T, addr, helpSrc, code string) *lineClient {
	t.Helper()
	help := dialLineFrom(t, helpSrc, addr)
	help.send(t, proto.MsgJoinRequest, proto.JoinRequest{Code: code, Version: "v1.1.0"})
	if m := help.recv(t); m == nil || m.Type != proto.MsgJoinResponse || !strings.Contains(string(m.Payload), `"success":true`) {
		t.Fatalf("重新加入失败: %s", m.Payload)
	}
	return help
}

func sendGarbageFrom(t *testing.T, src, addr string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}}
		c, err := d.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, '\n'})
		readUntilClosed(t, c)
		c.Close()
	}
}

// 第四轮 R1：同一 IP 注册 share 再用自己的协助码 join（自配对）不授予豁免，判定也不写已配对。
func TestBlocklistSelfPairingNotExempt(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, r4Config(bl))
	share, help := pair(t, addr, "127.0.0.1", "127.0.0.1")
	defer share.c.Close()
	defer help.c.Close()
	time.Sleep(r4VerifyWait)
	sendGarbageFrom(t, "127.0.0.1", addr, 20)
	time.Sleep(100 * time.Millisecond)
	if !bl.IsBanned("127.0.0.1") {
		t.Fatal("自配对白嫖了 7 天豁免：握手垃圾第 20 次仍未被封")
	}
	if d, _ := bl.Detail("127.0.0.1"); strings.Contains(d.Verdict, "成功加入") {
		t.Fatalf("自配对不应显示为已配对: %q", d.Verdict)
	}
	_ = blocklist.KindTLSGarbage
}

func wrapLine(c net.Conn) *lineClient {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &lineClient{c: c, sc: sc}
}

// 测试里把"配对持续多久才算成功加入"调到 200ms。
const r4VerifyWait = 600 * time.Millisecond

func r4Config(bl *blocklist.List) *Config {
	return &Config{UseTLS: true, Blocklist: bl, BlocklistVerifyAfter: 200 * time.Millisecond}
}

// 第四轮 R1：不同出口的两端配对持续到阈值后，help 所在 IP 获得豁免、判定显示已配对。
func TestBlocklistSustainedPairingGrantsExemption(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, r4Config(bl))
	share, help := pair(t, addr, "127.0.0.2", "127.0.0.1")
	defer share.c.Close()
	defer help.c.Close()
	time.Sleep(r4VerifyWait)
	sendGarbageFrom(t, "127.0.0.1", addr, 30)
	time.Sleep(100 * time.Millisecond)
	if bl.IsBanned("127.0.0.1") {
		t.Fatal("持续配对的协助端 IP 不应因握手垃圾被封")
	}
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		if d, _ := bl.Detail(ip); !strings.Contains(d.Verdict, "成功加入") {
			t.Fatalf("%s 判定应显示已配对: %q", ip, d.Verdict)
		}
	}
}

// 第四轮 R1：配对未持续到阈值就断开，不授予豁免。
func TestBlocklistShortPairingNotExempt(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, r4Config(bl))
	share, help := pair(t, addr, "127.0.0.2", "127.0.0.1")
	help.c.Close()
	share.c.Close()
	time.Sleep(r4VerifyWait)
	sendGarbageFrom(t, "127.0.0.1", addr, 20)
	time.Sleep(100 * time.Millisecond)
	if !bl.IsBanned("127.0.0.1") {
		t.Fatal("配对立即断开也拿到了豁免")
	}
}

// 最终轮 B2：同一会话只保留一个待验证计时器；help 反复加入/断开不会累积计时器，share 断开后清零。
func TestVerifyTimersDoNotAccumulate(t *testing.T) {
	bl := newTestBlocklist(t)
	cfg := r4Config(bl)
	cfg.BlocklistVerifyAfter = time.Hour
	srv, addr := startBlocklistRelay(t, cfg)
	share, help, code := pairWithCode(t, addr, "127.0.0.2", "127.0.0.1")
	if n := srv.pendingVerifyCount(); n != 1 {
		t.Fatalf("配对后应有 1 个待验证计时器，got %d", n)
	}
	for i := 0; i < 15; i++ { // 低于 per-IP join 突发上限 20
		help.c.Close()
		waitFor(t, func() bool { return srv.pendingVerifyCount() == 0 }, "help 断开后待验证计时器应被停掉")
		help = rejoin(t, addr, "127.0.0.1", code)
		if n := srv.pendingVerifyCount(); n > 1 {
			t.Fatalf("第 %d 次重新加入后待验证计时器累积到 %d 个", i, n)
		}
	}
	help.c.Close()
	share.c.Close()
	waitFor(t, func() bool { return srv.pendingVerifyCount() == 0 }, "两端断开后待验证计时器应清零")
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
