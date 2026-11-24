package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/quictransport"
	"github.com/remote-assist/tool/internal/relay/blocklist"
)

// accessDeniedErr 是 Go crypto/tls 客户端收到 access_denied alert 时的错误文本。
const accessDeniedErr = "remote error: tls: access denied"

// bannedLocalhost 返回已封禁 127.0.0.1 的黑名单。
func bannedLocalhost(t *testing.T) *blocklist.List {
	t.Helper()
	bl := newTestBlocklist(t)
	if _, err := bl.Ban("127.0.0.1", time.Hour, false, "admin", "测试"); err != nil {
		t.Fatal(err)
	}
	return bl
}

func dialTLS(addr string) error {
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err == nil {
		c.Close()
	}
	return err
}

// waitNotices 等正在回 alert 的协程数变成 want。
func waitNotices(t *testing.T, srv *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(srv.blockedNotice) != want {
		if time.Now().After(deadline) {
			t.Fatalf("回 alert 的协程数应为 %d，实际 %d", want, len(srv.blockedNotice))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeHello 是一条最小的 TLS 握手记录（sendAccessDenied 只看记录头）；fakeTrailing 是紧跟
// 其后的另一条记录（change_cipher_spec）。
var (
	fakeHello    = []byte{0x16, 0x03, 0x01, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00}
	fakeTrailing = []byte{0x14, 0x03, 0x03, 0x00, 0x01, 0x01}
)

// scriptConn 是给 sendAccessDenied 用的假连接，按顺序记下每次写、半关、关闭时还剩多少
// 未读。「先读后写、写完先半关再读空」这个顺序在真实 socket 上只有部分系统能观察到。
type scriptConn struct {
	net.Conn // 其余方法用不到，调用即 panic
	r        *bytes.Reader
	events   []string
}

func (c *scriptConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *scriptConn) Write(p []byte) (int, error) {
	c.events = append(c.events, fmt.Sprintf("write %x unread=%d", p, c.r.Len()))
	return len(p), nil
}
func (c *scriptConn) CloseWrite() error {
	c.events = append(c.events, fmt.Sprintf("fin unread=%d", c.r.Len()))
	return nil
}
func (c *scriptConn) Close() error {
	c.events = append(c.events, fmt.Sprintf("close unread=%d", c.r.Len()))
	return nil
}
func (c *scriptConn) SetDeadline(time.Time) error { return nil }

// 关闭时接收缓冲里还有未读数据，内核会发 RST，客户端可能来不及读到 alert。所以：写 alert
// 之前恰好读完第一条记录；写完先半关（发 FIN），再把对端后续发来的数据读空，最后才关。
func TestSendAccessDeniedOrder(t *testing.T) {
	c := &scriptConn{r: bytes.NewReader(append(append([]byte{}, fakeHello...), fakeTrailing...))}
	sendAccessDenied(c)
	want := []string{
		fmt.Sprintf("write %x unread=%d", tlsAlertAccessDenied, len(fakeTrailing)),
		fmt.Sprintf("fin unread=%d", len(fakeTrailing)),
		"close unread=0",
	}
	if !reflect.DeepEqual(c.events, want) {
		t.Fatalf("事件顺序\n got %q\nwant %q", c.events, want)
	}
}

// 首条记录不像 TLS 握手记录（类型、主版本号、长度不对）或没发完：什么也不回，直接关。
func TestSendAccessDeniedIgnoresNonTLS(t *testing.T) {
	for name, in := range map[string][]byte{
		"不是握手记录":   {0x17, 0x03, 0x03, 0x00, 0x01, 0x00},
		"主版本号不是 3": {0x16, 0x00, 0x00, 0x00, 0x01, 0x00},
		"记录超长":     append([]byte{0x16, 0x03, 0x01, 0x40, 0x01}, make([]byte, 1<<14+1)...),
		"记录没发完":    {0x16, 0x03, 0x01, 0x00, 0x04, 0x01},
		"记录头没发完":   {0x16, 0x03},
	} {
		c := &scriptConn{r: bytes.NewReader(in)}
		sendAccessDenied(c)
		if len(c.events) != 1 || !strings.HasPrefix(c.events[0], "close ") {
			t.Errorf("%s：不应回任何东西，实际 %q", name, c.events)
		}
	}
}

// 被封 IP 的 TCP 连接：relay 不握 TLS，读掉 ClientHello 后回 access_denied alert 再关，
// 所有版本的 Go 客户端都会得到 "remote error: tls: access denied"（新客户端据此显示
// 「访问受限，请联系管理员。」），而不是一个看不出原因的 EOF。
func TestBlockedTCPGetsAccessDeniedAlert(t *testing.T) {
	bl := bannedLocalhost(t)
	srv, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})
	for i := 0; i < 20; i++ {
		err := dialTLS(addr)
		if err == nil {
			t.Fatal("被封 IP 不应能完成 TLS 握手")
		}
		if err.Error() != accessDeniedErr {
			t.Fatalf("第 %d 次：被封 IP 应收到 access_denied alert，实际 %v", i+1, err)
		}
	}
	if n := atomic.LoadUint64(&srv.blockedSampleCtr); n < 20 {
		t.Fatalf("每次拒绝都应照旧记录，实际 %d 次", n)
	}
	// 客户端收到 alert 就关，名额随之归还。
	waitNotices(t, srv, 0)
}

// 回 alert 的协程有全局名额：占满后新的被封连接立即静默关闭，不排队、不阻塞 accept；
// 名额释放后恢复回 alert。
func TestBlockedTCPNoticeLimit(t *testing.T) {
	oldMax, oldTimeout := blockedNoticeMax, blockedNoticeTimeout
	blockedNoticeMax, blockedNoticeTimeout = 2, 5*time.Second
	t.Cleanup(func() { blockedNoticeMax, blockedNoticeTimeout = oldMax, oldTimeout })

	bl := bannedLocalhost(t)
	srv, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})

	// 两条连上不发数据的连接占满名额（各自要等 5 秒读超时）。中途失败也要关掉，别让
	// 关停多等那 5 秒。
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	waitNotices(t, srv, 2)

	start := time.Now()
	err := dialTLS(addr)
	if err == nil || err.Error() == accessDeniedErr {
		t.Fatalf("名额占满时应静默关闭，实际 %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("名额占满时被封连接等了 %v 才被关，accept 被阻塞了", d)
	}

	// 对端一关，读到 EOF 立即归还名额，之后恢复回 alert。
	for _, c := range held {
		c.Close()
	}
	waitNotices(t, srv, 0)
	if err := dialTLS(addr); err == nil || err.Error() != accessDeniedErr {
		t.Fatalf("名额释放后应恢复回 alert，实际 %v", err)
	}
}

