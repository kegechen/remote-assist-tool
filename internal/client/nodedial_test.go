package client

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
)

// certFP 返回证书文件里叶子证书的指纹（同节点列表里的 tls_fps）。
func certFP(t *testing.T, certFile, keyFile string) string {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return crypto.CertFingerprint(pair.Certificate[0])
}

// testNode 造一个节点：只有 TCP 地址、只有 QUIC 地址，或两者都有。
func testNode(fps []string, tcp, quic []string) directory.Node {
	return directory.Node{
		NodeCert: directory.NodeCert{Prefix: "K", Name: "test-node", TCP: tcp, QUIC: quic, TLSFPs: fps},
		Status:   directory.StatusActive,
		Weight:   100,
	}
}

// deadAddr 返回一个没人监听的本机地址（TCP、UDP 都空闲，拨过去立刻被拒绝）。
func deadAddr(t *testing.T) string {
	t.Helper()
	return freePort(t)
}

func TestTargetTransport(t *testing.T) {
	both := nodeTarget{Addr: "a:1", TCP: true, QUIC: true}
	tcpOnly := nodeTarget{Addr: "a:1", TCP: true}
	quicOnly := nodeTarget{Addr: "a:1", QUIC: true}
	for _, tc := range []struct {
		name   string
		user   string
		target nodeTarget
		want   string
		ok     bool
	}{
		{"auto 两种都有就竞速", "", both, TransportAuto, true},
		{"显式 auto", TransportAuto, both, TransportAuto, true},
		{"auto 只有 TCP 就只拨 TCP", "", tcpOnly, TransportTCP, true},
		{"auto 只有 QUIC 就只拨 QUIC", TransportAuto, quicOnly, TransportQUIC, true},
		{"用户要 tcp、地址有 TCP", TransportTCP, both, TransportTCP, true},
		{"用户要 quic、地址有 QUIC", TransportQUIC, both, TransportQUIC, true},
		{"用户要 tcp、地址只有 QUIC", TransportTCP, quicOnly, "", false},
		{"用户要 quic、地址只有 TCP", TransportQUIC, tcpOnly, "", false},
		{"两种都没声明", "", nodeTarget{Addr: "a:1"}, "", false},
		{"非法传输名", "udp", both, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := targetTransport(tc.user, tc.target)
			if tc.ok != (err == nil) || got != tc.want {
				t.Fatalf("targetTransport(%q, %+v) = %q, %v; want %q ok=%v", tc.user, tc.target, got, err, tc.want, tc.ok)
			}
		})
	}
}

// 连节点：TCP 地址，指纹对得上，建出来的连接能交给 Client 用。
func TestDialNodeTCPAndAdopt(t *testing.T) {
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, addr, "", cert, key)
	n := testNode([]string{certFP(t, cert, key)}, []string{addr}, nil)

	r, err := dialNode(context.Background(), n, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Addr != addr || r.Transport != TransportTCP || r.Mode != TransportTCP || len(r.Failed) != 0 {
		t.Fatalf("result = addr %q transport %q mode %q failed %v", r.Addr, r.Transport, r.Mode, r.Failed)
	}

	// 建连时 Client 配置里的地址是别的（比如默认节点）：接管后要改成实际连上的地址。
	// 之前的连接已经断了（重新选节点时就是这样），接管后要回到已连接状态，心跳也有新的一代可绑。
	c := testClient(t, "default.example.test:8443", "")
	c.Close()
	c.adoptNode(r)
	if c.config.ServerAddr != addr || c.transport != TransportTCP || c.IsClosed() {
		t.Fatalf("adopt 后 ServerAddr=%q transport=%q closed=%v", c.config.ServerAddr, c.transport, c.IsClosed())
	}
	c.mu.Lock()
	hb := c.hbStop
	c.mu.Unlock()
	if hb == nil {
		t.Fatal("adopt 后没有新的连接代次，心跳循环起不来")
	}
	registerRoundTrip(t, c)
}

