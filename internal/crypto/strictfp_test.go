package crypto

import (
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// 严格指纹比对（节点列表里的 relay，spec 6.3「证书校验」）：指纹必须是列表给的几个之一，
// 不走 TOFU、不记录、不放过回环地址。

func TestFPVerifierAcceptsListedFingerprint(t *testing.T) {
	der := newTestCertDER(t)
	other := newTestCertDER(t)
	verify := fpVerifier([]string{CertFingerprint(other), CertFingerprint(der)}, nil)
	if err := verify([][]byte{der}, nil); err != nil {
		t.Fatalf("列表里的指纹应放行: %v", err)
	}
}

func TestFPVerifierIgnoresCase(t *testing.T) {
	der := newTestCertDER(t)
	verify := fpVerifier([]string{strings.ToUpper(CertFingerprint(der))}, nil)
	if err := verify([][]byte{der}, nil); err != nil {
		t.Fatalf("大写写法的指纹应放行: %v", err)
	}
}

func TestFPVerifierRejectsUnlisted(t *testing.T) {
	der := newTestCertDER(t)
	var got []string
	verify := fpVerifier([]string{CertFingerprint(newTestCertDER(t))}, func(fp string) { got = append(got, fp) })
	err := verify([][]byte{der}, nil)
	if !errors.Is(err, ErrCertNotAllowed) {
		t.Fatalf("列表外的证书应返回 ErrCertNotAllowed, got %v", err)
	}
	if len(got) != 1 || got[0] != CertFingerprint(der) {
		t.Errorf("回调应收到本次出示证书的指纹, got %v", got)
	}
}

func TestFPVerifierOnlyChecksLeaf(t *testing.T) {
	leaf, chain := newTestCertDER(t), newTestCertDER(t)
	// 列表里只有链上后面那张：叶子证书才是 relay 的身份，不能靠链里别的证书蒙混过关。
	verify := fpVerifier([]string{CertFingerprint(chain)}, nil)
	if err := verify([][]byte{leaf, chain}, nil); !errors.Is(err, ErrCertNotAllowed) {
		t.Fatalf("只有链上的非叶子证书在列表里时应拒绝, got %v", err)
	}
}

func TestFPVerifierRejectsEmptyChain(t *testing.T) {
	called := false
	verify := fpVerifier([]string{CertFingerprint(newTestCertDER(t))}, func(string) { called = true })
	if err := verify(nil, nil); err == nil {
		t.Fatal("没有出示任何证书必须拒绝")
	}
	if called {
		t.Error("没有证书时没有指纹可报，不应调用回调")
	}
}

// 空列表不是「不限制」：fail closed。
func TestFPVerifierEmptyEntriesFailClosed(t *testing.T) {
	der := newTestCertDER(t)
	verify := fpVerifier([]string{""}, nil)
	if err := verify([][]byte{der}, nil); err == nil {
		t.Fatal("列表里只有空串时不能放行任何证书")
	}
}

func TestNewClientTLSConfigAllowedFPsRejectsCAFile(t *testing.T) {
	_, err := NewClientTLSConfig(ClientTLSOptions{AllowedFPs: []string{"ab"}, CAFile: "ca.pem"})
	if err == nil {
		t.Fatal("--ca 会被指纹校验架空，必须报错")
	}
}

// tlsHandshakeServer 起一个只做握手的 TLS 服务，返回地址和它的证书指纹。
func tlsHandshakeServer(t *testing.T) (addr, fp string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13})
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
			go func() {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
				buf := make([]byte, 1)
				_, _ = c.Read(buf)
			}()
		}
	}()
	return ln.Addr().String(), CertFingerprint(pair.Certificate[0])
}

func handshake(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := tls.Client(conn, cfg)
	return c.Handshake()
}

// 配置层：只给 AllowedFPs（不开 SkipVerify）也能连自签证书的 relay——身份由指纹认定，不走 CA 链。
// 同时给了 TOFU 的那些选项也一样：不写指纹表（钉扎键用非回环地址，TOFU 本会记下它），
// 对回环地址同样严格（TOFU 的回环豁免不适用）。
func TestNewClientTLSConfigAllowedFPs(t *testing.T) {
	addr, fp := tlsHandshakeServer(t)
	store := newTestStore(t)

	cfg, err := NewClientTLSConfig(ClientTLSOptions{AllowedFPs: []string{fp}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientSessionCache != nil {
		t.Error("严格比对不能开会话恢复：恢复时 crypto/tls 不调 VerifyPeerCertificate，指纹就不比对了")
	}
	if err := handshake(t, addr, cfg); err != nil {
		t.Fatalf("指纹在列表里应握手成功: %v", err)
	}

	const pinAddr = "relay.example.test:8443"
	cfg, err = NewClientTLSConfig(ClientTLSOptions{
		AllowedFPs: []string{fp}, SkipVerify: true, PinAddr: pinAddr, TrustStore: store, TrustNewCert: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, addr, cfg); err != nil {
		t.Fatalf("指纹在列表里应握手成功: %v", err)
	}
	if _, ok := store.Lookup(pinAddr); ok {
		t.Error("严格比对不是 TOFU，不该往指纹表里记东西")
	}

	var mismatched string
	cfg, err = NewClientTLSConfig(ClientTLSOptions{
		AllowedFPs: []string{CertFingerprint(newTestCertDER(t))}, SkipVerify: true, PinAddr: addr, TrustStore: store, TrustNewCert: true,
		OnFPMismatch: func(fp string) { mismatched = fp },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, addr, cfg); err == nil {
		t.Fatal("指纹不在列表里必须握手失败（回环地址也一样）")
	}
	if mismatched != fp {
		t.Errorf("OnFPMismatch 收到 %q, want %q", mismatched, fp)
	}
	if _, ok := store.Lookup(addr); ok {
		t.Error("握手失败也不该往指纹表里记东西")
	}
}

// 没给 AllowedFPs 时行为不变：回环豁免钉扎、没有地址不钉。
func TestNewClientTLSConfigWithoutAllowedFPsUnchanged(t *testing.T) {
	cfg, err := NewClientTLSConfig(ClientTLSOptions{SkipVerify: true, PinAddr: "relay.example:8443", TrustStore: newTestStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VerifyPeerCertificate == nil || !cfg.InsecureSkipVerify {
		t.Fatal("原有的 TOFU 配置被改动了")
	}
	cfg, err = NewClientTLSConfig(ClientTLSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VerifyPeerCertificate != nil || cfg.InsecureSkipVerify {
		t.Fatal("默认配置应走 PKI 校验、不挂回调")
	}
}
