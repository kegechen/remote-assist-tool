package relay

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/remote-assist/tool/internal/relay/blocklist"
)

// B2/S8：握手错误分类——只有非 TLS 数据/畸形记录算垃圾，对端中止（alert/EOF/超时/重置/
// 被本端手动封禁关闭）一律不计分。
func TestClassifyHandshakeErr(t *testing.T) {
	aborted := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		net.ErrClosed,
		&net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")},
		errors.New("remote error: tls: unknown certificate authority"),
		&net.OpError{Op: "read", Err: syscall.ECONNRESET},
		timeoutErr{},
	}
	for _, err := range aborted {
		if k, _ := classifyHandshakeErr(err); k != blocklist.KindTLSAborted {
			t.Errorf("%v 应归为 tls_aborted，got %s", err, k)
		}
	}
	// 第三轮 S8：合法但不兼容的 TLS 客户端（版本、套件、ALPN 不匹配）与其他未知错误只记事件不计分。
	aborted = append(aborted,
		errors.New("tls: client offered only unsupported versions: [301 302]"),
		errors.New("tls: no cipher suite supported by both client and server"),
		errors.New("tls: client requested unsupported application protocols ([h2])"),
		errors.New("some future crypto/tls error"),
	)
	for _, err := range aborted {
		if k, _ := classifyHandshakeErr(err); k != blocklist.KindTLSAborted {
			t.Errorf("%v 应归为 tls_aborted，got %s", err, k)
		}
	}
	garbage := []error{
		tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
		errors.New("tls: unexpected message"),
		errors.New("tls: oversized record received with length 20000"),
		errors.New("tls: unsupported SSLv2 handshake received"),
		errors.New("local error: tls: error decoding message"),
	}
	for _, err := range garbage {
		if k, _ := classifyHandshakeErr(err); k != blocklist.KindTLSGarbage {
			t.Errorf("%v 应归为 tls_garbage，got %s", err, k)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
