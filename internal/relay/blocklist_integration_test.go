package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/relay/blocklist"
)

// startBlocklistRelay 在 127.0.0.1 随机端口上启动真实 relay（可选 TLS），返回监听地址。
func startBlocklistRelay(t *testing.T, cfg *Config) (*Server, string) {
	t.Helper()
	cfg.ListenAddr = "127.0.0.1:0"
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = time.Hour
	}
	if cfg.UseTLS {
		dir := t.TempDir()
		cfg.TLSCertFile = filepath.Join(dir, "server.crt")
		cfg.TLSKeyFile = filepath.Join(dir, "server.key")
		if err := crypto.GenerateSelfSignedCert(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := NewServer(cfg)
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
		t.Fatalf("relay start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("relay not ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("relay did not stop")
		}
	})
	return srv, srv.Addr().String()
}

func newTestBlocklist(t *testing.T) *blocklist.List {
	t.Helper()
	l, err := blocklist.Open(blocklist.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// readUntilClosed 读到对端关闭，返回收到的字节数。
func readUntilClosed(t *testing.T, c net.Conn) int {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := io.Copy(io.Discard, c)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("等待对端关闭超时")
	}
	return int(n)
}

// TestBlocklistScannerBannedAtAccept 扫描器（往 TLS 端口发 HTTP 明文）累计 20 次握手失败后被封，
// 之后的连接在 accept 处被拒：不握手；只有 TLS 客户端会收到一条 access_denied alert，
// 其他连接收不到任何字节。
func TestBlocklistScannerBannedAtAccept(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})

	attempts := 0
	for !bl.IsBanned("127.0.0.1") {
		attempts++
		if attempts > 25 {
			t.Fatal("25 次垃圾握手后仍未被封")
		}
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: relay\r\n\r\n"))
		readUntilClosed(t, c)
		c.Close()
	}
	if attempts != 20 {
		t.Fatalf("应在第 20 次握手失败时封禁，实际第 %d 次", attempts)
	}

	// 被封后：真正的 TLS 客户端也握不了手，不发数据的原始 TCP 等读超时被关，收不到任何字节。
	if c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}); err == nil {
		c.Close()
		t.Fatal("被封 IP 不应能完成 TLS 握手")
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if n := readUntilClosed(t, c); n != 0 {
		t.Fatalf("被封连接收到 %d 字节，应静默关闭", n)
	}
	c.Close()

	e, ok := bl.Detail("127.0.0.1")
	if !ok {
		t.Fatal("没有条目")
	}
	if e.Rejected < 2 || !strings.Contains(e.Verdict, "从未完成握手") {
		t.Fatalf("rejected=%d verdict=%q", e.Rejected, e.Verdict)
	}
	for _, ev := range e.Events {
		if ev.Kind != string(blocklist.KindTLSGarbage) {
			t.Fatalf("意外事件 %+v", ev)
		}
	}
}

type lineClient struct {
	c  net.Conn
	sc *bufio.Scanner
}

