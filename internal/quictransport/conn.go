// Package quictransport 是 relay 控制连接的 QUIC（UDP）备用传输。
//
// 只在 TCP 不可达时兜底：例如 relay 所在 IP 被丢弃回程 TCP（SYN 到得了服务端，SYN-ACK
// 回不来），但 UDP 仍然通。应用层协议完全不变——一条 QUIC 连接只承载一条双向流，
// 包装成 net.Conn 后交给与 TCP 相同的读写代码，限流、黑名单、会话逻辑全部复用。
package quictransport

import (
	"crypto/tls"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// ALPN 区分 relay 协议与同端口上可能存在的其他 QUIC 服务（如 HTTP/3）。
const ALPN = "rat-relay/1"

const (
	// keepAlivePeriod 取代 TCP keepalive：UDP 没有连接状态，NAT 映射一般 30s 左右就回收。
	keepAlivePeriod = 15 * time.Second
	// maxIdleTimeout 对端静默超过这么久视为断开。应用层另有 readIdleTimeout 兜底。
	maxIdleTimeout = 60 * time.Second
	// handshakeIdleTimeout 握手阶段的静默上限，挡住只发首包不完成握手的连接。
	handshakeIdleTimeout = 10 * time.Second
	// closeLinger 关闭时等对端收完已发数据的上限：先发 FIN，再关连接，避免拒绝原因
	// 之类的最后一条消息被连接关闭一起丢掉（TCP 的 close 会把缓冲区发完，QUIC 不会）。
	closeLinger = 2 * time.Second
	// clientCloseLinger 客户端同步等待的上限。客户端常在 Close 后立刻退出进程，异步
	// 关连接的 goroutine 会随进程消失、CONNECTION_CLOSE 发不出去，relay 只能等
	// maxIdleTimeout 才发现断开；所以客户端同步等一小段再同步关，代价是 Close 最多慢这么久。
	clientCloseLinger = 500 * time.Millisecond
)

// ServerConfig relay 侧的 QUIC 参数：每个连接只允许一条双向流，不允许单向流。
func ServerConfig() *quic.Config {
	return &quic.Config{
		MaxIncomingStreams:    1,
		MaxIncomingUniStreams: -1,
		KeepAlivePeriod:       keepAlivePeriod,
		MaxIdleTimeout:        maxIdleTimeout,
		HandshakeIdleTimeout:  handshakeIdleTimeout,
	}
}

// ClientConfig 客户端侧的 QUIC 参数：不接受对端开流，流只由客户端打开。
func ClientConfig() *quic.Config {
	return &quic.Config{
		MaxIncomingStreams:    -1,
		MaxIncomingUniStreams: -1,
		KeepAlivePeriod:       keepAlivePeriod,
		MaxIdleTimeout:        maxIdleTimeout,
		HandshakeIdleTimeout:  handshakeIdleTimeout,
	}
}

// TLSConfig 在 TCP 用的 tls.Config 基础上设置 ALPN。证书、TOFU 钉扎回调原样沿用，
// 所以同一台 relay 走 TCP 还是 QUIC 看到的是同一个指纹。QUIC 本身强制 TLS 1.3。
func TLSConfig(base *tls.Config) *tls.Config {
	c := base.Clone()
	c.NextProtos = []string{ALPN}
	if c.MinVersion < tls.VersionTLS13 {
		c.MinVersion = tls.VersionTLS13
	}
	return c
}

// Conn 把一条 QUIC 双向流包装成 net.Conn。
type Conn struct {
	conn   *quic.Conn
	stream *quic.Stream
	// writeMu 串行化 Write 与 Close 里的 stream.Close：quic-go 约定二者不得并发，
	// 而 relay 写失败时、读循环退出时都会在别的 goroutine 里 Close。
	writeMu   sync.Mutex
	closeOnce sync.Once
	// syncClose 客户端侧：Close 同步等对端、同步发出 CONNECTION_CLOSE（见 clientCloseLinger）。
	syncClose bool
}

var _ net.Conn = (*Conn)(nil)

// NewConn 包装 relay 侧的连接与其上唯一的一条流。Close 会同时关闭两者，关连接异步完成。
func NewConn(conn *quic.Conn, stream *quic.Stream) *Conn {
	return &Conn{conn: conn, stream: stream}
}

// NewClientConn 同 NewConn，但 Close 同步完成，进程随后退出也能让 relay 立刻感知断开。
func NewClientConn(conn *quic.Conn, stream *quic.Stream) *Conn {
	return &Conn{conn: conn, stream: stream, syncClose: true}
}

func (c *Conn) Read(p []byte) (int, error) { return c.stream.Read(p) }

func (c *Conn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.stream.Write(p)
}

// Close 按 net.Conn 语义立即唤醒阻塞中的 Read 与 Write，同时尽量把已写出的数据送达：
// 停止接收、发 FIN，等对端收完（或 linger 到期）后再关连接。
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.stream.CancelRead(0)
		// 让卡在流控上的 Write 立刻返回，否则下面拿 writeMu 会一直等它。只影响 Write
		// 调用的返回，已进发送队列的数据照常发出。
		_ = c.stream.SetWriteDeadline(time.Now())
		c.writeMu.Lock()
		_ = c.stream.Close()
		c.writeMu.Unlock()
		if c.syncClose {
			c.closeConnAfter(clientCloseLinger)
			return
		}
		go c.closeConnAfter(closeLinger)
	})
	return nil
}

func (c *Conn) closeConnAfter(linger time.Duration) {
	select {
	case <-c.conn.Context().Done():
	case <-time.After(linger):
	}
	_ = c.conn.CloseWithError(0, "")
}

// Abort 立即关闭连接，不发 FIN、不等对端。用于竞速中输掉的那一路：它从没被使用过，
// 走 Close 反而会让 relay 收到一条只带 FIN 的流、白白进一次连接处理。
func (c *Conn) Abort() {
	c.closeOnce.Do(func() {
		_ = c.conn.CloseWithError(0, "")
	})
}

func (c *Conn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }
