package client

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
)

// Client 记下当前连接上 relay 出示的证书指纹（share 据此判断重连后是不是同一台 relay），TCP、QUIC 都要有。
func TestClientRecordsPeerFingerprint(t *testing.T) {
	setDialTimeout(t, 5*time.Second)
	cert, key := newCertPair(t)
	want := certFP(t, cert, key)
	addr := freePort(t)
	startRelay(t, addr, addr, cert, key)

	for _, transport := range []string{TransportTCP, TransportQUIC} {
		t.Run(transport, func(t *testing.T) {
			c := NewClient(&Config{
				ServerAddr: addr, UseTLS: true, InsecureSkip: true, Transport: transport,
				trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
			})
			t.Cleanup(c.Close)
			if got := c.peerFingerprint(); got != "" {
				t.Fatalf("还没连上就有指纹 %q", got)
			}
			if err := c.Connect(); err != nil {
				t.Fatal(err)
			}
			if c.transport != transport {
				t.Fatalf("前提不成立：连上的传输是 %q", c.transport)
			}
			if got := c.peerFingerprint(); got != want {
				t.Fatalf("指纹 = %q，期望 %q", got, want)
			}
			c.Close()
			if got := c.peerFingerprint(); got != "" {
				t.Fatalf("断开后还留着指纹 %q", got)
			}
		})
	}
}
