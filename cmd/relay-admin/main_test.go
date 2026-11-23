package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/quictransport"
)

const (
	primaryPass = "correct horse primary"
	backupPass  = "correct horse backup"
)

type testApp struct {
	*app
	t   *testing.T
	buf *bytes.Buffer
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	buf := &bytes.Buffer{}
	now := time.Now().Truncate(time.Second)
	return &testApp{t: t, buf: buf, app: &app{
		out: buf, errOut: buf,
		now:              func() time.Time { return now },
		dir:              t.TempDir(),
		logN:             minScryptLogN,
		fetchFingerprint: fetchFingerprint,
	}}
}

// exec 用 lines 作为标准输入执行一条命令。
func (ta *testApp) exec(lines []string, args ...string) error {
	ta.in = bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	ta.buf.Reset()
	return ta.run(args)
}

func (ta *testApp) mustExec(lines []string, args ...string) string {
	ta.t.Helper()
	if err := ta.exec(lines, args...); err != nil {
		ta.t.Fatalf("relay-admin %v: %v\noutput:\n%s", args, err, ta.buf)
	}
	return ta.buf.String()
}

func (ta *testApp) keygen() {
	ta.t.Helper()
	ta.mustExec([]string{primaryPass, primaryPass, backupPass, backupPass}, "keygen")
}

func (ta *testApp) roots() directory.Roots {
	ta.t.Helper()
	r, _, err := ta.app.roots(false)
	if err != nil {
		ta.t.Fatal(err)
	}
	return r
}

func (ta *testApp) ledger() *ledger {
	ta.t.Helper()
	l, err := ta.loadLedger()
	if err != nil {
		ta.t.Fatal(err)
	}
	return l
}

func readEnvelope(t *testing.T, path string) directory.Envelope {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env directory.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return env
}