// 只有一种传输的地址没有竞速，不能打「TCP 不可达，已改用 QUIC」；两种都有、TCP 输了才打。
func TestAdoptNodeTransportLog(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		want       bool
	}{
		{"只有 QUIC", TransportQUIC, false},
		{"竞速后改走 QUIC", TransportAuto, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			c := testClient(t, "default.example.test:8443", "")
			a, b := net.Pipe()
			t.Cleanup(func() { b.Close() })
			c.adoptNode(&nodeConn{Conn: a, Addr: "192.0.2.1:8443", Transport: TransportQUIC, Mode: tc.mode, FPs: []string{"x"}})
			if got := strings.Contains(buf.String(), "TCP 不可达"); got != tc.want {
				t.Fatalf("日志 = %q，期望打「TCP 不可达」= %v", buf.String(), tc.want)
			}
		})
	}
}

// 接管后断线重连回同一个地址，照样按指纹认身份。Client 的配置是 PKI 校验（不跳过证书校验），
// 自签证书只有按指纹比对才连得上：重连若退回原来的校验方式就会失败。（严格比对不写指纹表，
// 由 crypto 包的 TestNewClientTLSConfigAllowedFPs 覆盖：这里的地址是回环，TOFU 本来就不写。）
func TestAdoptedNodeReconnects(t *testing.T) {
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, addr, "", cert, key)
	n := testNode([]string{certFP(t, cert, key)}, []string{addr}, nil)
	r, err := dialNode(context.Background(), n, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c := testClient(t, "default.example.test:8443", "")
	c.config.InsecureSkip = false
	c.adoptNode(r)
	c.Close()

	if err := c.Connect(); err != nil {
		t.Fatalf("接管后重连失败: %v", err)
	}
	registerRoundTrip(t, c)
}

// adoptedPipe 让 c 接管一条假连接，节点信息由参数给出：用来造出「重连时证书或传输与列表不符」的场景。
func adoptedPipe(t *testing.T, c *Client, addr, mode string, fps []string) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { b.Close() })
	c.adoptNode(&nodeConn{Conn: a, Addr: addr, Transport: TransportTCP, Mode: mode, FPs: fps})
}

// 重连时地址上换成了列表之外的证书：拒绝，且不因为指纹表里没有这个地址就当成首次连接放行。
func TestAdoptedNodeReconnectRejectsWrongCert(t *testing.T) {
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, addr, "", cert, key)
	c := testClient(t, "default.example.test:8443", "")
	adoptedPipe(t, c, addr, TransportTCP, []string{strings.Repeat("e", 64)})
	c.Close()

	err := c.Connect()
	if err == nil {
		t.Fatal("证书不在列表里，重连却成功了")
	}
	var mm *CertMismatchError
	if !errors.As(err, &mm) || mm.FP != certFP(t, cert, key) {
		t.Fatalf("err = %v，应是带出示指纹的 *CertMismatchError", err)
	}
}

// 重连只用这个地址声明过的传输：地址只声明了 TCP，就不去拨 QUIC（哪怕那里正好有 relay）。
func TestAdoptedNodeReconnectKeepsAddressTransport(t *testing.T) {
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, "", addr, cert, key) // 只有 QUIC 在 addr 上，TCP 在别的端口
	c := testClient(t, "default.example.test:8443", "")
	adoptedPipe(t, c, addr, TransportTCP, []string{certFP(t, cert, key)})
	c.Close()

	if err := c.Connect(); err == nil {
		t.Fatalf("地址只声明了 TCP，重连却连上了（transport=%q）", c.transport)
	}
}

// 地址只在 QUIC 里列出：只走 QUIC，不碰 TCP。
func TestDialNodeQUICOnlyAddress(t *testing.T) {
	setQuicRaceDelay(t, 10*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	blackholeTCP(t, addr) // 若误拨 TCP 会挂住
	startRelay(t, "", addr, cert, key)
	n := testNode([]string{certFP(t, cert, key)}, nil, []string{addr})

	start := time.Now()
	r, err := dialNode(context.Background(), n, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Conn.Close() })
	if r.Transport != TransportQUIC || time.Since(start) > 3*time.Second {
		t.Fatalf("transport=%q 耗时 %v，期望直接走 QUIC", r.Transport, time.Since(start))
	}
}

