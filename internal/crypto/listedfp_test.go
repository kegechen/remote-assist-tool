package crypto

import (
	"errors"
	"strings"
	"testing"
)

// 自动模式的进程里指定了地址的连接（MCP connect 带 server，spec 6.1）：证书在节点列表里就按列表放行，
// 不查也不写指纹表；不在列表里照旧 TOFU。

// listedConfig 是走 TOFU（钉扎键用非回环地址，TOFU 本会查、写它）、另给了 listed 的配置。
func listedConfig(t *testing.T, store *TrustStore, pinAddr string, listed ...string) ClientTLSOptions {
	t.Helper()
	return ClientTLSOptions{SkipVerify: true, PinAddr: pinAddr, TrustStore: store, ListedFPs: listed}
}

func TestListedFPsBypassTOFU(t *testing.T) {
	addr, fp := tlsHandshakeServer(t)
	const pinAddr = "relay.example.test:8443"

	t.Run("首次连接不记指纹", func(t *testing.T) {
		store := newTestStore(t)
		var pinned []PinResult
		o := listedConfig(t, store, pinAddr, CertFingerprint(newTestCertDER(t)), fp)
		o.OnPin = func(r PinResult, _ string) { pinned = append(pinned, r) }
		cfg, err := NewClientTLSConfig(o)
		if err != nil {
			t.Fatal(err)
		}
		if err := handshake(t, addr, cfg); err != nil {
			t.Fatalf("证书在列表里应握手成功: %v", err)
		}
		if _, ok := store.Lookup(pinAddr); ok {
			t.Error("按列表放行的不是 TOFU，不该往指纹表里记")
		}
		if len(pinned) != 0 {
			t.Errorf("按列表放行时不该报钉扎结果: %v", pinned)
		}
	})

	t.Run("指纹表里记的是别的证书", func(t *testing.T) {
		store := newTestStore(t)
		old := CertFingerprint(newTestCertDER(t))
		if err := store.Put(pinAddr, old); err != nil {
			t.Fatal(err)
		}
		cfg, err := NewClientTLSConfig(listedConfig(t, store, pinAddr))
		if err != nil {
			t.Fatal(err)
		}
		if err := handshake(t, addr, cfg); !errors.Is(err, ErrCertChanged) {
			t.Fatalf("前提不成立：没给列表时应按 TOFU 报证书变了, got %v", err)
		}
		cfg, err = NewClientTLSConfig(listedConfig(t, store, pinAddr, strings.ToUpper(fp)))
		if err != nil {
			t.Fatal(err)
		}
		if err := handshake(t, addr, cfg); err != nil {
			t.Fatalf("证书在列表里（大写写法）就按列表放行，不看指纹表: %v", err)
		}
		if got, _ := store.Lookup(pinAddr); got != old {
			t.Errorf("指纹表被改成了 %q，应保持 %q", got, old)
		}
	})

	t.Run("不在列表里照旧 TOFU", func(t *testing.T) {
		store := newTestStore(t)
		cfg, err := NewClientTLSConfig(listedConfig(t, store, pinAddr, CertFingerprint(newTestCertDER(t)), ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := handshake(t, addr, cfg); err != nil {
			t.Fatalf("首次连接应按 TOFU 记下: %v", err)
		}
		if got, _ := store.Lookup(pinAddr); got != fp {
			t.Fatalf("TOFU 应记下 %q, got %q", fp, got)
		}
		other := newTestStore(t)
		if err := other.Put(pinAddr, CertFingerprint(newTestCertDER(t))); err != nil {
			t.Fatal(err)
		}
		cfg, err = NewClientTLSConfig(listedConfig(t, other, pinAddr, CertFingerprint(newTestCertDER(t))))
		if err != nil {
			t.Fatal(err)
		}
		if err := handshake(t, addr, cfg); !errors.Is(err, ErrCertChanged) {
			t.Fatalf("不在列表里、与指纹表不符时应照旧拒绝, got %v", err)
		}
	})
}

// 不走 TOFU 时 ListedFPs 不起作用：PKI 校验照旧（不挂回调），严格比对（AllowedFPs）照旧只认 AllowedFPs。
func TestListedFPsOnlyWithTOFU(t *testing.T) {
	addr, fp := tlsHandshakeServer(t)
	cfg, err := NewClientTLSConfig(ClientTLSOptions{ListedFPs: []string{fp}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VerifyPeerCertificate != nil || cfg.InsecureSkipVerify {
		t.Fatal("PKI 校验不该因为 ListedFPs 改动")
	}
	cfg, err = NewClientTLSConfig(ClientTLSOptions{AllowedFPs: []string{CertFingerprint(newTestCertDER(t))}, ListedFPs: []string{fp}})
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, addr, cfg); !errors.Is(err, ErrCertNotAllowed) {
		t.Fatalf("严格比对只认 AllowedFPs, got %v", err)
	}
}