func dialLine(t *testing.T, addr string, useTLS bool) *lineClient {
	t.Helper()
	var c net.Conn
	var err error
	if useTLS {
		c, err = tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	} else {
		c, err = net.Dial("tcp", addr)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &lineClient{c: c, sc: sc}
}

func (lc *lineClient) send(t *testing.T, typ proto.MessageType, payload interface{}) {
	t.Helper()
	msg, err := proto.NewMessage(typ, payload)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(msg)
	if _, err := lc.c.Write(append(data, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// recv 读一条消息；连接已关闭返回 nil。
func (lc *lineClient) recv(t *testing.T) *proto.Message {
	t.Helper()
	_ = lc.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !lc.sc.Scan() {
		if ne, ok := lc.sc.Err().(net.Error); ok && ne.Timeout() {
			t.Fatal("等待消息超时（既没收到消息也没被关闭）")
		}
		return nil
	}
	var m proto.Message
	if err := json.Unmarshal(lc.sc.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &m
}

// TestBlocklistClientIDPingPongAndReconnectsNotBanned 复现本次事故的场景：两个同 ClientID 的
// share 进程互踢 + 大量正常重连 + 撞上 create 限流，都不是异常行为，绝不能把出口 IP 封掉。
func TestBlocklistClientIDPingPongAndReconnectsNotBanned(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})

	reg := proto.RegisterRequest{ClientID: "cid-same-machine", Host: "alice@alice-pc kylin arm64"}
	var open []*lineClient
	sawConflict, sawRateLimit := false, false
	for i := 0; i < 40; i++ {
		lc := dialLine(t, addr, true)
		reg.Version = []string{"0.0.6", "v1.1.0"}[i%2]
		lc.send(t, proto.MsgRegisterRequest, reg)
		m := lc.recv(t)
		if m != nil && m.Type == proto.MsgError {
			var e proto.ErrorMessage
			_ = proto.DecodePayload(m, &e)
			switch e.Code {
			case proto.ErrCodeClientIDConflict:
				sawConflict = true
			case "RATE_LIMITED":
				sawRateLimit = true
			}
		}
		open = append(open, lc)
		// 一半连接保持（互踢），一半马上断开再连（正常重连）。
		if i%2 == 1 {
			lc.c.Close()
		}
	}
	for _, lc := range open {
		lc.c.Close()
	}
	if !sawConflict && !sawRateLimit {
		t.Fatal("前置条件：应触发 ClientID 冲突或 create 限流")
	}
	time.Sleep(100 * time.Millisecond)
	if bl.IsBanned("127.0.0.1") {
		t.Fatal("同 ClientID 互踢 / 正常重连 / 限流不得导致封 IP")
	}
	e, ok := bl.Detail("127.0.0.1")
	if !ok {
		t.Fatal("应有观察条目")
	}
	if e.Points != 0 || len(e.Events) != 0 {
		t.Fatalf("不应有任何违规计分: points=%d events=%+v", e.Points, e.Events)
	}
	if e.Connections < 40 || len(e.Clients) == 0 || e.Clients[0].Host != reg.Host {
		t.Fatalf("应记录连接数与客户端身份: conns=%d clients=%+v", e.Connections, e.Clients)
	}
	if strings.Contains(e.Clients[0].ClientFP, "cid-same-machine") {
		t.Fatal("不得暴露原始 ClientID")
	}
	// 只注册、没有完成过配对：如实标注为未认证的自报身份（第三轮 S2）。
	if !strings.Contains(e.Verdict, "仅有未认证的注册") {
		t.Fatalf("判定提示=%q", e.Verdict)
	}
}

// TestBlocklistJoinBruteForceBanned 每条连接 5 次错码被断开，第二条连接后封禁，第三条在 accept 处被拒。
func TestBlocklistJoinBruteForceBanned(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{Blocklist: bl})

	for conn := 0; conn < 2; conn++ {
		lc := dialLine(t, addr, false)
		for i := 0; i < 5; i++ {
			lc.send(t, proto.MsgJoinRequest, proto.JoinRequest{Code: "ZZZZZZZZZ" + string(rune('A'+i))})
			if m := lc.recv(t); m == nil || m.Type != proto.MsgJoinResponse {
				t.Fatalf("第 %d 条连接第 %d 次 join 无响应", conn, i)
			}
		}
		if lc.recv(t) != nil {
			t.Fatal("达到失败上限后连接应被关闭")
		}
		lc.c.Close()
		if conn == 0 && bl.IsBanned("127.0.0.1") {
			t.Fatal("第一条爆破连接不应就封")
		}
	}
	if !bl.IsBanned("127.0.0.1") {
		t.Fatal("两条爆破连接后应封禁")
	}
	e, _ := bl.Detail("127.0.0.1")
	if !strings.Contains(e.Verdict, "爆破") {
		t.Fatalf("判定=%q", e.Verdict)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if n := readUntilClosed(t, c); n != 0 {
		t.Fatalf("被封连接收到 %d 字节", n)
	}
	c.Close()
}

// TestBlocklistProtocolGarbageBanned 明文模式下非协议数据计 proto_decode（2 分）。
func TestBlocklistProtocolGarbageBanned(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{Blocklist: bl})
	for i := 0; i < 10; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("garbage not json\n"))
		readUntilClosed(t, c)
		c.Close()
	}
	if !bl.IsBanned("127.0.0.1") {
		t.Fatal("10 次协议垃圾（20 分）应封禁")
	}
	e, _ := bl.Detail("127.0.0.1")
	if len(e.Events) == 0 || e.Events[0].Kind != string(blocklist.KindProtoDecode) {
		t.Fatalf("事件=%+v", e.Events)
	}
}

