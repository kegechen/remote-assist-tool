package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

// hookReader 在第一次被读取时先执行 hook，用来在命令「已把台账读进内存、还没写回」的时刻插入一次变动：
// 三个改台账的命令都是先读台账，之后才读标准输入（确认或输口令），最后才写台账。
type hookReader struct {
	r    io.Reader
	hook func()
	once sync.Once
}

func (h *hookReader) Read(p []byte) (int, error) {
	h.once.Do(h.hook)
	return h.r.Read(p)
}

func (ta *testApp) execWithHook(hook func(), lines []string, args ...string) error {
	ta.in = bufio.NewReader(&hookReader{r: strings.NewReader(strings.Join(lines, "\n") + "\n"), hook: hook})
	ta.buf.Reset()
	return ta.run(args)
}

// 台账写不进去时，命令不能留下一份「已签出但台账不知道」的文件：吊销没记进台账，
// 下一份吊销列表就会漏掉它，被吊销的证书还能续签；签出的证书没记进台账，前缀可能被重复分配。
func TestLedgerWriteFailureIssuesNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
		args  func(t *testing.T) []string
		files []string
	}{
		{"revoke", []string{"yes", primaryPass}, func(*testing.T) []string { return []string{"revoke", "--node-serial", "9"} },
			[]string{"revocations-*"}},
		{"status-key new", []string{primaryPass}, func(*testing.T) []string { return []string{"status-key", "new"} },
			[]string{"status-key-*"}},
		{"sign-node", []string{"yes", primaryPass}, func(t *testing.T) []string {
			cert := testCert(t)
			return []string{"sign-node", "--prefix", "R", writeRequest(t, cert, startFakeRelay(t, cert), "node-r")}
		}, []string{"node-R-*"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ta := newTestApp(t)
			ta.keygen()
			before, err := os.ReadFile(ta.ledgerPath())
			if err != nil {
				t.Fatal(err)
			}
			// 把台账文件换成目录：写回（rename）必然失败。
			broken := func() {
				_ = os.Remove(ta.ledgerPath())
				_ = os.Mkdir(ta.ledgerPath(), 0o700)
			}
			err = ta.execWithHook(broken, c.lines, c.args(t)...)
			_ = os.Remove(ta.ledgerPath())
			if werr := os.WriteFile(ta.ledgerPath(), before, 0o600); werr != nil {
				t.Fatal(werr)
			}
			if err == nil || !strings.Contains(err.Error(), "未签发") {
				t.Fatalf("error = %v, want one saying nothing was issued", err)
			}
			for _, g := range c.files {
				if left := issued(t, ta, g); len(left) != 0 {
					t.Fatalf("files left behind after the ledger write failed: %v", left)
				}
			}
			if l := ta.ledger(); len(l.Nodes) != 0 || len(l.RevokedNodes) != 0 || l.LastStatusSerial != 0 || l.LastRevocationSerial != 0 {
				t.Fatalf("ledger changed: %+v", l)
			}
		})
	}
}

// 两个 relay-admin 同时运行（例如一个停在确认提示上）时，后写的不能用旧内容覆盖先写的：
// 否则先写的那次吊销就从台账里消失了。
func TestLedgerConcurrentChangeIsNotOverwritten(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	other := func() {
		l, err := ta.loadLedger()
		if err != nil {
			t.Error(err)
			return
		}
		l.RevokedNodes = []uint64{5}
		if err := ta.saveLedger(l); err != nil {
			t.Error(err)
		}
	}
	err := ta.execWithHook(other, []string{"yes", primaryPass}, "revoke", "--node-serial", "9")
	if err == nil || !strings.Contains(err.Error(), "台账") {
		t.Fatalf("error = %v, want a refusal explaining the ledger changed underneath", err)
	}
	if l := ta.ledger(); !slices.Equal(l.RevokedNodes, []uint64{5}) {
		t.Fatalf("ledger revoked nodes = %v, want the other process's [5] untouched", l.RevokedNodes)
	}
	if left := issued(t, ta, "revocations-*"); len(left) != 0 {
		t.Fatalf("revocation list issued although it could not be recorded: %v", left)
	}
	// 重新运行即可。
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", "9")
	if l := ta.ledger(); !slices.Equal(l.RevokedNodes, []uint64{5, 9}) {
		t.Fatalf("after rerun: %v", l.RevokedNodes)
	}
}

