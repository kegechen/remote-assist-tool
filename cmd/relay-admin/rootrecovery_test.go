package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

// 吊销主根时列出的「要重签」的前缀，照着提示做必须走得通：
//   - 最新证书就由被吊销的根签发：可以 --renew 换根重签；
//   - 最新证书已被吊销（私钥泄露）、但还剩更早的有效证书由被吊销的根签发：--renew 会被拒绝，
//     只能吊销剩下的证书后用 --prefix 重新入网；
//   - 最新证书已经由另一把根签发：什么都不用做，不能列出来。
func TestRevokeRootGuidesEachPrefixToAWorkingPath(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()

	// R：一次完整的换证书 cert1 [O] → cert2 [N,O] → cert3 [N]，之后 N 泄露，吊销 cert3（cert2 连带）；cert1 仍有效。
	oldCert, newCert := testCert(t), testCert(t)
	oldRelay, newRelay := startFakeRelay(t, oldCert), startFakeRelay(t, newCert)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "R", writeRequest(t, oldCert, oldRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRenewRequest(t, newCert, &oldCert, oldRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, newCert, newRelay, "node-r"))
	cert1, cert3 := ta.ledger().Nodes[0].Serial, ta.ledger().Nodes[2].Serial
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(cert3))

	// T：和 R 一样换过证书、最新一张被吊销，但剩下的 cert1 由备用根签发，吊销主根不影响它，不能列出来。
	tOld, tNew := testCert(t), testCert(t)
	tOldRelay, tNewRelay := startFakeRelay(t, tOld), startFakeRelay(t, tNew)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "T", "--root", "backup", writeRequest(t, tOld, tOldRelay, "node-t"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "T", writeRenewRequest(t, tNew, &tOld, tOldRelay, "node-t"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "T", writeRequest(t, tNew, tNewRelay, "node-t"))
	nodes := ta.ledger().Nodes
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(nodes[len(nodes)-1].Serial))

	// S：主根签发，之后最新一张已改由备用根重签。
	sCert := testCert(t)
	sReq := writeRequest(t, sCert, startFakeRelay(t, sCert), "node-s")
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "S", sReq)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--renew", "S", "--root", "backup", sReq)

	// E：普通情形，最新证书由主根签发。
	eCert := testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "E", writeRequest(t, eCert, startFakeRelay(t, eCert), "node-e"))

	out := ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	for _, want := range []string{
		"sign-node --renew E --root backup",
		"sign-node --prefix R --root backup",
		// 吊销主根之后主根不能再签，吊销剩下的证书要改用备用根签。
		fmt.Sprintf("revoke --signer backup --node-serial %d", cert1),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("root revocation output lacks %q:\n%s", want, out)
		}
	}
	// R 的 --renew 走不通；S 的最新证书、T 剩下的有效证书都是备用根签的，不用重签。
	for _, bad := range []string{"--renew R", "--renew S", `"node-s"`, "前缀 T ", "--prefix T", "--renew T"} {
		if strings.Contains(out, bad) {
			t.Fatalf("root revocation output contains %q, which the operator cannot or need not act on:\n%s", bad, out)
		}
	}

	// R 的 --renew 确实走不通（最新证书已吊销），所以不能提示它。要在吊销 cert1 之前试：吊销之后 cert1 的私钥
	// 也进了泄露名单，被拒的原因就换成了那个，测不到这里。
	err := ta.exec([]string{"yes", backupPass}, "sign-node", "--renew", "R", "--root", "backup", writeRequest(t, oldCert, oldRelay, "node-r"))
	if err == nil || !strings.Contains(err.Error(), "最新的证书已被吊销") {
		t.Fatalf("renewing R from its remaining old cert: err = %v, want a refusal because the latest cert is revoked", err)
	}

	// 照着 R 的提示做：吊销剩下的证书，用新私钥换前缀 R 重新入网。
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--signer", "backup", "--node-serial", fmt.Sprint(cert1))
	pCert := testCert(t)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "R", "--root", "backup", writeRequest(t, pCert, startFakeRelay(t, pCert), "node-r"))
}