// TestBlocklistManualBanClosesLiveConnections 手动封禁立即断开该 IP 的现有连接。
func TestBlocklistManualBanClosesLiveConnections(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{Blocklist: bl})
	lc := dialLine(t, addr, false)
	lc.send(t, proto.MsgRegisterRequest, proto.RegisterRequest{ClientID: "cid-live", Version: "v1.1.0"})
	if m := lc.recv(t); m == nil || m.Type != proto.MsgRegisterResponse {
		t.Fatal("注册失败")
	}
	if _, err := bl.Ban("127.0.0.1", time.Hour, false, "admin", "测试"); err != nil {
		t.Fatal(err)
	}
	if m := lc.recv(t); m != nil {
		t.Fatalf("封禁后应断开现有连接，却收到 %s", m.Type)
	}
	lc.c.Close()
}

// TestBlocklistDisabledWhenSourceIPUntrusted 源 IP 不可信（SNAT/LB）时黑名单整体停用。
func TestBlocklistDisabledWhenSourceIPUntrusted(t *testing.T) {
	bl := newTestBlocklist(t)
	srv, addr := startBlocklistRelay(t, &Config{Blocklist: bl, DisableSourceIPLimits: true})
	if srv.Blocklist() != nil {
		t.Fatal("DisableSourceIPLimits 时 Blocklist() 应返回 nil")
	}
	for i := 0; i < 12; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("garbage\n"))
		readUntilClosed(t, c)
		c.Close()
	}
	if bl.IsBanned("127.0.0.1") {
		t.Fatal("源 IP 不可信时不得自动封禁")
	}
	if _, err := bl.Ban("127.0.0.1", time.Hour, false, "admin", ""); err != nil {
		t.Fatal(err)
	}
	lc := dialLine(t, addr, false)
	lc.send(t, proto.MsgRegisterRequest, proto.RegisterRequest{ClientID: "cid-lb"})
	if m := lc.recv(t); m == nil || m.Type != proto.MsgRegisterResponse {
		t.Fatal("源 IP 不可信时 accept 不应查黑名单")
	}
	lc.c.Close()
}

// manualClock 供黑名单注入的可推进时钟。
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func eventCount(bl *blocklist.List, ip string) int {
	e, ok := bl.Detail(ip)
	if !ok {
		return 0
	}
	return len(e.Events)
}

// waitEvents 等服务端把第 want 条事件记下（连接处理是异步的）。
func waitEvents(t *testing.T, bl *blocklist.List, ip string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for eventCount(bl, ip) < want {
		if time.Now().After(deadline) {
			t.Fatalf("等待第 %d 条事件超时（当前 %d）", want, eventCount(bl, ip))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// B2：证书校验失败的正常客户端（换证书/证书过期/TOFU 指纹不匹配/时钟错）会发 TLS alert
// 并无限重连，绝不能因此被封——这与本次 NAT 出口被 fail2ban 封掉是同一类误伤。
func TestBlocklistCertVerifyFailureNotBanned(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})
	for i := 0; i < 30; i++ {
		c, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "relay.example"})
		if err == nil {
			c.Close()
			t.Fatal("前置：自签证书应校验失败")
		}
		waitEvents(t, bl, "127.0.0.1", min(i+1, 20)) // 事件列表最多保留 20 条
	}
	time.Sleep(200 * time.Millisecond) // 等最后几条连接处理完
	if bl.IsBanned("127.0.0.1") {
		t.Fatal("证书校验失败的客户端重连 30 次被封了")
	}
	e, _ := bl.Detail("127.0.0.1")
	if e.Points != 0 || len(e.Events) == 0 || e.Events[0].Kind != string(blocklist.KindTLSAborted) || e.Events[0].Detail != "alert" {
		t.Fatalf("对端 alert 应记为 tls_aborted/alert 且不计分: points=%d events=%+v", e.Points, e.Events)
	}
}

// B2：每 30 秒一次的纯 TCP 健康检查（连上就断）持续 30 分钟不得被封。
func TestBlocklistTCPHealthCheckNotBanned(t *testing.T) {
	clk := &manualClock{now: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)}
	cfg := blocklist.DefaultConfig()
	cfg.Now = clk.Now
	bl, err := blocklist.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bl.Close()
	_, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})
	for i := 0; i < 60; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		waitEvents(t, bl, "127.0.0.1", min(i+1, 20))
		clk.Advance(30 * time.Second)
	}
	time.Sleep(200 * time.Millisecond)
	if bl.IsBanned("127.0.0.1") {
		t.Fatal("TCP 健康检查被封了")
	}
}