// 命令执行期间台账被删（例如正在把管理目录挪去备份）：不能凭内存里的旧内容另起一份台账。
func TestLedgerRemovedDuringCommand(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	err := ta.execWithHook(func() { _ = os.Remove(ta.ledgerPath()) }, []string{"yes", primaryPass}, "revoke", "--node-serial", "9")
	if err == nil || !strings.Contains(err.Error(), "台账") {
		t.Fatalf("error = %v, want a refusal explaining the ledger disappeared", err)
	}
	if _, err := os.Stat(ta.ledgerPath()); !os.IsNotExist(err) {
		t.Fatalf("ledger recreated from stale memory: %v", err)
	}
	if left := issued(t, ta, "revocations-*"); len(left) != 0 {
		t.Fatalf("revocation list issued although it could not be recorded: %v", left)
	}
}

// 同一份台账连续写两次（自己刚写的不算「别人改过」）。
func TestLedgerSaveTwice(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	l, err := ta.loadLedger()
	if err != nil {
		t.Fatal(err)
	}
	l.RevokedNodes = []uint64{5}
	if err := ta.saveLedger(l); err != nil {
		t.Fatal(err)
	}
	l.RevokedNodes = []uint64{5, 6}
	if err := ta.saveLedger(l); err != nil {
		t.Fatalf("second save of the same ledger: %v", err)
	}
	if got := ta.ledger().RevokedNodes; !slices.Equal(got, []uint64{5, 6}) {
		t.Fatalf("ledger = %v", got)
	}
}

// 吊销意味着那把私钥不再可信。被吊销证书里的指纹：不能原样再签，也不能当作「掌握现有证书」的证明。
func TestSignNodeRefusesFingerprintOfRevokedCert(t *testing.T) {
	t.Run("same certificate signed again after revocation", func(t *testing.T) {
		ta := newTestApp(t)
		ta.keygen()
		cert := testCert(t)
		req := writeRequest(t, cert, startFakeRelay(t, cert), "node-r")
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", req)
		serial := ta.ledger().Nodes[0].Serial
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(serial))
		err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", req)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(serial)) {
			t.Fatalf("re-signing the revoked certificate: %v, want a refusal naming serial %d", err, serial)
		}
		if n := len(issued(t, ta, "node-R-*.json")); n != 1 {
			t.Fatalf("%d certs for R, want only the revoked original", n)
		}
	})

	t.Run("old cert revoked during rotation", func(t *testing.T) {
		ta := newTestApp(t)
		ta.keygen()
		oldCert, newCert := testCert(t), testCert(t)
		relay := startFakeRelay(t, oldCert)
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", "--official", writeRequest(t, oldCert, relay, "node-r"))
		coSigned := writeRenewRequest(t, newCert, &oldCert, relay, "node-r")
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", coSigned) // 最新证书含 [new, old]
		l := ta.ledger()
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(l.Nodes[0].Serial))
		// 轮换证书也信任旧私钥，随之吊销。
		if got := ta.ledger().RevokedNodes; !slices.Equal(got, sortedUnion([]uint64{l.Nodes[0].Serial, l.Nodes[1].Serial}, nil)) {
			t.Fatalf("revoked %v, want both certs that trust the old key", got)
		}

		// 旧私钥（疑似泄露）既不能单独续期，也不能给新证书共签。
		for name, req := range map[string]string{
			"old key alone":          writeRequest(t, oldCert, relay, "node-r"),
			"old key co-signing new": coSigned,
		} {
			if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", req); err == nil {
				t.Fatalf("%s: renewed after the old cert was revoked\n%s", name, ta.buf)
			}
		}
		if n := len(issued(t, ta, "node-R-*.json")); n != 2 {
			t.Fatalf("%d certs for R, want the original two", n)
		}

		// 前缀已经空出；新私钥没有被宣告泄露（只是跟着轮换证书被连带吊销），节点切到新证书后可以重新入网。
		newFP := directory.CertFingerprint(newCert.Certificate[0])
		ta.fetchFingerprint = func(context.Context, string, string) (string, error) { return newFP, nil }
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", "--official", writeRequest(t, newCert, relay, "node-r"))
		if n := len(issued(t, ta, "node-R-*.json")); n != 3 {
			t.Fatalf("%d certs for R, want 3", n)
		}
	})
}

