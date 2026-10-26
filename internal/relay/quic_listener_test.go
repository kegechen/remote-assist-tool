package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/quictransport"
)

// startQUICRelay 起一个 TCP+QUIC 的 TLS relay，返回 QUIC 地址。
func startQUICRelay(t *testing.T, mutate func(*Config)) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := crypto.GenerateSelfSignedCert(cert, key); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		ListenAddr: "127.0.0.1:0", QUICListenAddr: "127.0.0.1:0",
		UseTLS: true, TLSCertFile: cert, TLSKeyFile: key,
		CodeTTL: time.Minute, CodeLength: 10,
	}
	if mutate != nil {
		mutate(cfg)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	errc := make(chan error, 1)
	go func() { errc <- srv.StartWithContextReady(ctx, func() { close(ready) }) }()
	t.Cleanup(cancel)
	select {
	case <-ready:
	case err := <-errc:
		t.Fatalf("relay 启动失败: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("relay 启动超时")
	}
	if a := srv.QUICAddr(); a != nil {
		return srv, a.String()
	}
	return srv, ""
}

type lineConn struct {
	net.Conn
	r *bufio.Reader
}

func dialQUIC(t *testing.T, addr string) *lineConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qc, err := quic.DialAddr(ctx, addr, quictransport.TLSConfig(&tls.Config{InsecureSkipVerify: true}), quictransport.ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := quictransport.NewConn(qc, st)
	t.Cleanup(func() { c.Close() })
	return &lineConn{Conn: c, r: bufio.NewReader(c)}
}

func (c *lineConn) send(t *testing.T, typ proto.MessageType, payload interface{}) {
	t.Helper()
	msg, err := proto.NewMessage(typ, payload)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(msg)
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// recv 读到指定类型的消息为止（跳过心跳之类的旁路消息）。
func (c *lineConn) recv(t *testing.T, want proto.MessageType, out interface{}) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("等 %s 时读失败: %v", want, err)
		}
		var msg proto.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == want {
			if err := json.Unmarshal(msg.Payload, out); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

// QUIC 上的连接走与 TCP 完全相同的处理：share 注册拿码、help 用码加入。
func TestQUICShareRegisterAndHelpJoin(t *testing.T) {
	_, addr := startQUICRelay(t, nil)

	share := dialQUIC(t, addr)
	share.send(t, proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "v1.1.0", Host: "u@pc"})
	var reg proto.RegisterResponse
	share.recv(t, proto.MsgRegisterResponse, &reg)
	if reg.Code == "" {
		t.Fatal("QUIC 上注册未拿到协助码")
	}

	help := dialQUIC(t, addr)
	help.send(t, proto.MsgJoinRequest, &proto.JoinRequest{Code: reg.Code, Version: "v1.1.0"})
	var join proto.JoinResponse
	help.recv(t, proto.MsgJoinResponse, &join)
	if !join.Success || join.PeerHost != "u@pc" {
		t.Fatalf("QUIC 上加入失败: %+v", join)
	}
}

// QUIC 连接同样计入连接数准入：同 IP 超限的第二条 QUIC 连接被关闭。
func TestQUICConnectionsCountTowardLimits(t *testing.T) {
	srv, addr := startQUICRelay(t, func(c *Config) {
		c.Limits = DefaultLimits()
		c.Limits.MaxConnectionsPerIP = 1
	})

	first := dialQUIC(t, addr)
	first.send(t, proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "v1.1.0"})
	var reg proto.RegisterResponse
	first.recv(t, proto.MsgRegisterResponse, &reg)

	second := dialQUIC(t, addr)
	second.send(t, proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "v1.1.0"})
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := second.r.ReadBytes('\n'); err == nil {
		t.Fatalf("超出每 IP 连接上限的 QUIC 连接应被关闭，却收到 %s", line)
	}
	srv.connMu.Lock()
	total := srv.connTotal
	srv.connMu.Unlock()
	if total != 1 {
		t.Fatalf("connTotal=%d，期望只有第一条 QUIC 连接占名额", total)
	}
}