// 地址同时有 TCP 与 QUIC：照旧竞速，TCP 被丢回程时改走 QUIC。
func TestDialNodeAutoRacesWithinAddress(t *testing.T) {
	setQuicRaceDelay(t, 200*time.Millisecond)
	setDialTimeout(t, 10*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	blackholeTCP(t, addr)
	startRelay(t, "", addr, cert, key)
	n := testNode([]string{certFP(t, cert, key)}, []string{addr}, []string{addr})

	r, err := dialNode(context.Background(), n, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Conn.Close() })
	if r.Transport != TransportQUIC {
		t.Fatalf("transport=%q，TCP 黑洞时应改走 QUIC", r.Transport)
	}
	if r.OtherErr != nil && !tcpUnreachable(r.OtherErr) {
		t.Logf("tcpErr = %v", r.OtherErr)
	}
}

// 证书指纹不在列表里：不是 TOFU，一次都不放过。TCP、QUIC、auto 三种拨法都要识别成「证书不符」，
// 而不是笼统的「连不上」——QUIC 把握手错误转成文本，errors.Is 认不出，要靠回调记下来。
func TestDialNodeWrongFingerprint(t *testing.T) {
	cert, key := newCertPair(t)
	wrongCert, wrongKey := newCertPair(t)
	wrong := certFP(t, wrongCert, wrongKey)
	for _, tc := range []struct {
		name      string
		tcp, quic bool
		user      string
	}{
		{"tcp", true, false, ""},
		{"quic", false, true, ""},
		{"auto", true, true, ""},
		{"用户指定 tcp", true, true, TransportTCP},
		{"用户指定 quic", true, true, TransportQUIC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := freePort(t)
			startRelay(t, addr, addr, cert, key)
			var tcpAddrs, quicAddrs []string
			if tc.tcp {
				tcpAddrs = []string{addr}
			}
			if tc.quic {
				quicAddrs = []string{addr}
			}
			n := testNode([]string{wrong}, tcpAddrs, quicAddrs)
			r, err := dialNode(context.Background(), n, "", tc.user, "")
			if err == nil {
				r.Conn.Close()
				t.Fatal("证书不在列表里却连上了")
			}
			var mm *CertMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("err = %v，应能识别成 *CertMismatchError", err)
			}
			if mm.Addr != addr || mm.FP != certFP(t, cert, key) {
				t.Fatalf("mismatch = %+v，应带着地址和出示的指纹", mm)
			}
		})
	}
}

// 本机专用地址排最前；它连不上时记下来（调用方据此清缓存），再试列表里的地址。
func TestDialNodeLocalAddressFailsThenList(t *testing.T) {
	local := deadAddr(t)
	listAddr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, listAddr, "", cert, key)
	n := testNode([]string{certFP(t, cert, key)}, []string{listAddr}, nil)

	r, err := dialNode(context.Background(), n, local, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Conn.Close() })
	if r.Addr != listAddr {
		t.Fatalf("Addr = %q，本机专用地址连不上后应改用列表地址 %q", r.Addr, listAddr)
	}
	if len(r.Failed) != 1 || r.Failed[0].Addr != local || r.Failed[0].Err == nil {
		t.Fatalf("Failed = %+v，应记下本机专用地址连不上", r.Failed)
	}
}

func TestDialNodeLocalAddressUsedFirst(t *testing.T) {
	local, listAddr := freePort(t), freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, local, "", cert, key)
	startRelay(t, listAddr, "", cert, key)
	n := testNode([]string{certFP(t, cert, key)}, []string{listAddr}, nil)

	r, err := dialNode(context.Background(), n, local, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Conn.Close() })
	if r.Addr != local || len(r.Failed) != 0 {
		t.Fatalf("Addr = %q Failed = %v，本机专用地址能用就该用它", r.Addr, r.Failed)
	}
}