// reissue 用同一把私钥另签一张证书：证书序号、有效期不同，指纹（证书 DER 的哈希）也就不同。
func reissue(t *testing.T, c tls.Certificate) tls.Certificate {
	t.Helper()
	key := c.PrivateKey.(*ecdsa.PrivateKey)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "relay"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(2 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// skewSPKI 把证书里公钥 BIT STRING 的填充位数改成 n、密钥字节整体左移 n 位：长度不变，x509 解析出来还是
// 同一把公钥（解析时按填充位数右移还原），但公钥字段的原始字节和整张证书的指纹都变了。
// 证书的自签名随之失效，但没有谁检查它：拨号核对只比指纹，客户端也只比指纹。
func skewSPKI(t *testing.T, c tls.Certificate, n uint) tls.Certificate {
	t.Helper()
	der := c.Certificate[0]
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	spki := leaf.RawSubjectPublicKeyInfo
	// P-256 公钥：SPKI 以 BIT STRING 03 42 00 04 || X || Y 结尾（65 字节未压缩点）。
	bs := len(spki) - 68
	if bs < 0 || spki[bs] != 0x03 || spki[bs+1] != 0x42 || spki[bs+2] != 0 || spki[bs+3] != 0x04 {
		t.Fatalf("unexpected SPKI layout % x", spki)
	}
	skewed := slices.Clone(spki)
	key := skewed[bs+3:]
	for i := range key {
		key[i] <<= n
		if i+1 < len(key) {
			key[i] |= key[i+1] >> (8 - n)
		}
	}
	skewed[bs+2] = byte(n)
	at := bytes.Index(der, spki)
	if at < 0 {
		t.Fatal("SPKI not found in certificate")
	}
	out := slices.Clone(der)
	copy(out[at:], skewed)
	got, err := x509.ParseCertificate(out)
	if err != nil {
		t.Fatalf("skewed certificate does not parse: %v", err)
	}
	if !got.PublicKey.(*ecdsa.PublicKey).Equal(leaf.PublicKey) || bytes.Equal(got.RawSubjectPublicKeyInfo, spki) {
		t.Fatal("skewing did not keep the key while changing its encoding")
	}
	return tls.Certificate{Certificate: [][]byte{out}, PrivateKey: c.PrivateKey}
}

// 吊销认的是那把私钥，不是证书文件：用同一把私钥另签一张证书（指纹不同）也绕不过去。
func TestSignNodeRefusesKeyOfRevokedCert(t *testing.T) {
	t.Run("re-encoded public key", func(t *testing.T) {
		ta := newTestApp(t)
		ta.keygen()
		cert := testCert(t)
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, cert, startFakeRelay(t, cert), "node-r"))
		serial := ta.ledger().Nodes[0].Serial
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(serial))

		for _, n := range []uint{1, 5} {
			skewed := skewSPKI(t, cert, n)
			err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "S", writeRequest(t, skewed, startFakeRelay(t, skewed), "node-s"))
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(serial)) {
				t.Fatalf("padding %d: %v, want a refusal naming serial %d\n%s", n, err, serial, ta.buf)
			}
		}
	})

	t.Run("new prefix", func(t *testing.T) {
		ta := newTestApp(t)
		ta.keygen()
		cert := testCert(t)
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, cert, startFakeRelay(t, cert), "node-r"))
		serial := ta.ledger().Nodes[0].Serial
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(serial))

		same := reissue(t, cert)
		err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "S", writeRequest(t, same, startFakeRelay(t, same), "node-r"))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(serial)) {
			t.Fatalf("signing a reissued cert of a revoked key: %v, want a refusal naming serial %d\n%s", err, serial, ta.buf)
		}
	})

	t.Run("still refused after later revocations", func(t *testing.T) {
		// 泄露的私钥是永久记录：之后再吊销别的证书，不能把它从记录里冲掉。
		ta := newTestApp(t)
		ta.keygen()
		cert, other := testCert(t), testCert(t)
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, cert, startFakeRelay(t, cert), "node-r"))
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "Q", writeRequest(t, other, startFakeRelay(t, other), "node-q"))
		serial, otherSerial := ta.ledger().Nodes[0].Serial, ta.ledger().Nodes[1].Serial
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(serial))
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(otherSerial))

		same := reissue(t, cert)
		err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "S", writeRequest(t, same, startFakeRelay(t, same), "node-r"))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(serial)) {
			t.Fatalf("key revoked before another revocation: %v, want a refusal naming serial %d\n%s", err, serial, ta.buf)
		}
	})

	t.Run("renewal", func(t *testing.T) {
		// 节点用同一把私钥续了证书（X → X'），换证书时签出含 [X', X] 的证书；之后吊销只含 X 的第一张。
		ta := newTestApp(t)
		ta.keygen()
		x := testCert(t)
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, x, startFakeRelay(t, x), "node-r"))
		first := ta.ledger().Nodes[0].Serial
		x2 := reissue(t, x)
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRenewRequest(t, x2, &x, startFakeRelay(t, x), "node-r"))
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(first))

		// X' 用的就是被吊销的那把私钥：不能单独续期，也不能给新证书共签。
		relay2 := startFakeRelay(t, x2)
		y := testCert(t)
		for name, req := range map[string]string{
			"same key alone":      writeRequest(t, x2, relay2, "node-r"),
			"same key co-signing": writeRenewRequest(t, y, &x2, relay2, "node-r"),
		} {
			err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", req)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(first)) {
				t.Fatalf("%s: %v, want a refusal naming serial %d\n%s", name, err, first, ta.buf)
			}
		}
	})

	t.Run("both keys of a rotation cert", func(t *testing.T) {
		// 换证书期间签出的证书含新旧两把公钥；吊销它，两把私钥都作废。
		ta := newTestApp(t)
		ta.keygen()
		oldCert, newCert := testCert(t), testCert(t)
		relay := startFakeRelay(t, oldCert)
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, oldCert, relay, "node-r"))
		ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRenewRequest(t, newCert, &oldCert, relay, "node-r"))
		rotation := ta.ledger().Nodes[1].Serial
		ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(rotation))

		for name, c := range map[string]tls.Certificate{"old key": oldCert, "new key": newCert} {
			same := reissue(t, c)
			err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "S", writeRequest(t, same, startFakeRelay(t, same), "node-s"))
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(rotation)) {
				t.Fatalf("%s: %v, want a refusal naming serial %d\n%s", name, err, rotation, ta.buf)
			}
		}
	})
}