func issued(t *testing.T, ta *testApp, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(ta.issuedDir(), pattern))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestKeygen(t *testing.T) {
	ta := newTestApp(t)
	out := ta.mustExec([]string{primaryPass, primaryPass, backupPass, backupPass}, "keygen")

	for _, name := range []string{directory.SignerPrimary, directory.SignerBackup} {
		pub, err := readPub(ta.pubPath(name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, base64.StdEncoding.EncodeToString(pub)) {
			t.Errorf("output does not show the %s public key", name)
		}
		if !strings.Contains(out, ta.keyPath(name)) {
			t.Errorf("output does not show where the %s key file is", name)
		}
		if runtime.GOOS != "windows" {
			fi, _ := os.Stat(ta.keyPath(name))
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %o", ta.keyPath(name), fi.Mode().Perm())
			}
		}
	}
	data, _ := os.ReadFile(ta.keyPath(directory.SignerPrimary))
	priv, err := openKey(data, directory.SignerPrimary, primaryPass)
	if err != nil {
		t.Fatalf("open primary: %v", err)
	}
	if pub, _ := readPub(ta.pubPath(directory.SignerPrimary)); !pub.Equal(priv.Public()) {
		t.Fatal(".pub does not match the encrypted key")
	}
	if _, err := openKey(data, directory.SignerPrimary, backupPass); !errors.Is(err, errWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	// 把备用根的文件当主根用必须被识破（名字写在 AAD 里）。
	backup, _ := os.ReadFile(ta.keyPath(directory.SignerBackup))
	if _, err := openKey(backup, directory.SignerPrimary, backupPass); err == nil {
		t.Fatal("backup key file accepted as primary")
	}
	if _, err := os.Stat(ta.ledgerPath()); err != nil {
		t.Fatalf("ledger not created: %v", err)
	}

	// 再跑一次必须拒绝，且不动已有文件。
	if err := ta.exec([]string{primaryPass, primaryPass, backupPass, backupPass}, "keygen"); err == nil {
		t.Fatal("second keygen overwrote existing keys")
	}
	again, _ := os.ReadFile(ta.keyPath(directory.SignerPrimary))
	if !bytes.Equal(again, data) {
		t.Fatal("existing key file changed")
	}
}

func TestKeygenPassphraseRules(t *testing.T) {
	ta := newTestApp(t)
	lines := []string{"short", "a long passphrase 1", "a long passphrase 2", "a long passphrase 3", "nope"}
	if err := ta.exec(lines, "keygen"); err == nil {
		t.Fatal("keygen accepted bad passphrases")
	}
	if _, err := os.Stat(ta.keyPath(directory.SignerPrimary)); !os.IsNotExist(err) {
		t.Fatalf("key file written after failed passphrase setup: %v", err)
	}
}

func TestKeyFileTampering(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	data, err := sealKey(directory.SignerPrimary, priv, primaryPass, minScryptLogN)
	if err != nil {
		t.Fatal(err)
	}
	var kf keyFile
	_ = json.Unmarshal(data, &kf)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	mods := map[string]struct {
		mod func(*keyFile)
		// paramErr：必须在派生密钥之前就按参数不合法拒绝，而不是解密失败后才报“口令不对”。
		paramErr bool
	}{
		"swapped public key": {mod: func(k *keyFile) { k.PublicKey = other }},
		"renamed":            {mod: func(k *keyFile) { k.Name = directory.SignerBackup }},
		"huge scrypt cost":   {mod: func(k *keyFile) { k.LogN = 30 }, paramErr: true},
		"weak scrypt cost":   {mod: func(k *keyFile) { k.LogN = minScryptLogN - 1 }, paramErr: true},
		"flipped ciphertext": {mod: func(k *keyFile) { k.Ciphertext[0] ^= 1 }},
		// 口令正确、AAD 也自洽，只是公钥与 seed 不配对：只有解密后的公私钥核对能挡住。
		"consistent but mismatched public key": {mod: func(k *keyFile) {
			k.PublicKey = other
			key, _ := deriveKey(primaryPass, k.Salt, k.LogN)
			aead, _ := chacha20poly1305.NewX(key)
			k.Ciphertext = aead.Seal(nil, k.Nonce, priv.Seed(), keyAAD(k.Name, k.PublicKey))
		}},
	}
	for name, m := range mods {
		k := kf
		k.Ciphertext = slices.Clone(kf.Ciphertext)
		m.mod(&k)
		b, _ := json.Marshal(k)
		_, err := openKey(b, k.Name, primaryPass)
		switch {
		case err == nil:
			t.Errorf("%s: tampered key file opened", name)
		case m.paramErr && errors.Is(err, errWrongPassphrase):
			t.Errorf("%s: rejected only after key derivation (%v), want parameter error", name, err)
		}
	}
	if _, err := sealKey(directory.SignerPrimary, priv, "short", minScryptLogN); err == nil {
		t.Error("sealKey accepted a short passphrase")
	}
}

type jwk struct {
	Kty, Crv, D, X string
}

func TestStatusKeyNew(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	if err := ta.exec([]string{"wrong passphrase!"}, "status-key", "new"); err == nil {
		t.Fatal("status-key new with wrong passphrase succeeded")
	}
	if len(issued(t, ta, "status-key-*")) != 0 {
		t.Fatal("files written despite wrong passphrase")
	}

	out := ta.mustExec([]string{primaryPass}, "status-key", "new")
	certs := issued(t, ta, "status-key-*.json")
	secrets := issued(t, ta, "status-key-*.secret.jwk")
	if len(certs) != 1 || len(secrets) != 1 {
		t.Fatalf("issued certs %v secrets %v", certs, secrets)
	}
	if !strings.Contains(out, "wrangler secret put") || !strings.Contains(out, secrets[0]) {
		t.Fatalf("output lacks upload instructions:\n%s", out)
	}
	env := readEnvelope(t, certs[0])
	if err := directory.CheckRootSignature(env, directory.KindStatusKey, ta.roots()); err != nil {
		t.Fatalf("status key cert: %v", err)
	}
	var c directory.StatusKeyCert
	_ = json.Unmarshal(env.Payload, &c)
	if c.Serial != uint64(ta.now().Unix()) {
		t.Fatalf("serial %d, want current unix time", c.Serial)
	}
	if got := time.Duration(c.NotAfter-c.NotBefore) * time.Second; got != 365*24*time.Hour {
		t.Fatalf("validity %s", got)
	}

	data, _ := os.ReadFile(secrets[0])
	var k jwk
	if err := json.Unmarshal(data, &k); err != nil {
		t.Fatal(err)
	}
	seed, _ := base64.RawURLEncoding.DecodeString(k.D)
	x, _ := base64.RawURLEncoding.DecodeString(k.X)
	if k.Kty != "OKP" || k.Crv != "Ed25519" || len(seed) != ed25519.SeedSize {
		t.Fatalf("jwk = %+v", k)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !pub.Equal(ed25519.PublicKey(c.PubKey)) || !pub.Equal(ed25519.PublicKey(x)) {
		t.Fatal("secret key does not match the authorized public key")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(secrets[0])
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("secret mode %o", fi.Mode().Perm())
		}
	}

	// 同一秒内再签一把，序号也要递增。
	ta.mustExec([]string{primaryPass}, "status-key", "new")
	if l := ta.ledger(); l.LastStatusSerial != c.Serial+1 {
		t.Fatalf("second status key serial %d, want %d", l.LastStatusSerial, c.Serial+1)
	}
}

// fakeRelay 在本机起 TCP+TLS 与 QUIC 监听，出示 cert，握手后立即断开。
type fakeRelay struct {
	tcp, quic string
}

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startFakeRelay(t *testing.T, cert tls.Certificate) fakeRelay {
	t.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
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
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	ql, err := quic.ListenAddr("127.0.0.1:0", quictransport.TLSConfig(cfg), quictransport.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ql.Close() })
	go func() {
		for {
			qc, err := ql.Accept(context.Background())
			if err != nil {
				return
			}
			_ = qc.CloseWithError(0, "")
		}
	}()
	return fakeRelay{tcp: ln.Addr().String(), quic: ql.Addr().String()}
}