// 要重新入网的前缀，列出的是它剩下的全部有效证书，其中可能有另一把根签的（客户端仍接受，只是重新入网前也得吊销）。
// 这些要标出来，不能笼统说成「由被吊销的根签发、客户端不再接受」。
func TestRevokeRootReonboardMarksCertsOfOtherRoot(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	// R：c1[Z] 由备用根签，之后主根签 c2[O,Z] → c3[O] → c4[N,O] → c5[N]；吊销 c5（c4 连带），剩 c1、c2、c3。
	z, o, n := testCert(t), testCert(t), testCert(t)
	zRelay, oRelay, nRelay := startFakeRelay(t, z), startFakeRelay(t, o), startFakeRelay(t, n)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "R", "--root", "backup", writeRequest(t, z, zRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRenewRequest(t, o, &z, zRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, o, oRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRenewRequest(t, n, &o, oRelay, "node-r"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "R", writeRequest(t, n, nRelay, "node-r"))
	nodes := ta.ledger().Nodes
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(nodes[4].Serial))

	out := ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	lineOf := func(serial uint64) string {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, fmt.Sprintf("序号 %d", serial)) {
				return line
			}
		}
		t.Fatalf("cert %d not listed:\n%s", serial, out)
		return ""
	}
	if line := lineOf(nodes[0].Serial); !strings.Contains(line, "仍接受") {
		t.Fatalf("backup-signed cert listed without saying clients still accept it: %q\n%s", line, out)
	}
	for _, c := range nodes[1:3] {
		if line := lineOf(c.Serial); strings.Contains(line, "仍接受") {
			t.Fatalf("primary-signed cert claimed to stay accepted: %q", line)
		}
	}
}

// 同一条命令里既吊销主根、又点名吊销节点证书时，「要重签」的证书按这次吊销之后的状态算：这次点名吊销的不能列。
func TestRevokeRootAndNodesInOneCommand(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	eCert, gCert := testCert(t), testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "E", writeRequest(t, eCert, startFakeRelay(t, eCert), "node-e"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "G", writeRequest(t, gCert, startFakeRelay(t, gCert), "node-g"))
	serialE := ta.ledger().Nodes[0].Serial

	out := ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary", "--node-serial", fmt.Sprint(serialE))
	if !strings.Contains(out, "sign-node --renew G --root backup") {
		t.Fatalf("output lacks the re-sign step for G:\n%s", out)
	}
	if strings.Contains(out, "--renew E") || strings.Contains(out, `"node-e"`) {
		t.Fatalf("cert E, revoked by this very command, is listed for re-signing:\n%s", out)
	}
}

// 备用根泄露只能发版（spec 3.3）。拒绝 --root backup 时要列出备用根签发、仍有效的节点证书，
// 并告诉管理员用主根重签：发版换掉备用根公钥之后，客户端不再接受它签过的证书。
func TestRevokeBackupRootRefusalListsCertsToResign(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	fCert, eCert := testCert(t), testCert(t)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "F", "--root", "backup", writeRequest(t, fCert, startFakeRelay(t, fCert), "node-f"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "E", writeRequest(t, eCert, startFakeRelay(t, eCert), "node-e"))
	serialF := ta.ledger().Nodes[0].Serial

	err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup")
	if err == nil {
		t.Fatalf("revoking the backup root was accepted:\n%s", ta.buf)
	}
	out := ta.buf.String()
	for _, want := range []string{fmt.Sprint(serialF), "sign-node --renew F --root primary"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--renew E") {
		t.Fatalf("primary-signed cert E listed for re-signing:\n%s", out)
	}
	// 主根还能用：先用主根授权新状态密钥，只换备用根；不能让人把两把根都换掉。
	if !strings.Contains(err.Error(), "先用主根") || strings.Contains(err.Error(), "两把根") {
		t.Fatalf("error %q does not match the situation: only the backup root leaked", err)
	}
	// 工具里没有换根的命令，错误要指到手工做法，不能让人去跑会拒绝覆盖的 keygen。
	if !strings.Contains(err.Error(), "没有换根的命令") {
		t.Fatalf("error %q does not say how to get a new backup key", err)
	}
	// 台账里还没有任何吊销，就没有要重签的吊销列表：不能提示 revoke --resign（照做会被拒绝）。
	if strings.Contains(out, "revoke --resign") {
		t.Fatalf("output suggests revoke --resign although nothing is revoked:\n%s", out)
	}
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--signer", "backup", "--node-serial", fmt.Sprint(ta.ledger().Nodes[1].Serial))
	if err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup"); err == nil {
		t.Fatal("revoking the backup root was accepted")
	}
	// 后果要说清：发版后新客户端不是只忽略这份吊销，而是整份节点列表拒收。
	if out := ta.buf.String(); !strings.Contains(out, "revoke --resign") || !strings.Contains(out, "整份拒收") {
		t.Fatalf("output lacks the revoke --resign step and its stakes although revocations exist:\n%s", out)
	}
}