// 吊销一张证书就是宣告它的私钥泄露：仍信任这把私钥的其他有效证书必须一并吊销，
// 否则状态密钥或 CF 账号被盗时，把那张证书重新挂上，配上泄露的私钥照样能冒充节点。
func TestRevokeCoversOtherCertsWithSameKey(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	oldCert, newCert := testCert(t), testCert(t)
	oldRelay, newRelay := startFakeRelay(t, oldCert), startFakeRelay(t, newCert)
	// 一次完整的换证书：cert1 [old] → cert2 [new, old] → cert3 [new]。
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, oldCert, oldRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRenewRequest(t, newCert, &oldCert, oldRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, newCert, newRelay, "node-r"))
	l := ta.ledger()
	cert1, cert2, cert3 := l.Nodes[0].Serial, l.Nodes[1].Serial, l.Nodes[2].Serial

	// 新私钥泄露，管理员只给了现用证书的序号。
	out := ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(cert3))
	if !strings.Contains(out, fmt.Sprintf("证书 %d（", cert2)) {
		t.Fatalf("output does not show that cert %d (which also trusts the new key) is revoked too:\n%s", cert2, out)
	}
	if strings.Contains(out, fmt.Sprintf("证书 %d（", cert3)) {
		t.Fatalf("output lists the named cert %d as if it were pulled in:\n%s", cert3, out)
	}
	want := sortedUnion([]uint64{cert2, cert3}, nil)
	if got := ta.ledger().RevokedNodes; !slices.Equal(got, want) {
		t.Fatalf("ledger revoked %v, want %v (cert %d trusts only the old key and stays)", got, want, cert1)
	}
	if r := readRevocations(t, ta, issued(t, ta, "revocations-*.json")[0], directory.SignerPrimary); !slices.Equal(r.NodeSerials, want) {
		t.Fatalf("revocation list has %v, want %v", r.NodeSerials, want)
	}
}