// 本机专用地址上是另一个证书（比如那个地址被换成了别的机器）：当作这个地址连不上，原因是证书不符，
// 继续试列表地址；调用方清缓存时能说清原因。
func TestDialNodeLocalAddressCertMismatchContinues(t *testing.T) {
	local, listAddr := freePort(t), freePort(t)
	certA, keyA := newCertPair(t)
	certB, keyB := newCertPair(t)
	startRelay(t, local, "", certB, keyB) // 本机专用地址上是别人的证书
	startRelay(t, listAddr, "", certA, keyA)
	n := testNode([]string{certFP(t, certA, keyA)}, []string{listAddr}, nil)

	r, err := dialNode(context.Background(), n, local, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Conn.Close() })
	if r.Addr != listAddr || len(r.Failed) != 1 {
		t.Fatalf("Addr = %q Failed = %+v", r.Addr, r.Failed)
	}
	var mm *CertMismatchError
	if !errors.As(r.Failed[0].Err, &mm) || mm.Addr != local {
		t.Fatalf("Failed[0].Err = %v，应是本机专用地址的证书不符", r.Failed[0].Err)
	}
}

// 全部地址都连不上：错误里每个地址一条；调用方能取到逐地址的失败原因。
func TestDialNodeAllAddressesFail(t *testing.T) {
	setDialTimeout(t, 2*time.Second) // 没人监听的 QUIC 地址要等到超时
	local, a, b := deadAddr(t), deadAddr(t), deadAddr(t)
	n := testNode([]string{strings.Repeat("a", 64)}, []string{a}, []string{b})

	r, err := dialNode(context.Background(), n, local, "", "")
	if err == nil {
		r.Conn.Close()
		t.Fatal("没有任何地址可连，却成功了")
	}
	var de *nodeDialError
	if !errors.As(err, &de) || len(de.Failures) != 3 {
		t.Fatalf("err = %v，应是带三个地址失败原因的 *nodeDialError", err)
	}
	for i, want := range []string{local, a, b} {
		if de.Failures[i].Addr != want {
			t.Errorf("Failures[%d].Addr = %q, want %q（顺序：本机专用、TCP 列表、QUIC 列表）", i, de.Failures[i].Addr, want)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文本里没有地址 %s: %v", want, err)
		}
	}
}

// 没有指纹的节点不能拨：拨过去等于没有身份校验。
func TestDialNodeWithoutFingerprintsFailsClosed(t *testing.T) {
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, addr, "", cert, key)
	for _, fps := range [][]string{nil, {}} {
		n := testNode(fps, []string{addr}, nil)
		r, err := dialNode(context.Background(), n, "", "", "")
		if err == nil {
			r.Conn.Close()
			t.Fatalf("fps=%v 却连上了", fps)
		}
		// 根本不该去拨：没有指纹时 TLS 配置会退回别的校验方式，能不能连上全看证书恰好怎样。
		var de *nodeDialError
		if errors.As(err, &de) {
			t.Fatalf("fps=%v 时去拨了地址: %v", fps, err)
		}
	}
}

// 用户用 --transport 限定了传输：地址不提供它就不拨，不能悄悄换另一种传输。
func TestDialNodeHonorsUserTransport(t *testing.T) {
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, "", addr, cert, key)
	n := testNode([]string{certFP(t, cert, key)}, nil, []string{addr}) // 只有 QUIC

	r, err := dialNode(context.Background(), n, "", TransportTCP, "")
	if err == nil {
		r.Conn.Close()
		t.Fatal("--transport tcp 遇到只有 QUIC 的地址应失败")
	}
	// 是用户限定了传输，不是地址连不上：不能记成失败（调用方会据此清本机专用地址）。
	var de *nodeDialError
	if !errors.As(err, &de) || len(de.Skipped) != 1 || len(de.Failures) != 0 {
		t.Fatalf("err = %v，应只记为跳过", err)
	}
}

