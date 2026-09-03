package crypto

import (
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore 返回一个指向临时目录的指纹表，避免测试污染用户主目录。
func newTestStore(t *testing.T) *TrustStore {
	t.Helper()
	return &TrustStore{Path: filepath.Join(t.TempDir(), KnownHostsFileName)}
}

// newTestCertDER 生成一张自签证书并返回它的 DER。每次调用都是一张全新的证书，
// 因此两次调用的指纹必然不同——正好用来模拟「证书换了」。
func newTestCertDER(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	return pair.Certificate[0]
}

func TestPinLearnsOnFirstUse(t *testing.T) {
	store := newTestStore(t)
	der := newTestCertDER(t)

	var gotResult PinResult = -1
	var gotFP string
	verify := pinVerifier("relay.example:8443", store, false, func(r PinResult, fp string) {
		gotResult, gotFP = r, fp
	})

	if err := verify([][]byte{der}, nil); err != nil {
		t.Fatalf("首次连接不应报错: %v", err)
	}
	if gotResult != PinLearned {
		t.Errorf("PinResult = %v, want PinLearned", gotResult)
	}
	want := CertFingerprint(der)
	if gotFP != want {
		t.Errorf("回调指纹 = %q, want %q", gotFP, want)
	}
	if fp, ok := store.Lookup("relay.example:8443"); !ok || fp != want {
		t.Errorf("Lookup = (%q, %v), want (%q, true)", fp, ok, want)
	}
}

func TestPinMatchesSilently(t *testing.T) {
	store := newTestStore(t)
	der := newTestCertDER(t)
	if err := store.Put("relay.example:8443", CertFingerprint(der)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var gotResult PinResult = -1
	verify := pinVerifier("relay.example:8443", store, false, func(r PinResult, _ string) {
		gotResult = r
	})
	if err := verify([][]byte{der}, nil); err != nil {
		t.Fatalf("指纹一致不应报错: %v", err)
	}
	if gotResult != PinMatched {
		t.Errorf("PinResult = %v, want PinMatched", gotResult)
	}
}

func TestPinRejectsChangedCert(t *testing.T) {
	store := newTestStore(t)
	oldDER := newTestCertDER(t)
	newDER := newTestCertDER(t)
	oldFP := CertFingerprint(oldDER)
	if err := store.Put("relay.example:8443", oldFP); err != nil {
		t.Fatalf("Put: %v", err)
	}

	verify := pinVerifier("relay.example:8443", store, false, func(PinResult, string) {
		t.Error("指纹不一致时不应触发结果回调")
	})
	err := verify([][]byte{newDER}, nil)
	if err == nil {
		t.Fatal("指纹变化必须拒绝连接，却放行了")
	}
	if !errors.Is(err, ErrCertChanged) {
		t.Errorf("errors.Is(err, ErrCertChanged) = false, err = %v", err)
	}
	// 报错里要同时给出新旧指纹和自救办法，否则用户只能干瞪眼。
	for _, want := range []string{oldFP, CertFingerprint(newDER), "--trust-new-cert"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q: %v", want, err)
		}
	}
	// 拒绝之后记录必须原样保留，不能被这次握手悄悄改写。
	if fp, _ := store.Lookup("relay.example:8443"); fp != oldFP {
		t.Errorf("被拒绝的握手改写了记录: %q, want %q", fp, oldFP)
	}
}

func TestPinReplacesWithTrustNewCert(t *testing.T) {
	store := newTestStore(t)
	oldDER := newTestCertDER(t)
	newDER := newTestCertDER(t)
	if err := store.Put("relay.example:8443", CertFingerprint(oldDER)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var gotResult PinResult = -1
	verify := pinVerifier("relay.example:8443", store, true, func(r PinResult, _ string) {
		gotResult = r
	})
	if err := verify([][]byte{newDER}, nil); err != nil {
		t.Fatalf("--trust-new-cert 下不应报错: %v", err)
	}
	if gotResult != PinReplaced {
		t.Errorf("PinResult = %v, want PinReplaced", gotResult)
	}
	if fp, _ := store.Lookup("relay.example:8443"); fp != CertFingerprint(newDER) {
		t.Errorf("记录未被更新: %q", fp)
	}
}

func TestPinRejectsEmptyCertChain(t *testing.T) {
	verify := pinVerifier("relay.example:8443", newTestStore(t), false, nil)
	if err := verify(nil, nil); err == nil {
		t.Fatal("对端没出示证书时必须报错")
	}
}

func TestPinIsolatesByAddr(t *testing.T) {
	store := newTestStore(t)
	derA := newTestCertDER(t)
	derB := newTestCertDER(t)

	if err := pinVerifier("a.example:8443", store, false, nil)([][]byte{derA}, nil); err != nil {
		t.Fatalf("a 首次连接: %v", err)
	}
	// 另一个地址用另一张证书，不该被 a 的记录干扰。
	if err := pinVerifier("b.example:8443", store, false, nil)([][]byte{derB}, nil); err != nil {
		t.Fatalf("b 首次连接: %v", err)
	}
	if fp, _ := store.Lookup("a.example:8443"); fp != CertFingerprint(derA) {
		t.Errorf("写 b 的记录时冲掉了 a")
	}
}

func TestTrustStoreSkipsMalformedLines(t *testing.T) {
	store := newTestStore(t)
	content := "# 注释\n\n  \nrelay.example:8443 ABCDEF\n只有一列\n三 个 字 段\n"
	if err := os.WriteFile(store.Path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fp, ok := store.Lookup("relay.example:8443")
	if !ok {
		t.Fatal("坏行不应让整张表作废")
	}
	if fp != "abcdef" {
		t.Errorf("指纹应归一化为小写, got %q", fp)
	}
}

// TestTrustStoreZeroPathDegrades 拿不到主目录时钉扎降级为不生效，而不是拒绝连接。
func TestTrustStoreZeroPathDegrades(t *testing.T) {
	store := &TrustStore{}
	if _, ok := store.Lookup("relay.example:8443"); ok {
		t.Error("空 Path 的 store 不该报告有记录")
	}
	if err := store.Put("relay.example:8443", "abc"); err != nil {
		t.Errorf("空 Path 的 Put 应为空操作, got %v", err)
	}
	if err := pinVerifier("relay.example:8443", store, false, nil)([][]byte{newTestCertDER(t)}, nil); err != nil {
		t.Errorf("钉扎降级时必须放行, got %v", err)
	}
}

func TestNewClientTLSConfigPinningScope(t *testing.T) {
	cases := []struct {
		name    string
		opts    ClientTLSOptions
		wantPin bool
	}{
		{"公网地址启用钉扎", ClientTLSOptions{SkipVerify: true, PinAddr: "1.2.3.4:8443"}, true},
		{"域名启用钉扎", ClientTLSOptions{SkipVerify: true, PinAddr: "relay.example:8443"}, true},
		{"IPv4 回环豁免", ClientTLSOptions{SkipVerify: true, PinAddr: "127.0.0.1:8443"}, false},
		{"IPv6 回环豁免", ClientTLSOptions{SkipVerify: true, PinAddr: "[::1]:8443"}, false},
		{"localhost 豁免", ClientTLSOptions{SkipVerify: true, PinAddr: "LocalHost:8443"}, false},
		{"走 PKI 校验时不钉扎", ClientTLSOptions{SkipVerify: false, PinAddr: "1.2.3.4:8443"}, false},
		{"没有地址就没法钉", ClientTLSOptions{SkipVerify: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.TrustStore = newTestStore(t)
			cfg, err := NewClientTLSConfig(tc.opts)
			if err != nil {
				t.Fatalf("NewClientTLSConfig: %v", err)
			}
			if got := cfg.VerifyPeerCertificate != nil; got != tc.wantPin {
				t.Errorf("启用钉扎 = %v, want %v", got, tc.wantPin)
			}
		})
	}
}

// TestNewClientTLSConfigCAAndInsecureFailClosed 守住既有约定：--ca 会被 --insecure
// 静默架空，必须报错而不是装作生效。
func TestNewClientTLSConfigCAAndInsecureFailClosed(t *testing.T) {
	_, err := NewClientTLSConfig(ClientTLSOptions{SkipVerify: true, CAFile: "ca.pem"})
	if err == nil {
		t.Fatal("--ca 与 --insecure 同时给出时必须报错")
	}
}

func TestLoadOrCreateSelfSignedCertReusesExisting(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")

	fresh, err := LoadOrCreateSelfSignedCert(certFile, keyFile)
	if err != nil {
		t.Fatalf("首次生成: %v", err)
	}
	if !fresh {
		t.Fatal("首次调用应生成新证书")
	}
	first, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	// 第二次必须原样复用：standalone 每次启动换证书正是 TOFU 钉扎无法成立的根因。
	fresh, err = LoadOrCreateSelfSignedCert(certFile, keyFile)
	if err != nil {
		t.Fatalf("复用: %v", err)
	}
	if fresh {
		t.Error("已有有效证书时不应重新生成")
	}
	second, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	if CertFingerprint(first.Certificate[0]) != CertFingerprint(second.Certificate[0]) {
		t.Error("复用后指纹变了，钉扎会在下一次连接时误报")
	}
}

func TestLoadOrCreateSelfSignedCertRegeneratesBrokenPair(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, []byte("not a cert"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fresh, err := LoadOrCreateSelfSignedCert(certFile, keyFile)
	if err != nil {
		t.Fatalf("损坏的证书应被重新生成而不是报错: %v", err)
	}
	if !fresh {
		t.Error("损坏的证书对应当重新生成")
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Errorf("重新生成后仍不可用: %v", err)
	}
}
