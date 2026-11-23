package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/quic-go/quic-go"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/quictransport"
)

// fetchFingerprint 实际连一次 addr，只做 TLS 握手、拿到对方证书就断开，不发送任何协议消息：
// relay 对「握手完成后不发消息就断开」不计封禁分（TCP 与 QUIC 都是），反复核对也不会把管理员的 IP 封掉。
func fetchFingerprint(ctx context.Context, network, addr string) (string, error) {
	var leaf []byte
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	cfg := &tls.Config{
		// 这里本来就是要看对方出示了什么证书，而不是信任它；比对在调用方做。
		InsecureSkipVerify: true, //nolint:gosec
		ServerName:         host,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("对方没有出示证书")
			}
			leaf = cs.PeerCertificates[0].Raw
			return nil
		},
	}
	switch network {
	case "tcp":
		d := tls.Dialer{NetDialer: &net.Dialer{}, Config: cfg}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return "", err
		}
		conn.Close()
	case "quic":
		qc, err := quic.DialAddr(ctx, addr, quictransport.TLSConfig(cfg), quictransport.ClientConfig())
		if err != nil {
			return "", err
		}
		_ = qc.CloseWithError(0, "")
	default:
		return "", fmt.Errorf("不认识的传输 %q", network)
	}
	if leaf == nil {
		return "", errors.New("没有拿到证书")
	}
	return directory.CertFingerprint(leaf), nil
}