// alert 只回给 TLS 客户端：首条记录不是 TLS 握手（扫描器发 HTTP、垃圾字节）的连接什么也
// 收不到，照旧静默关闭。
func TestBlockedTCPNonTLSGetsNothing(t *testing.T) {
	bl := bannedLocalhost(t)
	_, addr := startBlocklistRelay(t, &Config{UseTLS: true, Blocklist: bl})
	for _, payload := range [][]byte{
		[]byte("GET / HTTP/1.1\r\nHost: relay\r\n\r\n"),
		{0x17, 0x03, 0x03, 0x00, 0x01, 0x00}, // 长度自洽，但记录类型不是握手
	} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write(payload)
		if n := readUntilClosed(t, c); n != 0 {
			t.Fatalf("非 TLS 连接 %q 收到 %d 字节，应静默关闭", payload, n)
		}
		c.Close()
	}
}

// --plain 没有 TLS，回 alert 无从谈起：被封连接照旧立即静默关闭，即使对方发的是 TLS 握手。
func TestBlockedPlainStaysSilent(t *testing.T) {
	bl := bannedLocalhost(t)
	_, addr := startBlocklistRelay(t, &Config{Blocklist: bl})
	if err := dialTLS(addr); err == nil || strings.Contains(err.Error(), "access denied") {
		t.Fatalf("明文 relay 不应回 TLS alert，实际 %v", err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x01, 0x00})
	if n := readUntilClosed(t, c); n != 0 {
		t.Fatalf("明文模式被封连接收到 %d 字节，应静默关闭", n)
	}
	c.Close()
}

// 被封 IP 的 QUIC 连接握手完成后带原因关闭：老客户端的错误里也能看到「访问受限」，新客户端
// 据此映射为 proto.ErrBlocked。
func TestQUICBlockedCloseCarriesNotice(t *testing.T) {
	bl := bannedLocalhost(t)
	_, addr := startQUICRelay(t, func(c *Config) { c.Blocklist = bl })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qc, err := quic.DialAddr(ctx, addr, quictransport.TLSConfig(&tls.Config{InsecureSkipVerify: true}), quictransport.ClientConfig())
	if err == nil {
		// relay 握手后才查黑名单，关闭帧通常在握手完成之后到达：等连接被关，取关闭原因。
		_, err = qc.AcceptStream(ctx)
	}
	var ae *quic.ApplicationError
	if !errors.As(err, &ae) || !ae.Remote || ae.ErrorMessage != proto.BlockedMessage {
		t.Fatalf("被封 IP 的 QUIC 连接应以「%s」关闭，实际 %v", proto.BlockedMessage, err)
	}
	if !strings.Contains(err.Error(), proto.BlockedMessage) {
		t.Fatalf("老客户端直接打印错误时应能看到提示，实际 %q", err.Error())
	}
}