// 同一把私钥不能用在两个前缀上（另签一张证书、指纹不同也不行）：吊销其中一个时另一个会被连带，
// 而且持有这把私钥的一方能同时冒充两个节点。
func TestSignNodeRejectsKeyUsedByAnotherPrefix(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "A", writeRequest(t, cert, startFakeRelay(t, cert), "node-a"))
	serial := ta.ledger().Nodes[0].Serial
	same := reissue(t, cert)
	err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--prefix", "B", writeRequest(t, same, startFakeRelay(t, same), "node-b"))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(serial)) {
		t.Fatalf("same key for a second prefix: %v, want a refusal naming serial %d\n%s", err, serial, ta.buf)
	}
	if len(issued(t, ta, "node-B-*.json")) != 0 {
		t.Fatal("cert issued for prefix B")
	}
	// 提示的补救办法必须走得通：吊销那张证书会把这把私钥永久作废，照样签不进 B，所以不能引导去吊销。
	msg := err.Error()
	if strings.Contains(msg, "revoke") || !strings.Contains(msg, "新生成的私钥") {
		t.Fatalf("error %q should tell the operator the node needs a newly generated key, not to revoke the other cert", msg)
	}
}

// 只有仍有效的证书才算「这把私钥在别的前缀上用着」。换证书时被连带吊销的那张里还有节点的新私钥，
// 它没有泄露；那个前缀上的证书都吊销之后，节点用新私钥换到另一个前缀入网是可以的。
func TestSignNodeKeyOfRevokedCertMayMovePrefix(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	oldCert, newCert := testCert(t), testCert(t)
	relay := startFakeRelay(t, oldCert)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, oldCert, relay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRenewRequest(t, newCert, &oldCert, relay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(ta.ledger().Nodes[0].Serial))
	if n := len(ta.ledger().RevokedNodes); n != 2 {
		t.Fatalf("%d certs revoked, want the named one and the rotation cert", n)
	}
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "S", writeRequest(t, newCert, startFakeRelay(t, newCert), "node-r"))
	if len(issued(t, ta, "node-S-*.json")) != 1 {
		t.Fatal("no cert issued for prefix S")
	}
}

