package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/remote-assist/tool/internal/quictransport"
)

// quicStreamAcceptTimeout 握手完成后等客户端开流的上限。正常客户端连上立刻发
// register/join，开流就在那一刻；握手完却迟迟不开流的连接直接关掉。
var quicStreamAcceptTimeout = 10 * time.Second

// quicListener 是 QUIC 监听器与它独占的 UDP socket。自建 Transport 才能开源地址校验，
// 代价是关 Listener 不会顺带关 Transport，而 Transport 也不关调用方传入的 socket，
// 所以三者一起关，否则端口会一直被占着。
type quicListener struct {
	*quic.Listener
	tr *quic.Transport
	pc net.PacketConn
}

func (l *quicListener) Close() {
	_ = l.Listener.Close()
	_ = l.tr.Close()
	_ = l.pc.Close()
}

// listenQUIC 在 QUICListenAddr 上监听 QUIC。QUIC 强制 TLS，所以没有证书时直接报错，
// 而不是悄悄只开 TCP——显式配置了却没生效，现场只会表现成「UDP 连不上」。
func (s *Server) listenQUIC(tlsConfig *tls.Config) (*quicListener, error) {
	if tlsConfig == nil {
		return nil, errors.New("--quic-listen requires TLS (--cert/--key)")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", s.config.QUICListenAddr)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{
		Conn: pc,
		// 一律先做源地址校验（Retry）：未经校验的 UDP 首包就能让 relay 做一次握手签名，
		// 伪造源地址可以白嫖这份算力；多一个 RTT 对兜底通道无所谓。校验过后才按真实
		// 源 IP 占连接名额，per-IP 限流才有意义。
		VerifySourceAddress: func(net.Addr) bool { return true },
	}
	ln, err := tr.Listen(quictransport.TLSConfig(tlsConfig), quictransport.ServerConfig())
	if err != nil {
		_ = tr.Close()
		_ = pc.Close()
		return nil, err
	}
	return &quicListener{Listener: ln, tr: tr, pc: pc}, nil
}

// serveQUIC 是 QUIC 的 Accept 循环。
func (s *Server) serveQUIC(ctx context.Context, ln *quicListener) {
	for {
		qc, err := ln.Accept(ctx)
		if err != nil {
			// quic-go 的 Accept 一旦出错就是终态（监听或底层 socket 已关），继续循环只会
			// 拿到同一个错误空转，所以除关停外记一行就退出。
			if ctx.Err() == nil {
				log.Printf("QUIC accept stopped: %v", err)
			}
			return
		}
		go s.acceptQUICConn(ctx, qc)
	}
}

// acceptQUICConn 先按源 IP 占连接名额，再等客户端开流——不能等开流后再占：握手完成而
// 不开流的连接同样占着 goroutine 和连接状态，否则单个 IP 能开任意多条，挤满 quic-go
// 有限的 Accept 队列，挡住正常客户端。之后与 TCP 一样进 serveConn。
func (s *Server) acceptQUICConn(ctx context.Context, qc *quic.Conn) {
	clientIP := qc.RemoteAddr().String()
	host, _, err := net.SplitHostPort(clientIP)
	if err != nil {
		host = clientIP
	}
	if !s.acquireConnSlot(host) {
		log.Printf("Connection rejected from %s: too many connections", clientIP)
		_ = qc.CloseWithError(0, "too many connections")
		return
	}
	defer s.releaseConnSlot(host)

	sctx, cancel := context.WithTimeout(ctx, quicStreamAcceptTimeout)
	stream, err := qc.AcceptStream(sctx)
	cancel()
	if err != nil {
		_ = qc.CloseWithError(0, "no stream")
		return
	}
	s.serveConn(quictransport.NewConn(qc, stream), clientIP)
}