func writeRequest(t *testing.T, cert tls.Certificate, r fakeRelay, name string) string {
	t.Helper()
	return writeRenewRequest(t, cert, nil, r, name)
}

// writeRenewRequest 生成换证书申请：prev 是节点当前在用的旧证书，由它共签。prev 为 nil 时就是普通申请。
func writeRenewRequest(t *testing.T, cert tls.Certificate, prev *tls.Certificate, r fakeRelay, name string) string {
	t.Helper()
	info := directory.NodeRequest{Name: name, Operator: "ops", Region: "test", TCP: []string{r.tcp}}
	if r.quic != "" {
		info.QUIC = []string{r.quic}
	}
	raw, err := directory.NewNodeRequest(cert, prev, info)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name+".request.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readNodeCert(t *testing.T, ta *testApp, path string) directory.NodeCert {
	t.Helper()
	env := readEnvelope(t, path)
	if err := directory.CheckRootSignature(env, directory.KindNodeCert, ta.roots()); err != nil {
		t.Fatalf("node cert %s: %v", path, err)
	}
	var c directory.NodeCert
	_ = json.Unmarshal(env.Payload, &c)
	if err := directory.ValidateNodeCert(&c); err != nil {
		t.Fatalf("issued cert invalid: %v", err)
	}
	return c
}

func TestSignNode(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	relay := startFakeRelay(t, cert)
	req := writeRequest(t, cert, relay, "node-k")

	out := ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "k", "--official", req)
	files := issued(t, ta, "node-K-*.json")
	if len(files) != 1 {
		t.Fatalf("issued %v\n%s", files, out)
	}
	c := readNodeCert(t, ta, files[0])
	fp := directory.CertFingerprint(cert.Certificate[0])
	if c.Prefix != "K" || !c.Official || c.Name != "node-k" || !slices.Equal(c.TLSFPs, []string{fp}) {
		t.Fatalf("cert = %+v", c)
	}
	if c.Serial == 0 || c.Serial >= 1<<53 {
		t.Fatalf("serial %d must be in 1..2^53 (JS 安全整数)", c.Serial)
	}
	if got := time.Duration(c.NotAfter-c.NotBefore) * time.Second; got != 365*24*time.Hour {
		t.Fatalf("validity %s", got)
	}
	if !strings.Contains(out, relay.tcp) || !strings.Contains(out, relay.quic) {
		t.Fatalf("output does not report the dial checks:\n%s", out)
	}
	if l := ta.ledger(); len(l.Nodes) != 1 || l.Nodes[0].Prefix != "K" || l.Nodes[0].Serial != c.Serial {
		t.Fatalf("ledger = %+v", l.Nodes)
	}

	// 前缀已被占用。
	cert2 := testCert(t)
	relay2 := startFakeRelay(t, cert2)
	req2 := writeRequest(t, cert2, relay2, "node-2")
	if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "K", req2); err == nil || !strings.Contains(err.Error(), "K") {
		t.Fatalf("duplicate prefix: %v", err)
	}
	// 不指定前缀时分配第一个空闲的；默认不是官方节点、有效期可缩短。
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--valid", "90", req2)
	files = issued(t, ta, "node-A-*.json")
	if len(files) != 1 {
		t.Fatalf("auto prefix: %v", issued(t, ta, "node-*.json"))
	}
	c2 := readNodeCert(t, ta, files[0])
	if c2.Official || time.Duration(c2.NotAfter-c2.NotBefore)*time.Second != 90*24*time.Hour {
		t.Fatalf("second cert = %+v", c2)
	}
}

func TestSignNodeRejectsFingerprintMismatch(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	claimed, served := testCert(t), testCert(t)
	relay := startFakeRelay(t, served)
	req := writeRequest(t, claimed, relay, "node-x")
	// 不提供任何输入：指纹核对必须在确认和输入口令之前就失败。
	err := ta.exec(nil, "sign-node", "--prefix", "X", req)
	if err == nil || !strings.Contains(err.Error(), "指纹") {
		t.Fatalf("mismatch error = %v\n%s", err, ta.buf)
	}
	if len(issued(t, ta, "node-*")) != 0 || len(ta.ledger().Nodes) != 0 {
		t.Fatal("cert issued despite fingerprint mismatch")
	}
}

func TestSignNodeUnreachable(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	req := writeRequest(t, cert, fakeRelay{tcp: dead}, "node-dead")
	if err := ta.exec(nil, "sign-node", req); err == nil {
		t.Fatal("cert issued for an unreachable relay")
	}
}

func TestSignNodeDeclined(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	req := writeRequest(t, cert, startFakeRelay(t, cert), "node-n")
	if err := ta.exec([]string{"no"}, "sign-node", req); err == nil {
		t.Fatal("sign-node proceeded without yes")
	}
	if len(issued(t, ta, "node-*")) != 0 {
		t.Fatal("cert issued after the user declined")
	}
}

// 换证书：relay 还在出示旧证书时，新证书同时列出新旧两个指纹，切换前后客户端都能连。
func TestSignNodeRenew(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	oldCert := testCert(t)
	relay := startFakeRelay(t, oldCert)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", "--official", writeRequest(t, oldCert, relay, "node-r"))

	newCert := testCert(t)
	if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, newCert, relay, "node-r")); err == nil {
		t.Fatal("re-signing an occupied prefix without --renew succeeded")
	}
	req := writeRenewRequest(t, newCert, &oldCert, relay, "node-r") // relay 仍出示旧证书，申请由旧证书共签
	if err := ta.exec(nil, "sign-node", "--prefix", "S", req); err == nil || !strings.Contains(err.Error(), "--renew") {
		t.Fatalf("renewal request without --renew: %v, want a hint to use --renew", err)
	}
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", req)
	oldFP, newFP := directory.CertFingerprint(oldCert.Certificate[0]), directory.CertFingerprint(newCert.Certificate[0])
	fpsOf := func() [][]string {
		var fps [][]string
		for _, f := range issued(t, ta, "node-R-*.json") {
			fps = append(fps, readNodeCert(t, ta, f).TLSFPs)
		}
		return fps
	}
	if fps := fpsOf(); len(fps) != 2 || !slices.ContainsFunc(fps, func(s []string) bool { return slices.Equal(s, []string{newFP, oldFP}) }) {
		t.Fatalf("renewed cert fingerprints %v, want one [new old]", fps)
	}

	// 节点切换到新证书后，用同一份申请再签一次：只含新指纹。
	ta.fetchFingerprint = func(context.Context, string, string) (string, error) { return newFP, nil }
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", req)
	if fps := fpsOf(); len(fps) != 3 || !slices.ContainsFunc(fps, func(s []string) bool { return slices.Equal(s, []string{newFP}) }) {
		t.Fatalf("after switching: fingerprints %v, want one [new]", fps)
	}
	// 证书不变的单纯续期：用现有证书签的普通申请即可。
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", writeRequest(t, newCert, relay, "node-r"))
	if n := len(fpsOf()); n != 4 {
		t.Fatalf("plain renewal: %d certs for R, want 4", n)
	}
}

// 冒名换证书：攻击者照抄受害节点公开的名称和地址，用自己的密钥提交申请并要求 --renew。
// 受害节点还在出示旧证书，旧指纹在台账里——但申请人从没证明自己掌握旧证书私钥，必须拒绝。
func TestSignNodeRenewRejectsImpostor(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	victimCert := testCert(t)
	victim := startFakeRelay(t, victimCert)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, victimCert, victim, "node-r"))

	attacker, attackerOld := testCert(t), testCert(t)
	own := startFakeRelay(t, attacker)
	ownOld := startFakeRelay(t, attackerOld)
	for name, c := range map[string]struct {
		tcp  []string
		prev *tls.Certificate
	}{
		"victim addresses only":           {tcp: []string{victim.tcp}},
		"victim plus attacker's own host": {tcp: []string{victim.tcp, own.tcp}},
		// 地址全换成自己的：连接核对必然通过，只能靠「证明掌握现有证书私钥」挡住。
		"attacker's own host only": {tcp: []string{own.tcp}},
		// 自己造一张「旧证书」来共签：共签只证明掌握那张证书，它不是前缀 R 的。
		"co-signed by attacker's own old cert": {tcp: []string{ownOld.tcp}, prev: &attackerOld},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := directory.NewNodeRequest(attacker, c.prev, directory.NodeRequest{Name: "node-r", Operator: "ops", TCP: c.tcp})
			if err != nil {
				t.Fatal(err)
			}
			req := filepath.Join(t.TempDir(), "impostor.json")
			if err := os.WriteFile(req, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", req); err == nil {
				t.Fatalf("impostor got a certificate for prefix R\n%s", ta.buf)
			}
			if n := len(issued(t, ta, "node-R-*.json")); n != 1 {
				t.Fatalf("%d certs for R, want only the victim's", n)
			}
		})
	}
}

// rotateR 签出前缀 R 的完整换证书流程：[old] → [new, old] → [new]，返回旧、新证书和节点地址。
func rotateR(t *testing.T, ta *testApp) (oldCert, newCert tls.Certificate, relay fakeRelay) {
	t.Helper()
	oldCert, newCert = testCert(t), testCert(t)
	relay = startFakeRelay(t, oldCert)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", "--official", writeRequest(t, oldCert, relay, "node-r"))
	req := writeRenewRequest(t, newCert, &oldCert, relay, "node-r")
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", req)
	saved := ta.fetchFingerprint
	newFP := directory.CertFingerprint(newCert.Certificate[0])
	ta.fetchFingerprint = func(context.Context, string, string) (string, error) { return newFP, nil }
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", req)
	ta.fetchFingerprint = saved
	return oldCert, newCert, relay
}

// 换证书完成后旧证书已被取代：拿到旧私钥的人（退役机器、旧备份）不能再用它证明「掌握该前缀现有证书」，
// 否则就能用 --renew 把前缀挪到自己的地址。
func TestSignNodeRenewRejectsSupersededKey(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	oldCert, _, _ := rotateR(t, ta)
	evilNew := testCert(t)
	for name, c := range map[string]struct {
		cert  tls.Certificate
		prev  *tls.Certificate
		relay fakeRelay
	}{
		"superseded key alone":                 {oldCert, nil, startFakeRelay(t, oldCert)},
		"superseded key co-signing a new cert": {evilNew, &oldCert, startFakeRelay(t, evilNew)},
	} {
		t.Run(name, func(t *testing.T) {
			req := writeRenewRequest(t, c.cert, c.prev, c.relay, "node-r")
			if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", req); err == nil {
				t.Fatalf("superseded key renewed prefix R\n%s", ta.buf)
			}
			if n := len(issued(t, ta, "node-R-*.json")); n != 3 {
				t.Fatalf("%d certs for R, want the 3 from the rotation", n)
			}
		})
	}
}

// 前缀最新的证书被吊销（节点私钥泄露）后，--renew 不再接受任何旧证书私钥作证明：哪张都可能已经泄露。
// 节点要重新接入，先把同前缀仍有效的旧证书也吊销，再走 --prefix 由管理员重新核对。
func TestSignNodeRenewAfterLatestRevoked(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	oldCert, newCert, relay := rotateR(t, ta)
	l := ta.ledger()
	latest := l.Nodes[len(l.Nodes)-1].Serial
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(latest))
	for name, cert := range map[string]tls.Certificate{"old key": oldCert, "new key": newCert} {
		saved := ta.fetchFingerprint
		fp := directory.CertFingerprint(cert.Certificate[0])
		ta.fetchFingerprint = func(context.Context, string, string) (string, error) { return fp, nil }
		err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, cert, relay, "node-r"))
		ta.fetchFingerprint = saved
		if err == nil {
			t.Fatalf("%s renewed R after its latest cert was revoked\n%s", name, ta.buf)
		}
	}
	fresh := testCert(t)
	freshReq := writeRequest(t, fresh, startFakeRelay(t, fresh), "node-r")
	if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", freshReq); err == nil {
		t.Fatal("--prefix R succeeded while older certs for R are still valid")
	}
	var others []string
	for _, n := range ta.ledger().Nodes {
		if n.Serial != latest {
			others = append(others, fmt.Sprint(n.Serial))
		}
	}
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", strings.Join(others, ","))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", freshReq)
}

// 台账里有别的前缀（并且是更晚签发的）时，续签只看自己前缀的最新证书：既不能因此被拒，也不能借别的前缀的证书续签。
func TestSignNodeRenewIgnoresOtherPrefixes(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	certR, certS := testCert(t), testCert(t)
	relayR, relayS := startFakeRelay(t, certR), startFakeRelay(t, certS)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, certR, relayR, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "S", writeRequest(t, certS, relayS, "node-s"))

	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, certR, relayR, "node-r"))
	if n := len(issued(t, ta, "node-R-*.json")); n != 2 {
		t.Fatalf("%d certs for R, want 2", n)
	}
	// S 的证书私钥证明不了 R：把 R 的地址指到 S 的机器上同样不行。
	if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, certS, relayS, "node-r")); err == nil {
		t.Fatalf("a key belonging to prefix S renewed prefix R\n%s", ta.buf)
	}
}

// 续期时地址变了要醒目提示：前缀挪到别的地址是最需要管理员看清的变化。
func TestSignNodeRenewWarnsOnAddressChange(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, cert, startFakeRelay(t, cert), "node-r"))
	moved := startFakeRelay(t, cert)
	out := ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, cert, moved, "node-r"))
	if !strings.Contains(out, "地址有变化") || !strings.Contains(out, "→ [\""+moved.tcp) {
		t.Fatalf("renewal to a new address printed no address warning:\n%s", out)
	}
	// 地址没变的续期不提示。
	out = ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, cert, moved, "node-r"))
	if strings.Contains(out, "地址有变化") {
		t.Fatalf("renewal at the same address warned about an address change:\n%s", out)
	}
}

// inspect <URL> 时服务端的状态行会进错误文本，错误最终打到管理员终端，控制字符必须转义。
func TestInspectURLEscapesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		buf.WriteString("HTTP/1.1 404 x\x1b]0;pwned\x07\r\nContent-Length: 0\r\n\r\n")
		buf.Flush()
		c.Close()
	}))
	defer srv.Close()
	_, err := readSource(srv.URL)
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Fatalf("readSource error = %q, want an error with control characters escaped", err)
	}
}

// main 打印错误前统一转义控制字符：错误可能带着对端或文件里的任意文本。
func TestPrintErrorEscapes(t *testing.T) {
	var buf bytes.Buffer
	printError(&buf, errors.New("bad \x1b]0;pwned\x07 ‮ and\nmore"))
	if s := buf.String(); strings.ContainsAny(s, "\x1b\x07‮") || strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Fatalf("printError wrote %q", s)
	}
}

// 同一张 TLS 证书不能出现在两个前缀上：放进同一份列表时客户端会整份拒绝。
func TestSignNodeRejectsFingerprintUsedByAnotherPrefix(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	req := writeRequest(t, cert, startFakeRelay(t, cert), "node-a")
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "A", req)
	err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "B", req)
	if err == nil || !strings.Contains(err.Error(), "A") {
		t.Fatalf("second prefix for the same certificate: %v", err)
	}
	if len(issued(t, ta, "node-B-*.json")) != 0 {
		t.Fatal("cert issued for prefix B")
	}
}

