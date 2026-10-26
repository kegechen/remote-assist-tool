package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/quictransport"
	"github.com/remote-assist/tool/internal/relay"
)

func setQuicRaceDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := quicRaceDelay
	quicRaceDelay = d
	t.Cleanup(func() { quicRaceDelay = orig })
}

func setDialTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := dialTimeout
	dialTimeout = d
	t.Cleanup(func() { dialTimeout = orig })
}

// freePort 拿一个 TCP、UDP 都空闲的端口号：测试要让 TCP 与 QUIC 共用同一个 host:port，
// 和生产环境（:8443 同号）一致。
func freePort(t *testing.T) string {
	t.Helper()
	for i := 0; i < 20; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		pc, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		pc.Close()
		return "127.0.0.1:" + strconv.Itoa(port)
	}
	t.Fatal("找不到 TCP/UDP 同时空闲的端口")
	return ""
}

func newCertPair(t *testing.T) (cert, key string) {
	t.Helper()
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := crypto.GenerateSelfSignedCert(cert, key); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// startRelay 起一个 TLS relay，tcpAddr / quicAddr 为空表示不开该传输（TCP 必开，空串时
// 落在随机端口上）。返回停止函数，调用后端口释放。
func startRelay(t *testing.T, tcpAddr, quicAddr, cert, key string) (stop func()) {
	t.Helper()
	if tcpAddr == "" {
		tcpAddr = "127.0.0.1:0"
	}
	srv, err := relay.NewServer(&relay.Config{
		ListenAddr: tcpAddr, QUICListenAddr: quicAddr,
		UseTLS: true, TLSCertFile: cert, TLSKeyFile: key,
		CodeTTL: time.Minute, CodeLength: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- srv.StartWithContextReady(ctx, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("relay 启动失败: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("relay 启动超时")
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// blackholeTCP 在 addr 上接受 TCP 连接但从不回应 TLS 握手——客户端看到的效果与
// 「SYN-ACK 被丢」一致：TCP 拨号一直挂到超时。
func blackholeTCP(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	})
}

func testClient(t *testing.T, addr, transport string) *Client {
	t.Helper()
	c := NewClient(&Config{
		ServerAddr: addr, UseTLS: true, InsecureSkip: true, Transport: transport,
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	})
	t.Cleanup(c.Close)
	return c
}

// registerRoundTrip 在已建立的连接上完成一次注册，确认这条连接确实通到 relay。
func registerRoundTrip(t *testing.T, c *Client) {
	t.Helper()
	if err := c.SendMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "v1.1.0"}); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var reg proto.RegisterResponse
	if msg.Type != proto.MsgRegisterResponse || json.Unmarshal(msg.Payload, &reg) != nil || reg.Code == "" {
		t.Fatalf("注册响应异常: %s %s", msg.Type, msg.Payload)
	}
}

// 本次修复的核心场景：relay 的 TCP 被丢回程（这里用只 Accept 不握手的黑洞模拟），
// UDP 正常。auto 必须在竞速延迟后改走 QUIC 并完整可用，而不是挂到 dialTimeout。
func TestAutoFallsBackToQUICWhenTCPBlackholed(t *testing.T) {
	setQuicRaceDelay(t, 300*time.Millisecond)
	setDialTimeout(t, 10*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	blackholeTCP(t, addr)
	startRelay(t, "", addr, cert, key)

	c := testClient(t, addr, "")
	start := time.Now()
	if err := c.Connect(); err != nil {
		t.Fatalf("TCP 黑洞时 auto 应改走 QUIC，却失败: %v", err)
	}
	elapsed := time.Since(start)
	if c.transport != TransportQUIC {
		t.Fatalf("transport=%q，期望 quic", c.transport)
	}
	if elapsed < 300*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("耗时 %v，期望在竞速延迟（300ms）之后、远早于 dialTimeout 完成", elapsed)
	}
	registerRoundTrip(t, c)
}

// TCP 正常时只走 TCP，QUIC 一路根本不该被拨：不给 relay 和网络平白加一倍握手。
func TestAutoUsesTCPWithoutDialingQUICWhenHealthy(t *testing.T) {
	setQuicRaceDelay(t, 300*time.Millisecond)
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, addr, "", cert, key)

	// 在同一 UDP 端口上挂一个只计数的 QUIC 监听，看客户端有没有去拨它。
	tlsCert, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr(addr, quictransport.TLSConfig(&tls.Config{Certificates: []tls.Certificate{tlsCert}}), quictransport.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var quicDials atomic.Int32
	go func() {
		for {
			qc, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			quicDials.Add(1)
			qc.CloseWithError(0, "")
		}
	}()

	c := testClient(t, addr, "")
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if c.transport != TransportTCP {
		t.Fatalf("transport=%q，TCP 正常时期望 tcp", c.transport)
	}
	registerRoundTrip(t, c)
	time.Sleep(3 * quicRaceDelay)
	if n := quicDials.Load(); n != 0 {
		t.Fatalf("TCP 已成功，QUIC 却被拨了 %d 次", n)
	}
}

// TCP 被明确拒绝（端口没开）时不必等满竞速延迟，立刻换 QUIC。
func TestAutoStartsQUICImmediatelyWhenTCPRefused(t *testing.T) {
	setQuicRaceDelay(t, 10*time.Second)
	setDialTimeout(t, 20*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, "", addr, cert, key) // TCP 在别的端口，addr 上 TCP 拒绝

	c := testClient(t, addr, "")
	start := time.Now()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second || c.transport != TransportQUIC {
		t.Fatalf("耗时 %v transport=%q，期望不等 10s 竞速延迟就走 QUIC", elapsed, c.transport)
	}
}

// 记住上次成功的传输：share 断线重连时先拨 QUIC，不再每次都白等竞速延迟。
func TestReconnectDialsPreviouslySuccessfulTransportFirst(t *testing.T) {
	setQuicRaceDelay(t, time.Second)
	setDialTimeout(t, 10*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	blackholeTCP(t, addr)
	startRelay(t, "", addr, cert, key)

	c := testClient(t, addr, "")
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	start := time.Now()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= quicRaceDelay || c.transport != TransportQUIC {
		t.Fatalf("重连耗时 %v transport=%q，期望直接走上次成功的 QUIC", elapsed, c.transport)
	}
}

// --transport tcp 保持旧行为：TCP 不通就失败，不碰 UDP。
func TestTCPModeNeverFallsBackToQUIC(t *testing.T) {
	setQuicRaceDelay(t, 100*time.Millisecond)
	setDialTimeout(t, time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	blackholeTCP(t, addr)
	startRelay(t, "", addr, cert, key)

	c := testClient(t, addr, TransportTCP)
	if err := c.Connect(); err == nil {
		t.Fatalf("--transport tcp 在 TCP 黑洞下应失败，却连上了（transport=%q）", c.transport)
	}
}

// --transport quic 只走 QUIC，不等 TCP。
func TestQUICModeSkipsTCP(t *testing.T) {
	setQuicRaceDelay(t, 10*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	blackholeTCP(t, addr)
	startRelay(t, "", addr, cert, key)

	c := testClient(t, addr, TransportQUIC)
	start := time.Now()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second || c.transport != TransportQUIC {
		t.Fatalf("耗时 %v transport=%q，期望直接 QUIC", elapsed, c.transport)
	}
	registerRoundTrip(t, c)
}

// TOFU 钉扎对 QUIC 同样生效：TCP 上学到的指纹，换了证书的 QUIC 服务端必须被拒。
// 否则攻击者只要挡掉 TCP、在 UDP 上冒充 relay，就能绕过钉扎。
//
// 回环地址豁免钉扎（见 crypto.isLoopbackAddr），所以这里直接用钉扎键为公网地址的
// tls.Config 调 dialRelay：验证的是 QUIC 拨号确实执行了同一个证书校验回调。
func TestQUICEnforcesPinLearnedOverTCP(t *testing.T) {
	addr := freePort(t)
	store := &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)}
	tlsConfig, err := crypto.NewClientTLSConfig(crypto.ClientTLSOptions{SkipVerify: true, PinAddr: "relay.example:8443", TrustStore: store})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(addr, transport string) error {
		c := NewClient(&Config{ServerAddr: addr, UseTLS: true, InsecureSkip: true, Transport: transport})
		conn, _, err := c.dialRelay(tlsConfig, "")
		if conn != nil {
			conn.Close()
		}
		return err
	}

	certA, keyA := newCertPair(t)
	startRelay(t, addr, addr, certA, keyA)
	if err := dial(addr, TransportTCP); err != nil {
		t.Fatalf("首次 TCP 连接应学到指纹: %v", err)
	}
	if _, ok := store.Lookup("relay.example:8443"); !ok {
		t.Fatal("TCP 连接后应已记录指纹")
	}
	if err := dial(addr, TransportQUIC); err != nil {
		t.Fatalf("同一证书经 QUIC 应匹配钉扎: %v", err)
	}

	// 钉扎键是 relay.example:8443，与实际端口无关；B 用新端口，免得等 A 的 UDP 端口释放。
	addrB := freePort(t)
	certB, keyB := newCertPair(t)
	startRelay(t, "", addrB, certB, keyB)
	if err := dial(addrB, TransportQUIC); err == nil {
		t.Fatal("证书已更换的 QUIC relay 应被 TOFU 钉扎拒绝")
	}
}

func TestValidTransport(t *testing.T) {
	for _, s := range []string{"", "auto", "tcp", "quic"} {
		if !ValidTransport(s) {
			t.Errorf("%q 应合法", s)
		}
	}
	for _, s := range []string{"udp", "QUIC", "both"} {
		if ValidTransport(s) {
			t.Errorf("%q 应非法", s)
		}
	}
}