// 备用根平时离线保管，管理目录里可能没有它的文件。这时拒绝 --root backup 不能静默漏掉要做的事，
// 也不能在主根已吊销时还叫人先用主根。
func TestRevokeBackupRootRefusalWithoutBackupFiles(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	fCert := testCert(t)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "F", "--root", "backup", writeRequest(t, fCert, startFakeRelay(t, fCert), "node-f"))
	files := map[string][]byte{}
	for _, p := range []string{ta.keyPath(directory.SignerBackup), ta.pubPath(directory.SignerBackup)} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = data
	}
	setBackupFiles := func(present bool) {
		for p, data := range files {
			var err error
			if present {
				err = os.WriteFile(p, data, 0o600)
			} else {
				err = os.Remove(p)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	setBackupFiles(false)
	if err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup"); err == nil {
		t.Fatal("revoking the backup root was accepted")
	}
	if out := ta.buf.String(); !strings.Contains(out, "没法列出") {
		t.Fatalf("without the backup public key, the refusal silently lists nothing:\n%s", out)
	}
	// 主根公钥、台账读不到时同样要明说，不能静默。
	for _, c := range []struct {
		name string
		path string
		bad  []byte // nil 表示删掉
	}{
		{"no primary public key", ta.pubPath(directory.SignerPrimary), nil},
		{"unreadable ledger", filepath.Join(ta.dir, "ledger.json"), []byte("{")},
	} {
		orig, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if c.bad == nil {
			err = os.Remove(c.path)
		} else {
			err = os.WriteFile(c.path, c.bad, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup"); err == nil {
			t.Fatalf("%s: revoking the backup root was accepted", c.name)
		}
		if out := ta.buf.String(); !strings.Contains(out, "没法列出") {
			t.Fatalf("%s: the refusal silently lists nothing:\n%s", c.name, out)
		}
		if err := os.WriteFile(c.path, orig, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	setBackupFiles(true)
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	setBackupFiles(false)
	err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup")
	if err == nil || strings.Contains(err.Error(), "先用主根") || !strings.Contains(err.Error(), "两把根") {
		t.Fatalf("primary revoked, backup files away: err = %v, want the both-roots refusal\n%s", err, ta.buf)
	}
}

// 工具没有换根的命令，备用根泄露时按设计文档 3.3 手工换：在临时目录 keygen，只把新的备用根文件拷进管理目录。
// 这条路要真的走得通：新备用根能用，主根和台账不受影响。
func TestReplaceBackupRootByHand(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{primaryPass}, "status-key", "new")
	oldFP, err := ta.rootFP(directory.SignerBackup)
	if err != nil {
		t.Fatal(err)
	}
	before := ta.ledger()

	fresh := newTestApp(t)
	fresh.keygen()
	for _, p := range []string{fresh.keyPath(directory.SignerBackup), fresh.pubPath(directory.SignerBackup)} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ta.dir, filepath.Base(p)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if fp, _ := ta.rootFP(directory.SignerBackup); fp == oldFP {
		t.Fatal("backup key not replaced")
	}
	if after := ta.ledger(); after.LastStatusSerial != before.LastStatusSerial || len(after.Nodes) != len(before.Nodes) {
		t.Fatalf("ledger changed: %+v -> %+v", before, after)
	}
	ta.mustExec([]string{backupPass}, "status-key", "new", "--root", "backup")
	ta.mustExec([]string{primaryPass}, "status-key", "new")
}

// 手工换掉备用根之后，台账里旧备用根签的证书记的是旧指纹，管理目录里已经没有这把根。发版内置吊销旧备用根后
// 客户端不再接受它们，所以照样要列出来重签，不能因为认不出这个指纹就静默漏掉（也不能说成「客户端仍接受」）。
func TestReplacedBackupRootCertsStillListed(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	fCert := testCert(t)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "F", "--root", "backup", writeRequest(t, fCert, startFakeRelay(t, fCert), "node-f"))
	serialF := ta.ledger().Nodes[0].Serial
	// X 要重新入网：c1[O] 由旧备用根签，之后主根签 c2[O,N] → c3[N]；吊销 c3（c2 连带），只剩 c1。
	o, n := testCert(t), testCert(t)
	oRelay, nRelay := startFakeRelay(t, o), startFakeRelay(t, n)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "X", "--root", "backup", writeRequest(t, o, oRelay, "node-x"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "X", writeRenewRequest(t, n, &o, oRelay, "node-x"))
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--renew", "X", writeRequest(t, n, nRelay, "node-x"))
	nodes := ta.ledger().Nodes
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(nodes[3].Serial))
	serialX1 := nodes[1].Serial

	fresh := newTestApp(t)
	fresh.keygen()
	for _, p := range []string{fresh.keyPath(directory.SignerBackup), fresh.pubPath(directory.SignerBackup)} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ta.dir, filepath.Base(p)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup"); err == nil {
		t.Fatal("revoking the backup root was accepted")
	}
	if out := ta.buf.String(); !strings.Contains(out, fmt.Sprint(serialF)) || !strings.Contains(out, "sign-node --renew F --root primary") {
		t.Fatalf("after replacing the backup root by hand, cert F signed by the old one is not listed:\n%s", out)
	}

	out := ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	for _, want := range []string{"sign-node --renew F --root backup", fmt.Sprintf("revoke --signer backup --node-serial %d", serialX1)} {
		if !strings.Contains(out, want) {
			t.Fatalf("certs signed by the replaced backup root are not handled (want %q):\n%s", want, out)
		}
	}
	if strings.Contains(out, "仍接受") || !strings.Contains(out, "旧根") {
		t.Fatalf("certs signed by the replaced backup root are said to stay accepted, or not marked as such:\n%s", out)
	}
}

