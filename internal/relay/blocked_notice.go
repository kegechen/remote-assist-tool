package relay

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"time"
)

// 被封 IP 的 TCP 连接不握 TLS，读掉 ClientHello 后回一条 TLS 致命 alert（access_denied）再关，
// 客户端因此能分清「被封」与「网络不通」。不做任何加密计算。
var (
	// blockedNoticeMax 同时在回 alert 的协程上限。占满时新的被封连接直接静默关闭，被封
	// 连接的成本仍是 O(1)，有人狂连也堆不起协程。
	blockedNoticeMax = 256
	// blockedNoticeTimeout 等 ClientHello 的上限。正常客户端连上立刻就发。
	blockedNoticeTimeout = time.Second
)

const (
	// tlsRecordHandshake 是 TLS 握手记录的类型字节，ClientHello 就装在这种记录里。
	tlsRecordHandshake = 0x16
	// tlsMaxRecord 是 TLS 明文记录的长度上限（2^14）。
	tlsMaxRecord = 1 << 14
)

// tlsAlertAccessDenied：记录类型 alert(21)、版本 3.3、长度 2、级别 fatal(2)、access_denied(49)。
var tlsAlertAccessDenied = []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x31}

// noticeBlocked 在后台给被封连接回 access_denied alert 并关闭，返回是否已接手；没接手的由
// 调用方立即静默关闭。明文（--plain）没有 TLS，名额占满时也不排队——accept 循环从不等它。
func (s *Server) noticeBlocked(conn net.Conn) bool {
	raw := tlsRawConn(conn)
	if raw == nil {
		return false
	}
	select {
	case s.blockedNotice <- struct{}{}:
	default:
		return false
	}
	go func() {
		defer func() { <-s.blockedNotice }()
		sendAccessDenied(raw)
	}()
	return true
}

// tlsRawConn 取 TLS 连接底下的原始 TCP 连接（此时尚未握手）；不是 TLS 返回 nil。
func tlsRawConn(conn net.Conn) net.Conn {
	if tc, ok := conn.(*tls.Conn); ok {
		return tc.NetConn()
	}
	return nil
}

// sendAccessDenied 读掉第一条记录（ClientHello）后回 alert 再关。关闭时接收缓冲里还有未读
// 数据，内核会发 RST，客户端可能来不及读到 alert，所以前后都要防：写之前先读完第一条记录；
// 写之后先半关（发 FIN），再把对端后续发来的数据读空，等对端关闭后才关。首条记录不像 TLS
// 握手记录（类型、主版本号、长度不对）或没发完，就什么也不回直接关。全程受同一个截止时间
// 约束。
func sendAccessDenied(raw net.Conn) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(blockedNoticeTimeout))
	var hdr [5]byte
	if _, err := io.ReadFull(raw, hdr[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint16(hdr[3:])
	if hdr[0] != tlsRecordHandshake || hdr[1] != 3 || n > tlsMaxRecord {
		return
	}
	if _, err := io.CopyN(io.Discard, raw, int64(n)); err != nil {
		return
	}
	if _, err := raw.Write(tlsAlertAccessDenied); err != nil {
		return
	}
	if cw, ok := raw.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		_, _ = io.Copy(io.Discard, raw)
	}
}