// 吊销主根后，客户端不再接受主根签的节点证书。工具要点出受影响的证书和重签办法；
// 照着提示操作（换状态密钥、用备用根重签、一起上传），客户端上的节点必须仍然可用。
func TestRevokePrimaryRootKeepsNodesUsable(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	certE, certF, certG := testCert(t), testCert(t), testCert(t)
	reqE := writeRequest(t, certE, startFakeRelay(t, certE), "node-e")
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "E", "--official", reqE)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "F", "--root", "backup", writeRequest(t, certF, startFakeRelay(t, certF), "node-f"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "G", writeRequest(t, certG, startFakeRelay(t, certG), "node-g"))
	serialE, serialF, serialG := ta.ledger().Nodes[0].Serial, ta.ledger().Nodes[1].Serial, ta.ledger().Nodes[2].Serial
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(serialG))

	out := ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	// 发版写内置吊销根时要同时抬高内置序号下限（spec 3.3），工具直接给出要抬到多少；被吊销的公钥还要从 Keys 里
	// 拿掉（TestBuiltinRoots 会查），只说写 RevokedRoots 的话照做测试就挂。
	floor := fmt.Sprintf("MinStatusSerial 抬到不低于 %d", ta.ledger().MinStatusSerial)
	for _, want := range []string{fmt.Sprint(serialE), "sign-node --renew E --root backup", floor, "从 Keys 里删掉"} {
		if !strings.Contains(out, want) {
			t.Fatalf("root revocation output lacks %q:\n%s", want, out)
		}
	}
	// F 由备用根签，不受影响；G 已经吊销，不用重签。
	if strings.Contains(out, fmt.Sprint(serialF)) || strings.Contains(out, "--renew G") {
		t.Fatalf("certs %d (backup-signed) or %d (revoked) listed for re-signing:\n%s", serialF, serialG, out)
	}

	// 照着提示操作：备用根授权新状态密钥，用节点当初的申请重签。
	ta.mustExec([]string{backupPass}, "status-key", "new", "--root", "backup")
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--renew", "E", "--official", "--root", "backup", reqE)

	var nodes []directory.NodeEntry
	for _, f := range append(issued(t, ta, "node-E-*.json"), issued(t, ta, "node-F-*.json")...) {
		if !strings.Contains(f, fmt.Sprint(serialE)) {
			nodes = append(nodes, directory.NodeEntry{Cert: readEnvelope(t, f), Status: directory.StatusActive, Weight: 100})
		}
	}
	statusCert := readEnvelope(t, issued(t, ta, "status-key-*.json")[0])
	revs := issued(t, ta, "revocations-*.json") // 先吊销 G、后吊销主根；最新一份包含全部吊销
	revocations := readEnvelope(t, revs[len(revs)-1])
	secret, _ := os.ReadFile(issued(t, ta, "status-key-*.secret.jwk")[0])
	var k jwk
	_ = json.Unmarshal(secret, &k)
	seed, _ := base64.RawURLEncoding.DecodeString(k.D)
	now := ta.now()
	raw, err := directory.SignDirectory(ed25519.NewKeyFromSeed(seed), directory.Body{
		Format: directory.FormatVersion, Serial: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(30 * 24 * time.Hour).Unix(),
		Policy: directory.PolicyPreferDefault, DefaultPrefix: "E", StatusKey: statusCert, Revocations: &revocations, Nodes: nodes,
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := directory.Verify(raw, ta.roots(), directory.State{}, now)
	if err != nil {
		t.Fatalf("client rejects the list assembled after the root revocation: %v", err)
	}
	if len(v.RevokedRoots) != 1 || !slices.Contains(v.RevokedNodes, serialG) {
		t.Fatalf("list carries revoked roots %v, nodes %v; want the primary root and cert %d", v.RevokedRoots, v.RevokedNodes, serialG)
	}
	for _, c := range []tls.Certificate{certE, certF} {
		if v.NodeByFingerprint(directory.CertFingerprint(c.Certificate[0])) == nil {
			t.Fatalf("node %x unusable after following the instructions; skipped: %+v", c.Certificate[0][:8], v.Skipped)
		}
	}
}

// 重复吊销已吊销的主根不能抬高状态密钥序号下限：那会让刚授权的现用状态密钥作废。
func TestRevokeRootTwiceKeepsStatusFloor(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	base := ta.now()
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	first := readRevocations(t, ta, issued(t, ta, "revocations-*.json")[0], "backup")

	ta.now = func() time.Time { return base.Add(time.Hour) }
	if err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "primary"); err == nil {
		t.Fatalf("revoking an already revoked root again succeeded\n%s", ta.buf)
	}
	// 同时吊销新的节点证书是正常需求，但下限保持不变。
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary", "--node-serial", "5")
	var withNode *directory.Revocations
	for _, f := range issued(t, ta, "revocations-*.json") {
		if r := readRevocations(t, ta, f, "backup"); slices.Contains(r.NodeSerials, 5) {
			withNode = &r
		}
	}
	if withNode == nil {
		t.Fatal("no revocation list with node 5")
	}
	if withNode.MinStatusSerial != first.MinStatusSerial || !slices.Equal(withNode.Roots, first.Roots) {
		t.Fatalf("floor/roots changed: first %+v, later %+v", first, *withNode)
	}
}

// 续签没带 --official 时新证书就不是官方节点了：要明说，而不是只在汇总里留一个 false。
func TestSignNodeRenewWarnsWhenOfficialDropped(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	relay := startFakeRelay(t, cert)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", "--official", writeRequest(t, cert, relay, "node-r"))
	// 先带 --official 续签：最新证书仍是官方节点，不该提示。
	out := ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", "--official", writeRequest(t, cert, relay, "node-r"))
	if strings.Contains(out, "不再是官方节点") {
		t.Fatalf("renewal with --official warned about losing the official mark:\n%s", out)
	}
	out = ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, cert, relay, "node-r"))
	if !strings.Contains(out, "不再是官方节点") {
		t.Fatalf("renewal without --official did not say the node stops being official:\n%s", out)
	}
}