// sendGarbage 发 n 次非 TLS 字节（每次新建 TCP 连接）。
func sendGarbage(t *testing.T, addr string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, '\n'})
		readUntilClosed(t, c)
		c.Close()
	}
}

// B2 的"近期成功加入豁免"见 blocklist_r4_test.go：第四轮起只有不同出口、持续配对达到阈值才授予
// （原测试用同一 IP 自注册自加入，恰好演示了自配对白嫖豁免的绕过）。

// 第三轮 S1：仅注册（无需任何凭据）不授予豁免——扫描器注册一次后照样在第 20 次握手垃圾时被封。
func TestBlocklistRegisterOnlyNotExempt(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})
	lc := dialLine(t, addr, true)
	lc.send(t, proto.MsgRegisterRequest, proto.RegisterRequest{ClientID: "cid-scanner", Version: "v1.1.0", Host: "财务部-张三-PC"})
	if m := lc.recv(t); m == nil || m.Type != proto.MsgRegisterResponse {
		t.Fatal("注册失败")
	}
	lc.c.Close()
	sendGarbage(t, addr, 20)
	time.Sleep(100 * time.Millisecond)
	if !bl.IsBanned("127.0.0.1") {
		t.Fatal("仅注册的 IP 不应免疫握手垃圾计分")
	}
	if d, _ := bl.Detail("127.0.0.1"); !strings.Contains(d.Verdict, "未认证") {
		t.Fatalf("仅注册的判定应标注未认证: %q", d.Verdict)
	}
}

// S1：已完成注册的连接发未知消息类型（新版客户端连旧 relay 的前向兼容场景）不计分。
func TestBlocklistUnknownMessageAfterRegisterNotScored(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{Blocklist: bl})
	lc := dialLine(t, addr, false)
	lc.send(t, proto.MsgRegisterRequest, proto.RegisterRequest{ClientID: "cid-new", Version: "v9.9.9"})
	if m := lc.recv(t); m == nil || m.Type != proto.MsgRegisterResponse {
		t.Fatal("注册失败")
	}
	lc.send(t, proto.MessageType("future_feature"), map[string]string{"x": "y"})
	for lc.recv(t) != nil {
	}
	lc.c.Close()
	time.Sleep(50 * time.Millisecond)
	if e, _ := bl.Detail("127.0.0.1"); e.Points != 0 || len(e.Events) != 0 {
		t.Fatalf("已注册连接的未知消息不应计分: points=%d events=%+v", e.Points, e.Events)
	}
}

// TestBlocklistHeartbeatBeforeRegisterNotScored 老客户端（0.0.7 等）断线重连时会先发几次心跳
// 再注册，线上同一出口 IP 一次重连就有 6~7 次。心跳无害，不能计分，否则多台老客户端在同一
// NAT 后面会把整个出口 IP 封掉。连接照旧关闭。
func TestBlocklistHeartbeatBeforeRegisterNotScored(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{Blocklist: bl})
	for i := 0; i < 15; i++ {
		lc := dialLine(t, addr, false)
		lc.send(t, proto.MsgHeartbeat, map[string]string{})
		for lc.recv(t) != nil {
		}
		lc.c.Close()
	}
	time.Sleep(50 * time.Millisecond)
	if bl.IsBanned("127.0.0.1") {
		t.Fatal("注册前的心跳不应导致封禁")
	}
	if e, _ := bl.Detail("127.0.0.1"); e.Points != 0 {
		t.Fatalf("注册前的心跳不应计分: points=%d events=%+v", e.Points, e.Events)
	}
}

// TestBlocklistTunnelDataBeforeRegisterScored 注册前的其他业务消息仍按协议状态违规计分。
func TestBlocklistTunnelDataBeforeRegisterScored(t *testing.T) {
	bl := newTestBlocklist(t)
	_, addr := startBlocklistRelay(t, &Config{Blocklist: bl})
	lc := dialLine(t, addr, false)
	lc.send(t, proto.MsgTunnelData, map[string]string{"data": "x"})
	for lc.recv(t) != nil {
	}
	lc.c.Close()
	time.Sleep(50 * time.Millisecond)
	e, _ := bl.Detail("127.0.0.1")
	if e.Points == 0 || len(e.Events) == 0 || e.Events[0].Kind != string(blocklist.KindProtoState) {
		t.Fatalf("注册前的隧道数据应计分: points=%d events=%+v", e.Points, e.Events)
	}
}