// 跳过一个地址后接着试下一个：只有 TCP 的地址（排在前面）被 --transport quic 跳过，QUIC 地址照样能连。
func TestDialNodeSkipsThenDialsNext(t *testing.T) {
	tcpAddr, quicAddr := freePort(t), freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, tcpAddr, "", cert, key)
	startRelay(t, "", quicAddr, cert, key)
	n := testNode([]string{certFP(t, cert, key)}, []string{tcpAddr}, []string{quicAddr})

	r, err := dialNode(context.Background(), n, "", TransportQUIC, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Conn.Close() })
	if r.Addr != quicAddr || r.Transport != TransportQUIC || len(r.Failed) != 0 {
		t.Fatalf("Addr = %q transport = %q Failed = %+v，应跳过只有 TCP 的地址、用 QUIC 连上", r.Addr, r.Transport, r.Failed)
	}
}

// 被 relay 封禁：这个节点的其余地址同样会被拒（同一台 relay），马上结束；错误仍能认出是封禁。
func TestDialNodeBlockedStopsAndIsBlocked(t *testing.T) {
	blockedAddr := blockedTCP(t, freePort(t))
	other := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, other, "", cert, key)
	n := testNode([]string{certFP(t, cert, key)}, []string{blockedAddr, other}, nil)

	r, err := dialNode(context.Background(), n, "", TransportTCP, "")
	if err == nil {
		r.Conn.Close()
		t.Fatal("被封禁却连上了")
	}
	if !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("err = %v，应能认出封禁", err)
	}
}

// 调用方取消（别的节点已经赢了）：拨号马上结束，不等 dialTimeout。
func TestDialNodeCancel(t *testing.T) {
	setDialTimeout(t, 30*time.Second)
	addr := freePort(t)
	blackholeTCP(t, addr)
	n := testNode([]string{strings.Repeat("b", 64)}, []string{addr}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	r, err := dialNode(ctx, n, "", TransportTCP, "")
	if err == nil {
		r.Conn.Close()
		t.Fatal("黑洞地址不该连上")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消后 %v 才返回", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v，应能认出是调用方取消", err)
	}
}

// 被取消的那次拨号不算地址连不上：本机专用地址若因为别的节点赢了、这边被取消而记成失败，
// 调用方会把一个好好的本机专用地址从缓存里删掉。
func TestDialNodeCancelDoesNotBlameAddress(t *testing.T) {
	setDialTimeout(t, 30*time.Second)
	local := freePort(t)
	blackholeTCP(t, local)
	n := testNode([]string{strings.Repeat("d", 64)}, []string{deadAddr(t)}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	r, err := dialNode(ctx, n, local, TransportTCP, "")
	if err == nil {
		r.Conn.Close()
		t.Fatal("取消后不该连上")
	}
	var de *nodeDialError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v，应是 *nodeDialError", err)
	}
	if len(de.Failures) != 0 {
		t.Fatalf("Failures = %+v，被取消的拨号不该算地址连不上", de.Failures)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v，应能认出是调用方取消", err)
	}
}

// udpCounter 在 addr 上收 UDP 包并计数：用来确认没有拨 QUIC（拨了至少会发一个 Initial 包）。
func udpCounter(t *testing.T, addr string) *atomic.Int32 {
	t.Helper()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var n atomic.Int32
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
			n.Add(1)
		}
	}()
	return &n
}