// 节点（第三方运营者）控制的错误文本不能把控制字符原样带到管理员终端。
func TestSignNodeEscapesDialErrors(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	req := writeRequest(t, cert, fakeRelay{tcp: "relay.example.test:8443"}, "node-e")
	ta.fetchFingerprint = func(context.Context, string, string) (string, error) {
		return "", errors.New("closed by peer: \x1b]0;pwned\a\x1b[2J")
	}
	err := ta.exec([]string{"yes", primaryPass}, "sign-node", req)
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\a") {
		t.Fatalf("dial error = %q, want an error with control characters escaped", err)
	}
}

// inspect 单个节点证书时，证书内容在合规检查之前就会打印，必须全部转义。
func TestInspectEscapesNodeCert(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	data, _ := os.ReadFile(ta.keyPath(directory.SignerPrimary))
	priv, err := openKey(data, directory.SignerPrimary, primaryPass)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(directory.NodeCert{Serial: 1, Prefix: "\x1b[2J\x1b]0;pwned\a", Name: "n", Operator: "o"})
	env, _ := json.Marshal(directory.Sign(priv, directory.KindNodeCert, directory.SignerPrimary, payload))
	path := filepath.Join(t.TempDir(), "cert.json")
	if err := os.WriteFile(path, env, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = ta.exec(nil, "inspect", path)
	if strings.ContainsAny(ta.buf.String(), "\x1b\a") {
		t.Fatalf("inspect printed raw control characters:\n%q", ta.buf.String())
	}
}

func TestRevoke(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", "5,7")
	first := issued(t, ta, "revocations-*.json")
	if len(first) != 1 {
		t.Fatalf("issued %v", first)
	}
	r1 := readRevocations(t, ta, first[0], directory.SignerPrimary)
	if !slices.Equal(r1.NodeSerials, []uint64{5, 7}) || len(r1.Roots) != 0 {
		t.Fatalf("first revocations = %+v", r1)
	}

	// 吊销主根：自动改用备用根签，并包含之前的全部条目。
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	all := issued(t, ta, "revocations-*.json")
	if len(all) != 2 {
		t.Fatalf("issued %v", all)
	}
	var r2 directory.Revocations
	for _, f := range all {
		if f != first[0] {
			r2 = readRevocations(t, ta, f, directory.SignerBackup)
		}
	}
	// 吊销记的是主根公钥的指纹，并声明状态密钥序号下限：此前授权的状态密钥一律作废。
	primaryFP := directory.RootFingerprint(ta.roots().Keys[directory.SignerPrimary])
	if !slices.Equal(r2.NodeSerials, []uint64{5, 7}) || !slices.Equal(r2.Roots, []string{primaryFP}) || r2.Serial <= r1.Serial ||
		r2.MinStatusSerial < uint64(ta.now().Unix()) {
		t.Fatalf("second revocations = %+v (first serial %d, primary %s)", r2, r1.Serial, primaryFP)
	}
	// 之后用备用根授权的状态密钥序号不低于这个下限，客户端才会接受——哪怕签发时本机时钟往回调了。
	saved := ta.now
	ta.now = func() time.Time { return saved().Add(-time.Hour) }
	ta.mustExec([]string{backupPass}, "status-key", "new", "--root", "backup")
	ta.now = saved
	var sk directory.StatusKeyCert
	_ = json.Unmarshal(readEnvelope(t, issued(t, ta, "status-key-*.json")[0]).Payload, &sk)
	if sk.Serial < r2.MinStatusSerial {
		t.Fatalf("status key serial %d below the declared floor %d", sk.Serial, r2.MinStatusSerial)
	}

	// 主根已吊销，不能再用它签任何东西；按台账在确认和要口令之前就明确拒绝。
	if err := ta.exec(nil, "revoke", "--node-serial", "9"); err == nil || !strings.Contains(err.Error(), "已被吊销") {
		t.Fatalf("revoke with revoked primary root: %v, want refusal before any prompt", err)
	}
	if err := ta.exec(nil, "status-key", "new"); err == nil || !strings.Contains(err.Error(), "已被吊销") {
		t.Fatalf("status-key new with revoked primary root: %v, want refusal before any prompt", err)
	}
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--signer", "backup", "--node-serial", "9")
	if l := ta.ledger(); !slices.Equal(l.RevokedNodes, []uint64{5, 7, 9}) {
		t.Fatalf("ledger revoked nodes %v", l.RevokedNodes)
	}
	// 后续吊销列表照样带上根吊销和序号下限：只见过这一份的客户端也要知道。
	for _, f := range issued(t, ta, "revocations-*.json") {
		if r := readRevocations(t, ta, f, readEnvelope(t, f).Signer); slices.Contains(r.NodeSerials, 9) &&
			(!slices.Equal(r.Roots, r2.Roots) || r.MinStatusSerial != r2.MinStatusSerial) {
			t.Fatalf("later revocations dropped the root revocation or floor: %+v", r)
		}
	}
	if err := ta.exec(nil, "revoke"); err == nil {
		t.Fatal("revoke without targets succeeded")
	}
	if err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup", "--signer", "backup"); err == nil {
		t.Fatal("a root revoked itself")
	}
}