// 备用根签的前缀最新证书已被吊销、还剩更早的有效证书时，备用根泄露的提示要走「吊销剩下的、用主根重新入网」，
// 并且照做走得通。
func TestRevokeBackupRootRefusalReonboardsWithPrimary(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	o, n := testCert(t), testCert(t)
	oRelay, nRelay := startFakeRelay(t, o), startFakeRelay(t, n)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "X", "--root", "backup", writeRequest(t, o, oRelay, "node-x"))
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--renew", "X", "--root", "backup", writeRenewRequest(t, n, &o, oRelay, "node-x"))
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--renew", "X", "--root", "backup", writeRequest(t, n, nRelay, "node-x"))
	nodes := ta.ledger().Nodes
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(nodes[2].Serial))

	if err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup"); err == nil {
		t.Fatal("revoking the backup root was accepted")
	}
	out := ta.buf.String()
	for _, want := range []string{
		fmt.Sprintf("revoke --signer primary --node-serial %d", nodes[0].Serial),
		"sign-node --prefix X --root primary",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	// 照做。
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--signer", "primary", "--node-serial", fmt.Sprint(nodes[0].Serial))
	p := testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "X", "--root", "primary", writeRequest(t, p, startFakeRelay(t, p), "node-x"))
}

// 主根已经吊销过（台账里有记录）时备用根又泄露，就没有能用的根了：拒绝 --root backup 时不能再提示用主根重签
// （照做会被拒绝），要说明两把根都得换。
func TestRevokeBackupRootRefusalWhenPrimaryAlsoRevoked(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	fCert := testCert(t)
	fReq := writeRequest(t, fCert, startFakeRelay(t, fCert), "node-f")
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "F", "--root", "backup", fReq)
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")

	err := ta.exec([]string{"yes", backupPass}, "revoke", "--root", "backup")
	if err == nil {
		t.Fatalf("revoking the backup root was accepted:\n%s", ta.buf)
	}
	// 返回的错误（main 会打印出来）同样不能让人先用主根授权状态密钥。
	if strings.Contains(err.Error(), "先用主根") || !strings.Contains(err.Error(), "两把根") {
		t.Fatalf("error %q does not match the situation: the primary root is revoked too", err)
	}
	out := ta.buf.String()
	if strings.Contains(out, "--root primary") || strings.Contains(out, "由主根重签") {
		t.Fatalf("output tells the operator to use the revoked primary root:\n%s", out)
	}
	if !strings.Contains(out, "两把根都") {
		t.Fatalf("output does not say that both roots must be replaced:\n%s", out)
	}
	// 不该提示的那条路确实走不通。
	if err := ta.exec([]string{"yes", primaryPass}, "sign-node", "--renew", "F", "--root", "primary", fReq); err == nil {
		t.Fatal("re-signing with the revoked primary root was accepted")
	}
}