// 调用方取消后不再起另一路：首选一路因取消失败时，不能接着拿已取消的 ctx 去拨另一路。
// quic-go 拿到已取消的 ctx 发不发 Initial 包取决于它内部的时序，不能指望它，这里由竞速自己守住。
func TestDialRelayAddrCanceledDoesNotStartSecond(t *testing.T) {
	setQuicRaceDelay(t, 10*time.Second)
	var mu sync.Mutex
	var kinds []string
	orig := dialOneFunc
	t.Cleanup(func() { dialOneFunc = orig })
	ctx, cancel := context.WithCancel(context.Background())
	dialOneFunc = func(c context.Context, addr, kind string, cfg *tls.Config) (net.Conn, error) {
		mu.Lock()
		kinds = append(kinds, kind)
		mu.Unlock()
		if kind == TransportTCP {
			cancel()
			<-c.Done()
			return nil, c.Err()
		}
		return nil, errors.New("不该拨这一路")
	}
	if _, _, _, err := dialRelayAddr(ctx, "relay.example.test:8443", TransportAuto, &tls.Config{}, ""); err == nil {
		t.Fatal("取消了却返回成功")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 1 || kinds[0] != TransportTCP {
		t.Fatalf("拨了 %v，取消后只该有首选的 tcp 一路", kinds)
	}
}

// 端到端看同一件事：取消后 QUIC 一路不往地址上发包。自动选节点时输掉的节点都是这样被取消的。
func TestDialRelayAddrCanceledDoesNotStartQUIC(t *testing.T) {
	setDialTimeout(t, 30*time.Second)
	setQuicRaceDelay(t, 10*time.Second)
	addr := freePort(t)
	blackholeTCP(t, addr)
	udp := udpCounter(t, addr)
	tlsConfig, err := crypto.NewClientTLSConfig(crypto.ClientTLSOptions{AllowedFPs: []string{strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}

	// 对照：不取消时 QUIC 一路确实会发包，计数有效。
	probe, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if conn, err := dialOne(probe, addr, TransportQUIC, tlsConfig); err == nil {
		conn.Close()
	}
	stop()
	if udp.Load() == 0 {
		t.Fatal("对照组没收到 UDP 包，计数无效")
	}
	udp.Store(0)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	conn, _, _, err := dialRelayAddr(ctx, addr, TransportAuto, tlsConfig, "")
	if err == nil {
		conn.Close()
		t.Fatal("黑洞地址不该连上")
	}
	time.Sleep(300 * time.Millisecond)
	if n := udp.Load(); n != 0 {
		t.Fatalf("取消后还拨了 QUIC（收到 %d 个 UDP 包）", n)
	}
}

// 取消后这个节点后面的地址也不再拨（不发包），被取消的那次和后面的地址都不算连不上：
// 调用方据 Failures 清理本机专用地址，记进去就会删掉一个好好的地址。
func TestDialNodeCancelStopsDialing(t *testing.T) {
	setDialTimeout(t, 30*time.Second)
	first, next := freePort(t), freePort(t)
	blackholeTCP(t, first)
	udp := udpCounter(t, next)
	n := testNode([]string{strings.Repeat("b", 64)}, []string{first}, []string{next})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	r, err := dialNode(ctx, n, "", "", "")
	if err == nil {
		r.Conn.Close()
		t.Fatal("取消后不该连上")
	}
	var de *nodeDialError
	if !errors.As(err, &de) || de.Canceled == nil || len(de.Failures) != 0 {
		t.Fatalf("取消后的错误 = %#v，应只记 Canceled、没有失败的地址", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := udp.Load(); n != 0 {
		t.Fatalf("取消后还拨了后面的地址（收到 %d 个 UDP 包）", n)
	}
}

// 取消后这个节点后面的地址一个都不拨：数拨号次数，不靠 quic-go 发不发包。
func TestDialNodeCancelSkipsRemainingAddrs(t *testing.T) {
	const first, next = "relay-a.example.test:8443", "relay-b.example.test:8443"
	var mu sync.Mutex
	var dialed []string
	orig := dialOneFunc
	t.Cleanup(func() { dialOneFunc = orig })
	ctx, cancel := context.WithCancel(context.Background())
	dialOneFunc = func(c context.Context, addr, kind string, cfg *tls.Config) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr+"/"+kind)
		mu.Unlock()
		if addr == first {
			cancel()
			<-c.Done()
			return nil, c.Err()
		}
		return nil, errors.New("不该拨这个地址")
	}
	n := testNode([]string{strings.Repeat("b", 64)}, []string{first}, []string{next})
	if r, err := dialNode(ctx, n, "", "", ""); err == nil {
		r.Conn.Close()
		t.Fatal("取消后不该连上")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{first + "/" + TransportTCP}; !reflect.DeepEqual(dialed, want) {
		t.Fatalf("拨了 %v，取消后只该有第一个地址", dialed)
	}
}

// 取消恰好赶上 QUIC 起跑的定时器：首选一路还没返回（握手卡在不看 ctx 的地方），定时器先到，也不能拿已取消的 ctx 起另一路。
func TestDialRelayAddrTimerDoesNotStartSecondAfterCancel(t *testing.T) {
	setQuicRaceDelay(t, 50*time.Millisecond)
	var mu sync.Mutex
	var kinds []string
	orig := dialOneFunc
	t.Cleanup(func() { dialOneFunc = orig })
	ctx, cancel := context.WithCancel(context.Background())
	dialOneFunc = func(c context.Context, addr, kind string, cfg *tls.Config) (net.Conn, error) {
		mu.Lock()
		kinds = append(kinds, kind)
		mu.Unlock()
		if kind == TransportTCP {
			cancel()
			time.Sleep(300 * time.Millisecond)
			return nil, errors.New("tcp 握手失败")
		}
		return nil, errors.New("不该拨这一路")
	}
	if _, _, _, err := dialRelayAddr(ctx, "relay.example.test:8443", TransportAuto, &tls.Config{}, ""); err == nil {
		t.Fatal("取消了却返回成功")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 1 || kinds[0] != TransportTCP {
		t.Fatalf("拨了 %v，取消后定时器到点也只该有首选的 tcp 一路", kinds)
	}
}

// auto 模式下一路出示了列表之外的证书、另一路连上了：连上的那路经过了校验，但被劫持的那路要告诉用户。
func TestDialStrictWarnsWhenOtherTransportMismatched(t *testing.T) {
	addr := freePort(t)
	badCert, badKey := newCertPair(t)
	startRelay(t, addr, "", badCert, badKey) // TCP 上的证书不在列表里
	cert, key := newCertPair(t)
	startRelay(t, "", addr, cert, key)
	logs := captureLog(t)

	conn, transport, _, err := dialStrict(context.Background(), addr, TransportAuto, []string{certFP(t, cert, key)}, "")
	if err != nil {
		t.Fatalf("QUIC 那路的证书在列表里，应该连上: %v", err)
	}
	conn.Close()
	if transport != TransportQUIC {
		t.Fatalf("transport = %s，应为 quic", transport)
	}
	if got := logs.String(); !strings.Contains(got, "可能被劫持") || !strings.Contains(got, certFP(t, badCert, badKey)[:16]) {
		t.Fatalf("没有警告 TCP 那路的证书与列表不符:\n%s", got)
	}
}

// 证书不符的错误本身带着地址，汇总时不再重复一遍。
func TestNodeDialErrorNoDuplicateAddr(t *testing.T) {
	const addr = "relay-w.example.test:8443"
	de := &nodeDialError{Failures: []addrFailure{
		{Addr: addr, Err: &CertMismatchError{Addr: addr, FP: strings.Repeat("c", 64)}},
		{Addr: "relay-x.example.test:8443", Err: errors.New("connection refused")},
	}}
	got := de.Error()
	if n := strings.Count(got, addr); n != 1 {
		t.Fatalf("地址出现了 %d 次:\n%s", n, got)
	}
	if !strings.Contains(got, "relay-x.example.test:8443: connection refused") {
		t.Fatalf("其他错误应带上地址:\n%s", got)
	}
}

// TCP 被封禁、QUIC 那边出示了列表之外的证书：封禁优先。认不出封禁的话会接着拨这个节点的其余地址，
// 它们在同一台 relay 上，只会再被拒一遍。
func TestDialNodeBlockedWinsOverCertMismatch(t *testing.T) {
	addr := freePort(t)
	blockedTCP(t, addr)
	cert, key := newCertPair(t)
	startRelay(t, "", addr, cert, key) // QUIC 上的证书不在列表里
	n := testNode([]string{strings.Repeat("f", 64)}, []string{addr}, []string{addr})

	r, err := dialNode(context.Background(), n, "", "", TransportQUIC)
	if err == nil {
		r.Conn.Close()
		t.Fatal("被封禁却连上了")
	}
	if !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("err = %v，应能认出封禁", err)
	}
}

// 接管时 Client 还连着（没 Close）：旧连接要关掉，旧的心跳要叫停，否则旧码一直挂在旧 relay 上，
// 旧心跳还会往新连接上重复发。
func TestAdoptNodeReplacesLiveConnection(t *testing.T) {
	c := testClient(t, "default.example.test:8443", "")
	a1, b1 := net.Pipe()
	t.Cleanup(func() { b1.Close() })
	c.adoptNode(&nodeConn{Conn: a1, Addr: "192.0.2.1:8443", Transport: TransportTCP, Mode: TransportTCP, FPs: []string{"x"}})
	c.mu.Lock()
	hb1 := c.hbStop
	c.mu.Unlock()

	a2, b2 := net.Pipe()
	t.Cleanup(func() { b2.Close() })
	c.adoptNode(&nodeConn{Conn: a2, Addr: "192.0.2.2:8443", Transport: TransportTCP, Mode: TransportTCP, FPs: []string{"x"}})
	select {
	case <-hb1:
	default:
		t.Fatal("上一代心跳没有叫停")
	}
	_ = b1.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b1.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("旧连接没有关掉: %v", err)
	}
}

// 节点模式下没有指纹（不该出现）：不拨，不能退回 PKI 或别的校验方式。
func TestAdoptedNodeWithoutFingerprintsDoesNotDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conn.Close()
		}
	}()
	c := testClient(t, "default.example.test:8443", "")
	adoptedPipe(t, c, ln.Addr().String(), TransportTCP, nil)
	c.Close()

	if err := c.Connect(); err == nil {
		t.Fatal("没有指纹却连上了")
	}
	time.Sleep(200 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("没有指纹时去拨了地址（%d 次）", n)
	}
}