// 根吊销只能由备用根吊销主根；备用根泄露只能靠发版内置吊销。
func TestRevokeBackupRootRefused(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	for _, args := range [][]string{
		{"revoke", "--root", "backup"},
		{"revoke", "--root", "backup", "--signer", "primary"},
	} {
		err := ta.exec(nil, args...)
		if err == nil || !strings.Contains(err.Error(), "发版") {
			t.Fatalf("%v: %v, want refusal before any prompt that explains the release route", args, err)
		}
	}
	if len(issued(t, ta, "revocations-*")) != 0 {
		t.Fatal("revocation issued")
	}
}

func readRevocations(t *testing.T, ta *testApp, path, signer string) directory.Revocations {
	t.Helper()
	env := readEnvelope(t, path)
	if env.Signer != signer {
		t.Fatalf("%s signed by %s, want %s", path, env.Signer, signer)
	}
	if err := directory.CheckRootSignature(env, directory.KindRevocations, ta.roots()); err != nil {
		t.Fatal(err)
	}
	var r directory.Revocations
	_ = json.Unmarshal(env.Payload, &r)
	return r
}

// 端到端：keygen → status-key new → sign-node 签出的东西能拼成一份客户端认可的列表，inspect 能逐层验过。
func TestEndToEndDirectory(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{primaryPass}, "status-key", "new")
	cert := testCert(t)
	req := writeRequest(t, cert, startFakeRelay(t, cert), "node-e")
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "E", "--official", req)

	statusCert := readEnvelope(t, issued(t, ta, "status-key-*.json")[0])
	nodeCert := readEnvelope(t, issued(t, ta, "node-E-*.json")[0])
	secret, _ := os.ReadFile(issued(t, ta, "status-key-*.secret.jwk")[0])
	var k jwk
	_ = json.Unmarshal(secret, &k)
	seed, _ := base64.RawURLEncoding.DecodeString(k.D)

	now := ta.now()
	raw, err := directory.SignDirectory(ed25519.NewKeyFromSeed(seed), directory.Body{
		Format: directory.FormatVersion, Serial: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(30 * 24 * time.Hour).Unix(),
		Policy: directory.PolicyPreferDefault, DefaultPrefix: "E", StatusKey: statusCert,
		Nodes: []directory.NodeEntry{{Cert: nodeCert, Status: directory.StatusActive, Weight: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := directory.Verify(raw, ta.roots(), directory.State{}, now)
	if err != nil {
		t.Fatalf("client would reject the assembled list: %v", err)
	}
	if n := v.NodeByFingerprint(directory.CertFingerprint(cert.Certificate[0])); n == nil || n.Prefix != "E" {
		t.Fatalf("node lookup by served fingerprint = %+v", n)
	}

	dirFile := filepath.Join(t.TempDir(), "directory.json")
	_ = os.WriteFile(dirFile, raw, 0o600)
	out := ta.mustExec(nil, "inspect", dirFile)
	for _, want := range []string{"node-e", "E", "prefer-default"} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output lacks %q:\n%s", want, out)
		}
	}
	for _, f := range append(issued(t, ta, "*.json"), req) {
		if strings.HasSuffix(f, ".secret.jwk") {
			continue
		}
		ta.mustExec(nil, "inspect", f)
	}

	// 篡改过的列表 inspect 必须报错。
	tampered := bytes.Replace(raw, []byte(`"sig":"`), []byte(`"sig":"AAAA`), 1)
	_ = os.WriteFile(dirFile, tampered, 0o600)
	if err := ta.exec(nil, "inspect", dirFile); err == nil {
		t.Fatalf("inspect accepted a tampered list:\n%s", ta.buf)
	}
}