// revoke --resign：不新增任何条目，把现有的累积吊销换一把根重签。备用根签过吊销列表、之后发版换掉备用根时，
// 旧列表客户端不再认，要用主根重签一份。
func TestRevokeResignOnlyChangesTheSigner(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	cert := testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "X", writeRequest(t, cert, startFakeRelay(t, cert), "node-x"))
	serial := ta.ledger().Nodes[0].Serial
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--signer", "backup", "--node-serial", fmt.Sprint(serial))
	before := ta.ledger()
	prev := readRevocations(t, ta, issued(t, ta, "revocations-*.json")[0], "backup")

	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--resign")
	files := issued(t, ta, "revocations-*.json")
	if len(files) != 2 {
		t.Fatalf("%d revocation files, want the original and the re-signed one", len(files))
	}
	got := readRevocations(t, ta, files[1], "primary")
	if !slices.Equal(got.NodeSerials, prev.NodeSerials) || !slices.Equal(got.Roots, prev.Roots) || got.MinStatusSerial != prev.MinStatusSerial {
		t.Fatalf("re-signed list %+v differs from the original %+v in more than its serial and signer", got, prev)
	}
	if got.Serial <= prev.Serial {
		t.Fatalf("re-signed list serial %d is not above the original %d", got.Serial, prev.Serial)
	}
	after := ta.ledger()
	if !slices.Equal(after.RevokedNodes, before.RevokedNodes) || !slices.Equal(after.LeakedKeys, before.LeakedKeys) ||
		!slices.Equal(after.RevokedRoots, before.RevokedRoots) || after.MinStatusSerial != before.MinStatusSerial ||
		after.LastRevocationSerial != got.Serial {
		t.Fatalf("ledger changed beyond the revocation serial: before %+v after %+v", before, after)
	}

	// 不能同时指定要吊销的东西；没有任何吊销时没有可重签的。
	if err := ta.exec([]string{"yes", primaryPass}, "revoke", "--resign", "--node-serial", "99"); err == nil {
		t.Fatal("--resign together with --node-serial was accepted")
	}
	empty := newTestApp(t)
	empty.keygen()
	if err := empty.exec([]string{"yes", primaryPass}, "revoke", "--resign"); err == nil {
		t.Fatalf("--resign with nothing revoked was accepted:\n%s", empty.buf)
	}
}

// 累积吊销里含根吊销时只有备用根能签（checkRevocations）：用主根重签会得到客户端整份拒收的列表，要提前拒绝并说明。
func TestRevokeResignWithRootRevocationNeedsBackup(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	err := ta.exec([]string{"yes", primaryPass}, "revoke", "--resign", "--signer", "primary")
	if err == nil {
		t.Fatalf("re-signing a list that revokes a root with the primary root was accepted:\n%s", ta.buf)
	}
	if !strings.Contains(err.Error(), "备用根") {
		t.Fatalf("error %q does not say that only the backup root can sign root revocations", err)
	}
	// 用备用根重签可以，内容原样保留：丢了根吊销或下限，新列表就成了「撤销对主根的吊销」。
	prev := readRevocations(t, ta, issued(t, ta, "revocations-*.json")[0], "backup")
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--resign", "--signer", "backup")
	files := issued(t, ta, "revocations-*.json")
	got := readRevocations(t, ta, files[len(files)-1], "backup")
	if len(prev.Roots) != 1 || !slices.Equal(got.Roots, prev.Roots) || got.MinStatusSerial != prev.MinStatusSerial ||
		!slices.Equal(got.NodeSerials, prev.NodeSerials) || got.Serial <= prev.Serial {
		t.Fatalf("re-signed list %+v does not carry over %+v", got, prev)
	}
}