// 接管的是另一个地址：上一次的传输是别的地址上的，不能据此打「已恢复走 TCP」；同一地址上才算恢复。
func TestAdoptNodeTransportLogPerAddress(t *testing.T) {
	for _, tc := range []struct {
		name, from string
		want       bool
	}{
		{"换了地址", "192.0.2.1:8443", false},
		{"同一地址", "192.0.2.2:8443", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			c := testClient(t, tc.from, "")
			c.transport = TransportQUIC
			a, b := net.Pipe()
			t.Cleanup(func() { b.Close() })
			c.adoptNode(&nodeConn{Conn: a, Addr: "192.0.2.2:8443", Transport: TransportTCP, Mode: TransportAuto, FPs: []string{"x"}})
			if got := strings.Contains(buf.String(), "已恢复走 TCP"); got != tc.want {
				t.Fatalf("日志 = %q，期望打「已恢复走 TCP」= %v", buf.String(), tc.want)
			}
		})
	}
}

// 记录失败要拿得到 net 层面的原因，供「已从缓存删除」提示写原因。
func TestDialNodeFailureKeepsCause(t *testing.T) {
	addr := deadAddr(t)
	n := testNode([]string{strings.Repeat("c", 64)}, []string{addr}, nil)
	_, err := dialNode(context.Background(), n, "", TransportTCP, "")
	var op *net.OpError
	if !errors.As(err, &op) {
		t.Fatalf("err = %v，应保留底层的 *net.OpError", err)
	}
}