// 一个 QUIC 连接只承载一条控制流：第二条流开不出来，避免一条连接复用出多个会话绕过限流。
func TestQUICRejectsSecondStream(t *testing.T) {
	_, addr := startQUICRelay(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qc, err := quic.DialAddr(ctx, addr, quictransport.TLSConfig(&tls.Config{InsecureSkipVerify: true}), quictransport.ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer qc.CloseWithError(0, "")
	if _, err := qc.OpenStream(); err != nil {
		t.Fatalf("第一条流应可打开: %v", err)
	}
	if _, err := qc.OpenStream(); err == nil {
		t.Fatal("第二条流应因 MaxIncomingStreams=1 被拒")
	}
}

// QUIC 强制 TLS：显式配置了 --quic-listen 却没有证书时启动直接失败，而不是悄悄只开 TCP。
func TestQUICListenRequiresTLS(t *testing.T) {
	srv, err := NewServer(&Config{ListenAddr: "127.0.0.1:0", QUICListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.StartWithContext(ctx); err == nil {
		t.Fatal("无 TLS 时开启 QUIC 应启动失败")
	}
}

// 不配置 QUICListenAddr 时不监听 UDP（默认行为不变）。
func TestQUICDisabledByDefault(t *testing.T) {
	srv, _ := startQUICRelay(t, func(c *Config) { c.QUICListenAddr = "" })
	if srv.QUICAddr() != nil {
		t.Fatalf("未配置 --quic-listen 却监听了 %v", srv.QUICAddr())
	}
}

// 握手完成但迟迟不开流的连接也要占每 IP 名额：否则单个 IP 能并发开任意多条，挤满
// quic-go 有限的 Accept 队列。
func TestQUICHandshakeWithoutStreamCountsTowardLimits(t *testing.T) {
	_, addr := startQUICRelay(t, func(c *Config) {
		c.Limits = DefaultLimits()
		c.Limits.MaxConnectionsPerIP = 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idle, err := quic.DialAddr(ctx, addr, quictransport.TLSConfig(&tls.Config{InsecureSkipVerify: true}), quictransport.ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.CloseWithError(0, "")
	time.Sleep(200 * time.Millisecond) // 让 relay 处理完这条连接的 Accept

	c := dialQUIC(t, addr)
	c.send(t, proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "v1.1.0"})
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := c.r.ReadBytes('\n'); err == nil {
		t.Fatalf("已有一条未开流的连接占满每 IP 名额，第二条应被拒，却收到 %s", line)
	}
}

// Accept 出错是终态：serveQUIC 必须退出，而不是在同一个错误上空转。
func TestServeQUICReturnsWhenSocketClosed(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := crypto.GenerateSelfSignedCert(cert, key); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(&Config{QUICListenAddr: "127.0.0.1:0", UseTLS: true, TLSCertFile: cert, TLSKeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := crypto.NewTLSConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := srv.listenQUIC(tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { srv.serveQUIC(context.Background(), ln); close(done) }()
	_ = ln.tr.Close() // 只关底层 socket，ctx 未取消
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		ln.Close()
		t.Fatal("底层 socket 关闭后 serveQUIC 没有退出")
	}
}

// 关停 QUIC 监听必须释放 UDP 端口：自建 Transport 时 quic-go 不会替我们关传入的 socket，
// 否则同进程内重新监听（以及测试里反复起停 relay）会撞上 address already in use。
func TestQUICListenerCloseReleasesUDPPort(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := crypto.GenerateSelfSignedCert(cert, key); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := crypto.NewTLSConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(&Config{QUICListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := srv.listenQUIC(tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("关闭 QUIC 监听后 UDP 端口 %s 仍被占用: %v", addr, err)
	}
	pc.Close()
}