// inspect 要按台账里已吊销的根验证：客户端见过那份吊销（或发版内置了它）之后，被吊销的根签的东西就不再有效。
// 管理员应急时拿 inspect 核对要上传的东西，不能被「全部签名有效」误导。
func TestInspectAppliesLedgerRootRevocations(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{primaryPass}, "status-key", "new")
	cert, bCert := testCert(t), testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "E", "--official", writeRequest(t, cert, startFakeRelay(t, cert), "node-e"))
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "F", "--root", "backup", writeRequest(t, bCert, startFakeRelay(t, bCert), "node-f"))
	dirFile := writeTestDirectory(t, ta, "E")

	ta.mustExec(nil, "inspect", dirFile) // 吊销之前一切正常
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")

	// 由已吊销的主根授权状态密钥的列表：见过吊销的客户端会拒收，inspect 不能说它有效。
	if err := ta.exec(nil, "inspect", dirFile); err == nil {
		t.Fatalf("inspect shows a list authorized by the revoked primary root as valid:\n%s", ta.buf)
	}
	// 单个对象：主根签发的要标出已吊销，备用根签发的不能标。
	if out := ta.mustExec(nil, "inspect", issued(t, ta, "node-E-*.json")[0]); !strings.Contains(out, "签发它的主根密钥已吊销") {
		t.Fatalf("inspect of a cert signed by the revoked primary root does not say so:\n%s", out)
	}
	if out := ta.mustExec(nil, "inspect", issued(t, ta, "node-F-*.json")[0]); strings.Contains(out, "签发它的") {
		t.Fatalf("inspect of a backup-signed cert claims its signer is revoked:\n%s", out)
	}
}

// inspect 还要用台账里的状态密钥序号下限：吊销主根时声明了此前授权的状态密钥一律作废，哪怕是备用根授权的。
func TestInspectAppliesLedgerStatusFloor(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{backupPass}, "status-key", "new", "--root", "backup")
	cert := testCert(t)
	ta.mustExec([]string{"yes", backupPass}, "sign-node", "--prefix", "F", "--root", "backup", writeRequest(t, cert, startFakeRelay(t, cert), "node-f"))
	dirFile := writeTestDirectory(t, ta, "F")

	ta.mustExec(nil, "inspect", dirFile)
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	if err := ta.exec(nil, "inspect", dirFile); err == nil {
		t.Fatalf("inspect shows a list whose status key is below the declared floor as valid:\n%s", ta.buf)
	}
}

// inspect 也要按台账里吊销的节点证书验证：吊销 E 之后，含 E 的列表不能再把 E 列为可用，单看 E 的证书也要说明已吊销。
func TestInspectAppliesLedgerNodeRevocations(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{primaryPass}, "status-key", "new")
	cert := testCert(t)
	ta.mustExec([]string{"yes", primaryPass}, "sign-node", "--prefix", "E", writeRequest(t, cert, startFakeRelay(t, cert), "node-e"))
	dirFile, certFile := writeTestDirectory(t, ta, "E"), issued(t, ta, "node-E-*.json")[0]

	if out := ta.mustExec(nil, "inspect", dirFile); !strings.Contains(out, "通过校验的节点 1 个") {
		t.Fatalf("before revoking, E is not usable:\n%s", out)
	}
	if out := ta.mustExec(nil, "inspect", certFile); strings.Contains(out, "证书已吊销") {
		t.Fatalf("before revoking, the cert is reported revoked:\n%s", out)
	}
	ta.mustExec([]string{"yes", primaryPass}, "revoke", "--node-serial", fmt.Sprint(ta.ledger().Nodes[0].Serial))
	if out := ta.mustExec(nil, "inspect", dirFile); !strings.Contains(out, "通过校验的节点 0 个") || !strings.Contains(out, "被丢弃的节点 1 个") {
		t.Fatalf("inspect still lists the revoked cert E as usable:\n%s", out)
	}
	if out := ta.mustExec(nil, "inspect", certFile); !strings.Contains(out, "证书已吊销") {
		t.Fatalf("inspect of a revoked cert does not say so:\n%s", out)
	}
}

