package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/quictransport"
)

// tlsAlertAccessDenied TLS 致命 alert：content type 21、版本 3.3、长度 2、level fatal(2)、
// description access_denied(49)。新版 relay 对被封 IP 的 TCP 连接回的就是这 7 个字节。
var tlsAlertAccessDenied = []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x31}

// blockedTCP 在 addr 上模拟新版 relay 对被封 IP 的 TCP 处理：不握手，读掉 ClientHello
// 后回 access_denied alert 再关。必须先读掉再写，否则未读数据会让内核发 RST 冲掉 alert。
// 返回实际监听地址（addr 可为 127.0.0.1:0）。
func blockedTCP(t *testing.T, addr string) string {
	t.Helper()
	return blockedTCPAfter(t, addr, 0)
}

// blockedTCPAfter 同 blockedTCP，但读完 ClientHello 后先等 delay 再回 alert。
func blockedTCPAfter(t *testing.T, addr string, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				hdr := make([]byte, 5)
				if _, err := io.ReadFull(c, hdr); err == nil {
					_, _ = io.CopyN(io.Discard, c, int64(hdr[3])<<8|int64(hdr[4]))
				}
				time.Sleep(delay)
				_, _ = c.Write(tlsAlertAccessDenied)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// closingTCP 在 addr 上模拟「连上就被关」的 TCP：读掉 ClientHello 后什么也不回直接关。
// 新版 relay 给被封连接回提示的名额占满时就是这样。
func closingTCP(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				hdr := make([]byte, 5)
				if _, err := io.ReadFull(c, hdr); err == nil {
					_, _ = io.CopyN(io.Discard, c, int64(hdr[3])<<8|int64(hdr[4]))
				}
			}(c)
		}
	}()
}

// syncBuf 是并发安全的日志缓冲。
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog 在测试期间把标准 log 的输出收进缓冲。
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	prev := log.Writer()
	buf := &syncBuf{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// quicCloser 在 addr 的 UDP 上起一个 QUIC 服务端：每条连接握手完成后以 reason 关闭，
// 返回实际监听地址与累计接受的连接数。reason 为 proto.BlockedMessage 即模拟新版 relay
// 对被封 IP 的处理。
func quicCloser(t *testing.T, addr, cert, key, reason string) (string, *atomic.Int32) {
	t.Helper()
	return quicCloserWith(t, addr, cert, key, reason, quictransport.ServerConfig())
}

// quicCloserWith 同 quicCloser，服务端用 cfg。
func quicCloserWith(t *testing.T, addr, cert, key, reason string, cfg *quic.Config) (string, *atomic.Int32) {
	t.Helper()
	tlsCert, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr(addr, quictransport.TLSConfig(&tls.Config{Certificates: []tls.Certificate{tlsCert}}), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			qc, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			n.Add(1)
			_ = qc.CloseWithError(0, reason)
		}
	}()
	return ln.Addr().String(), &n
}

// 被封 IP 走 TCP：relay 回 access_denied，客户端必须识别为封禁，并且不再回落 QUIC——
// 封禁与传输无关，回落只会多一次被拒，还会先打出一行误导的「TCP 不可达」。
func TestTCPAccessDeniedIsBlockedWithoutQUICFallback(t *testing.T) {
	setQuicRaceDelay(t, 10*time.Second)
	setDialTimeout(t, 5*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	blockedTCP(t, addr)
	_, quicDials := quicCloser(t, addr, cert, key, "")

	c := testClient(t, addr, "")
	err := c.Connect()
	if !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("Connect() = %v（transport=%q），期望 ErrBlocked", err, c.transport)
	}
	if !strings.Contains(err.Error(), proto.BlockedMessage) {
		t.Fatalf("错误文本 %q 不含 %q", err, proto.BlockedMessage)
	}
	time.Sleep(300 * time.Millisecond)
	if n := quicDials.Load(); n != 0 {
		t.Fatalf("TCP 已明确被封，QUIC 却又被拨了 %d 次", n)
	}
}

// TCP 被封的结果到达时 QUIC 已经在拨（TCP 慢于竞速延迟）：直接结束，不等 QUIC 拨到超时。
func TestTCPAccessDeniedDoesNotWaitForInflightQUIC(t *testing.T) {
	setQuicRaceDelay(t, 10*time.Millisecond)
	setDialTimeout(t, 5*time.Second)
	addr := freePort(t)
	blockedTCPAfter(t, addr, 300*time.Millisecond)
	// 同端口的 UDP 只收不回：QUIC 拨号会一直挂到超时。
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })

	c := testClient(t, addr, "")
	start := time.Now()
	err = c.Connect()
	if !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("Connect() = %v，期望 ErrBlocked", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("TCP 已明确被封，却等了 %v（在等 QUIC 超时）", d)
	}
}

