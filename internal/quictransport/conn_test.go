package quictransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func testServerTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13}
}

// pair 建立一条 QUIC 连接，返回两端包装好的 Conn（服务端在客户端写出首字节后才拿到流）。
func pair(t *testing.T) (client, server *Conn) {
	return pairWith(t, NewConn)
}

func pairWith(t *testing.T, newClient func(*quic.Conn, *quic.Stream) *Conn) (client, server *Conn) {
	t.Helper()
	ln, err := quic.ListenAddr("127.0.0.1:0", TLSConfig(testServerTLS(t)), ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qc, err := quic.DialAddr(ctx, ln.Addr().String(), TLSConfig(&tls.Config{InsecureSkipVerify: true}), ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client = newClient(qc, st)
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	sc, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sst, err := sc.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	server = NewConn(sc, sst)
	buf := make([]byte, 1)
	if _, err := io.ReadFull(server, buf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// net.Conn 语义：别的 goroutine 调 Close 必须立刻唤醒阻塞中的 Read。客户端的读循环
// 靠这一点在断线重连时退出；只关写方向（QUIC stream.Close）做不到。
func TestCloseUnblocksPendingRead(t *testing.T) {
	client, _ := pair(t)
	done := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 16))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	client.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Close 后 Read 应返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close 没有唤醒阻塞中的 Read")
	}
}

// relay 拒绝连接时先写错误原因再 Close：这条最后的消息必须送达，不能随连接关闭被丢掉。
func TestCloseDeliversWrittenData(t *testing.T) {
	client, server := pair(t)
	if _, err := server.Write([]byte("bye\n")); err != nil {
		t.Fatal(err)
	}
	server.Close()

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(client)
	if string(got) != "bye\n" {
		t.Fatalf("读到 %q (err=%v)，期望关闭前写出的 \"bye\\n\"", got, err)
	}
}

func TestTLSConfigSetsALPNWithoutMutatingBase(t *testing.T) {
	base := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}
	got := TLSConfig(base)
	if len(got.NextProtos) != 1 || got.NextProtos[0] != ALPN || got.MinVersion != tls.VersionTLS13 {
		t.Fatalf("TLSConfig = NextProtos %v MinVersion %x", got.NextProtos, got.MinVersion)
	}
	if base.NextProtos[0] != "h2" || base.MinVersion != tls.VersionTLS12 {
		t.Fatal("TLSConfig 不得修改传入的 base（TCP 还在用它）")
	}
}

// Close 必须唤醒卡在流控上的 Write，而不是为了串行化 stream.Close 去等它：对端不读时
// Write 会一直阻塞，relay 的关停与读循环退出都依赖 Close 及时返回。
func TestCloseUnblocksBlockedWrite(t *testing.T) {
	client, _ := pair(t) // 服务端从不读，客户端很快写满流控窗口
	werr := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := client.Write(buf); err != nil {
				werr <- err
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	closed := make(chan struct{})
	go func() { client.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		// 不能 t.Fatal 后让 Cleanup 再调 Close：它会挂在同一个 closeOnce 上。直接关连接解开。
		_ = client.conn.CloseWithError(0, "")
		t.Fatal("Close 被阻塞中的 Write 卡住")
	}
	select {
	case <-werr:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 后阻塞中的 Write 没有返回")
	}
}

// 客户端 Close 同步发出 CONNECTION_CLOSE：返回时连接已关，进程随即退出也不会让 relay
// 干等 maxIdleTimeout。
func TestClientCloseIsSynchronous(t *testing.T) {
	client, _ := pairWith(t, NewClientConn)
	client.Close()
	select {
	case <-client.conn.Context().Done():
	default:
		t.Fatal("客户端 Close 返回时连接应已关闭")
	}
}

// Abort 不发 FIN：对端根本拿不到这条流，relay 不会为竞速输家进一次连接处理。
func TestAbortDoesNotDeliverStream(t *testing.T) {
	ln, err := quic.ListenAddr("127.0.0.1:0", TLSConfig(testServerTLS(t)), ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qc, err := quic.DialAddr(ctx, ln.Addr().String(), TLSConfig(&tls.Config{InsecureSkipVerify: true}), ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go NewClientConn(qc, st).Abort()

	actx, acancel := context.WithTimeout(context.Background(), time.Second)
	defer acancel()
	if _, err := sc.AcceptStream(actx); err == nil {
		t.Fatal("Abort 后对端不应拿到流")
	}
}