// 单看一份状态密钥授权时，也要按台账里的状态密钥序号下限判断：吊销主根时声明了此前授权的一律作废，备用根授权的也算。
func TestInspectStatusKeyBelowLedgerFloor(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{backupPass}, "status-key", "new", "--root", "backup")
	keyFile := issued(t, ta, "status-key-*.json")[0]
	if out := ta.mustExec(nil, "inspect", keyFile); strings.Contains(out, "客户端会拒收") {
		t.Fatalf("before the floor was raised, the status key is reported rejected:\n%s", out)
	}
	ta.mustExec([]string{"yes", backupPass}, "revoke", "--root", "primary")
	if out := ta.mustExec(nil, "inspect", keyFile); !strings.Contains(out, "低于状态密钥序号下限") || !strings.Contains(out, "客户端会拒收") {
		t.Fatalf("inspect of a status key below the declared floor does not say clients reject it:\n%s", out)
	}
}

// 台账读不出来时 inspect 不能悄悄当成「没有吊销」，把已吊销的东西说成有效；没有台账（只拷了 .pub）才照常验证。
func TestInspectRefusesUnreadableLedger(t *testing.T) {
	ta := newTestApp(t)
	ta.keygen()
	ta.mustExec([]string{primaryPass}, "status-key", "new")
	keyFile := issued(t, ta, "status-key-*.json")[0]
	ledgerFile := filepath.Join(ta.dir, "ledger.json")

	if err := os.WriteFile(ledgerFile, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ta.exec(nil, "inspect", keyFile)
	if err == nil || !strings.Contains(err.Error(), "台账") {
		t.Fatalf("inspect with an unreadable ledger: err = %v, want a refusal naming the ledger\n%s", err, ta.buf)
	}
	// 程序里还没内置根时，提示改用 --builtin 走不通。
	if builtin := len(directory.BuiltinRoots().Keys) > 0; strings.Contains(err.Error(), "--builtin") != builtin {
		t.Fatalf("error %q: mentions --builtin = %v, want %v (whether builtin roots exist)", err, !builtin, builtin)
	}
	// --builtin 不看管理目录：报错只能说程序里没内置，不能说管理目录里没有 .pub（明明有）。
	if len(directory.BuiltinRoots().Keys) == 0 {
		if err := ta.exec(nil, "inspect", "--builtin", keyFile); err == nil || strings.Contains(err.Error(), ".pub") {
			t.Fatalf("inspect --builtin without builtin roots: err = %v, want one about builtin roots only", err)
		}
	}
	if err := os.Remove(ledgerFile); err != nil {
		t.Fatal(err)
	}
	ta.mustExec(nil, "inspect", keyFile)
}

// writeTestDirectory 用管理目录里唯一的那把状态密钥签一份只含 prefix 节点的列表，写到临时文件里返回路径。
func writeTestDirectory(t *testing.T, ta *testApp, prefix string) string {
	t.Helper()
	secret, err := os.ReadFile(issued(t, ta, "status-key-*.secret.jwk")[0])
	if err != nil {
		t.Fatal(err)
	}
	var k jwk
	if err := json.Unmarshal(secret, &k); err != nil {
		t.Fatal(err)
	}
	seed, err := base64.RawURLEncoding.DecodeString(k.D)
	if err != nil {
		t.Fatal(err)
	}
	now := ta.now()
	raw, err := directory.SignDirectory(ed25519.NewKeyFromSeed(seed), directory.Body{
		Format: directory.FormatVersion, Serial: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(30 * 24 * time.Hour).Unix(),
		Policy: directory.PolicyPreferDefault, DefaultPrefix: prefix,
		StatusKey: readEnvelope(t, issued(t, ta, "status-key-*.json")[0]),
		Nodes:     []directory.NodeEntry{{Cert: readEnvelope(t, issued(t, ta, "node-"+prefix+"-*.json")[0]), Status: directory.StatusActive, Weight: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "directory.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