// relay 给被封连接回提示的名额占满时，TCP 被直接关掉（不是 access_denied），客户端照常
// 回落 QUIC，再在 join 时收到封禁。这时不能说「TCP 不可达」——TCP 是通的，只是被关了。
func TestQUICFallbackAfterTCPClosedNotCalledUnreachable(t *testing.T) {
	setQuicRaceDelay(t, 10*time.Second)
	setDialTimeout(t, 5*time.Second)
	addr := freePort(t)
	cert, key := newCertPair(t)
	closingTCP(t, addr)
	quicCloser(t, addr, cert, key, proto.BlockedMessage)
	logs := captureLog(t)

	h := NewHelpMode(&Config{
		ServerAddr: addr, UseTLS: true, InsecureSkip: true,
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	}, "ABCD-EFGH-JKLM", "")
	t.Cleanup(h.client.Close)
	if err := h.client.Connect(); err != nil {
		t.Fatalf("TCP 被关后应回落 QUIC 连上：%v", err)
	}
	if _, err := h.join(); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("join() = %v，期望 ErrBlocked", err)
	}
	out := logs.String()
	if strings.Contains(out, "不可达") || !strings.Contains(out, "TCP 连接失败") {
		t.Fatalf("回落 QUIC 的日志应如实说明 TCP 连接失败而不是不可达，实际：%q", out)
	}
}

// 「TCP 不可达」只用于真的连不上（拨号失败、超时、竞速时 TCP 还没回来）。
func TestLogTransportWording(t *testing.T) {
	cases := []struct {
		name   string
		tcpErr error
		want   string
	}{
		{"TCP 仍在途", nil, "TCP 不可达"},
		{"拨号被拒", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "TCP 不可达"},
		{"超时", context.DeadlineExceeded, "TCP 不可达"},
		{"连上后被关", io.EOF, "TCP 连接失败（EOF）"},
		{"连上后被重置", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}, "TCP 连接失败（read tcp: connection reset）"},
	}
	for _, c := range cases {
		logs := captureLog(t)
		logTransport("relay.example:8443", TransportQUIC, "", c.tcpErr)
		if out := logs.String(); !strings.Contains(out, c.want) || !strings.Contains(out, "已改用 QUIC") {
			t.Errorf("%s：日志 %q 应含 %q", c.name, out, c.want)
		}
	}
}

// --transport tcp 同样识别封禁（单路拨号不经竞速）。
func TestTCPModeAccessDeniedIsBlocked(t *testing.T) {
	setDialTimeout(t, 5*time.Second)
	addr := blockedTCP(t, "127.0.0.1:0")
	c := testClient(t, addr, TransportTCP)
	if err := c.Connect(); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("Connect() = %v，期望 ErrBlocked", err)
	}
}

// 被封 IP 走 QUIC：relay 在它那边握手完成后以 BlockedMessage 关闭。这时客户端的拨号已经
// 返回，关闭在第一次读写时才被看到，必须归为 ErrBlocked。
func TestQUICBlockedCloseIsBlocked(t *testing.T) {
	setDialTimeout(t, 5*time.Second)
	cert, key := newCertPair(t)
	addr, _ := quicCloser(t, "127.0.0.1:0", cert, key, proto.BlockedMessage)

	h := NewHelpMode(&Config{
		ServerAddr: addr, UseTLS: true, InsecureSkip: true, Transport: TransportQUIC,
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	}, "ABCD-EFGH-JKLM", "")
	t.Cleanup(h.client.Close)
	if err := h.client.Connect(); err != nil {
		t.Fatalf("拨号应成功（relay 握手完成后才关）：%v", err)
	}
	if _, err := h.join(); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("join() = %v，期望 ErrBlocked", err)
	}
}

// relay 的关闭在客户端开流之前就到了（客户端协程被推迟调度时可能发生）：封禁出现在拨号
// 阶段，同样必须归为 ErrBlocked。服务端不给开流名额，开流会一直挂到 relay 关闭，时序确定。
func TestQUICBlockedBeforeStreamOpenIsBlocked(t *testing.T) {
	setDialTimeout(t, 5*time.Second)
	cert, key := newCertPair(t)
	cfg := quictransport.ServerConfig()
	cfg.MaxIncomingStreams = -1
	addr, _ := quicCloserWith(t, "127.0.0.1:0", cert, key, proto.BlockedMessage, cfg)

	c := testClient(t, addr, TransportQUIC)
	if err := c.Connect(); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("Connect() = %v，期望 ErrBlocked", err)
	}
}

// 旧 relay 关 QUIC 连接不带原因：不能被误认成封禁。
func TestQUICCloseWithoutReasonIsNotBlocked(t *testing.T) {
	setDialTimeout(t, 5*time.Second)
	cert, key := newCertPair(t)
	addr, _ := quicCloser(t, "127.0.0.1:0", cert, key, "")

	h := NewHelpMode(&Config{
		ServerAddr: addr, UseTLS: true, InsecureSkip: true, Transport: TransportQUIC,
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	}, "ABCD-EFGH-JKLM", "")
	t.Cleanup(h.client.Close)
	err := h.client.Connect()
	if err == nil {
		_, err = h.join()
	}
	if err == nil || errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("err = %v，期望普通连接错误而非 ErrBlocked", err)
	}
}

// joinWithResponse 用内存管道让 join 收到指定的 JoinResponse。
func joinWithResponse(t *testing.T, resp *proto.JoinResponse) error {
	t.Helper()
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	_ = remote.SetDeadline(time.Now().Add(3 * time.Second))
	go func() {
		var req proto.Message
		if err := json.NewDecoder(remote).Decode(&req); err != nil {
			return
		}
		msg, _ := proto.NewMessage(proto.MsgJoinResponse, resp)
		_ = json.NewEncoder(remote).Encode(msg)
	}()
	h := NewHelpMode(&Config{}, "ABCD-EFGH-JKLM", "")
	h.client = &Client{conn: local, enc: json.NewEncoder(local), dec: json.NewDecoder(local)}
	_, err := h.join()
	return err
}

// 设备被封：relay 在 JoinResponse.Error 里回 BlockedMessage。必须归为 ErrBlocked，
// 不能包成 "join failed"——那会被 GUI / agent 当成协助码错误。
func TestJoinBlockedResponseIsErrBlocked(t *testing.T) {
	err := joinWithResponse(t, &proto.JoinResponse{Error: proto.BlockedMessage})
	if !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("join() = %v，期望 ErrBlocked", err)
	}
	if strings.Contains(err.Error(), "join failed") {
		t.Fatalf("封禁错误不应带 join failed 前缀：%q", err)
	}

	// 普通拒绝仍走原有的 join 失败分类。
	err = joinWithResponse(t, &proto.JoinResponse{Error: "join failed"})
	if err == nil || errors.Is(err, proto.ErrBlocked) || !errors.Is(err, ErrJoinFailed) {
		t.Fatalf("普通拒绝 join() = %v，期望 ErrJoinFailed", err)
	}
}

// MCP connect 被封：返回给 agent 的错误要明说访问受限、重试无效，不能带
// "relay connect failed" / "join failed" 前缀诱导 agent 重试或换码。
func TestBootstrapConnectBlockedError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				var req proto.Message
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				msg, _ := proto.NewMessage(proto.MsgJoinResponse, &proto.JoinResponse{Error: proto.BlockedMessage})
				_ = json.NewEncoder(c).Encode(msg)
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()

	b := NewHelpMCPBootstrap(&Config{ServerAddr: ln.Addr().String(), P2PMode: "disabled"})
	_, err = b.connectWith(context.Background(), connectArgs{Code: "ABCD-EFGH-JKLM"})
	checkBlockedConnectError(t, err)
}

// MCP connect 在拨号阶段就被封（IP 被封，TCP 收到 access_denied）：同样返回 access_denied。
func TestBootstrapConnectBlockedAtDial(t *testing.T) {
	setDialTimeout(t, 5*time.Second)
	addr := blockedTCP(t, "127.0.0.1:0")
	b := NewHelpMCPBootstrap(&Config{
		ServerAddr: addr, UseTLS: true, InsecureSkip: true, Transport: TransportTCP, P2PMode: "disabled",
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	})
	_, err := b.connectWith(context.Background(), connectArgs{Code: "ABCD-EFGH-JKLM"})
	checkBlockedConnectError(t, err)
}

func checkBlockedConnectError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("connect = %v，期望包着 ErrBlocked", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "access_denied: "+proto.BlockedMessage) {
		t.Fatalf("connect 错误 %q 应以 access_denied: %s 开头", msg, proto.BlockedMessage)
	}
	for _, bad := range []string{"relay connect failed", "join failed"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("connect 错误 %q 不应含 %q", msg, bad)
		}
	}
}

// share 重连被封：只显示那一句话，按上限间隔等；普通失败保持原文案与指数退避。
func TestShareReconnectFailureBlocked(t *testing.T) {
	err := fmt.Errorf("failed to connect: %w", proto.ErrBlocked)
	line, wait, next := reconnectFailure(3, err, 2*time.Second)
	if want := "连接失败(第 3 次): " + proto.BlockedMessage + " 30s 后重试...\n"; line != want {
		t.Fatalf("line = %q，期望 %q", line, want)
	}
	if wait != reconnectMaxDelay || next != reconnectMaxDelay {
		t.Fatalf("wait=%v next=%v，期望都为 %v", wait, next, reconnectMaxDelay)
	}

	line, wait, next = reconnectFailure(2, errors.New("EOF"), 2*time.Second)
	if line != "连接失败(第 2 次): EOF；2s 后重试...\n" || wait != 2*time.Second || next != 4*time.Second {
		t.Fatalf("普通失败被改了：line=%q wait=%v next=%v", line, wait, next)
	}
	if _, _, next = reconnectFailure(9, errors.New("EOF"), 20*time.Second); next != reconnectMaxDelay {
		t.Fatalf("退避应封顶 %v，得到 %v", reconnectMaxDelay, next)
	}
}
