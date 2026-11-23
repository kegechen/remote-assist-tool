package directory_test

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// 密钥泄露后的恢复路径（spec 3.3）：攻击者先用泄露的密钥把客户端持久状态推到极值，
// 管理员按规定流程处置后，客户端必须能恢复，而不是永久卡死。

func TestRecoverFromLeakedStatusKey(t *testing.T) {
	f := dirtest.New(t)
	// 攻击者拿着泄露的状态密钥（授权序号 1），签一份列表序号拉满的列表。
	poison := f.Body(f.Entry("A"))
	poison.Serial = math.MaxUint64
	poisonRaw := f.Sign(poison)
	st := mustVerify(t, f, poisonRaw, directory.State{}).NewState

	// 管理员用主根授权新状态密钥（序号 2），Worker 换新密钥后列表序号从头编。
	fresh := dirtest.SeedKey("status-2")
	b := f.Body(f.Entry("A"))
	b.StatusKey = f.IssueStatus(2, fresh, f.Primary, directory.SignerPrimary)
	b.Serial = 1
	first, err := directory.SignDirectory(fresh, b)
	if err != nil {
		t.Fatal(err)
	}
	b.Serial = 2
	second, err := directory.SignDirectory(fresh, b)
	if err != nil {
		t.Fatal(err)
	}
	mustVerify(t, f, first, st)
	st = mustVerify(t, f, second, st).NewState

	// 恢复之后：泄露的旧状态密钥签的列表、新密钥签的更早列表，都不再被接受。
	_, err = verify(t, f, poisonRaw, st)
	wantErr(t, err, directory.ErrRollback, "status_key")
	_, err = verify(t, f, first, st)
	wantErr(t, err, directory.ErrRollback, "directory")
}

func TestRecoverFromLeakedPrimaryRoot(t *testing.T) {
	f := dirtest.New(t)
	// 攻击者拿着主根：给自己的状态密钥签一份序号拉满的授权，再签一份序号拉满的列表。
	evil := dirtest.SeedKey("evil-status")
	pb := f.Body(f.Entry("A"))
	pb.StatusKey = f.IssueStatus(math.MaxUint64, evil, f.Primary, directory.SignerPrimary)
	pb.Serial = math.MaxUint64
	poisonRaw, err := directory.SignDirectory(evil, pb)
	if err != nil {
		t.Fatal(err)
	}
	st := mustVerify(t, f, poisonRaw, directory.State{}).NewState

	// 攻击者还想抢先吊销备用根，堵死恢复路径：根吊销只认备用根签的。
	pb.Revocations = f.Revocations(math.MaxUint64, nil, []string{directory.SignerBackup}, f.Primary, directory.SignerPrimary)
	revokeBackup, err := directory.SignDirectory(evil, pb)
	if err != nil {
		t.Fatal(err)
	}
	_, err = verify(t, f, revokeBackup, st)
	wantErr(t, err, directory.ErrMalformed, "revocations")

	// 管理员处置：备用根吊销主根、授权新状态密钥、重签节点证书，序号都是正常值。
	status := dirtest.SeedKey("status-backup")
	b := f.Body(directory.NodeEntry{
		Cert:   f.SignCertWith(f.Cert("A", nil), f.Backup, directory.SignerBackup),
		Status: directory.StatusActive, Weight: 100,
	})
	b.StatusKey = f.IssueStatus(5, status, f.Backup, directory.SignerBackup)
	b.Revocations = f.Revocations(1, nil, []string{directory.SignerPrimary}, f.Backup, directory.SignerBackup)
	b.Serial = 1
	raw, err := directory.SignDirectory(status, b)
	if err != nil {
		t.Fatal(err)
	}
	v := mustVerify(t, f, raw, st)
	if len(v.Nodes) != 1 || v.NewState.StatusRoot != f.RootFP(directory.SignerBackup) {
		t.Fatalf("recovered list: nodes %d, state %+v", len(v.Nodes), v.NewState)
	}

	// 之后攻击者的列表一律被拒。
	_, err = verify(t, f, poisonRaw, v.NewState)
	wantErr(t, err, directory.ErrRevoked, "status_key")
}

// listBy 生成一份由 root 授权的新状态密钥签的列表（节点证书也由 root 签），状态密钥序号 statusSerial、列表序号 1。
func listBy(t *testing.T, f *dirtest.Fixture, root ed25519.PrivateKey, signer string, statusSerial uint64) []byte {
	t.Helper()
	status := dirtest.SeedKey(fmt.Sprintf("status-%s-%d", signer, statusSerial))
	b := f.Body(directory.NodeEntry{Cert: f.SignCertWith(f.Cert("A", nil), root, signer), Status: directory.StatusActive, Weight: 100})
	b.StatusKey = f.IssueStatus(statusSerial, status, root, signer)
	b.Serial = 1
	raw, err := directory.SignDirectory(status, b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 备用根泄露：攻击者用它把位置推到极值，并吊销主根。spec 3.3 的处置是发版——内置吊销泄露的那把备用根、换上新的。
// 吊销认的是那把公钥，不是「backup」这个名字：发版后新备用根、原主根都必须能用，攻击者的列表一律拒收。
func TestRecoverFromLeakedBackupRoot(t *testing.T) {
	f := dirtest.New(t)
	evil := dirtest.SeedKey("evil-status")
	pb := f.Body(f.Entry("A"))
	pb.StatusKey = f.IssueStatus(math.MaxUint64, evil, f.Backup, directory.SignerBackup)
	pb.Serial = math.MaxUint64
	pb.Revocations = f.Revocations(math.MaxUint64, nil, []string{directory.SignerPrimary}, f.Backup, directory.SignerBackup)
	poisonRaw, err := directory.SignDirectory(evil, pb)
	if err != nil {
		t.Fatal(err)
	}
	st := mustVerify(t, f, poisonRaw, directory.State{}).NewState

	newBackup, newPrimary := dirtest.SeedKey("backup-2"), dirtest.SeedKey("primary-2")
	release := f.Roots
	release.Keys = map[string]ed25519.PublicKey{
		directory.SignerPrimary: f.Primary.Public().(ed25519.PublicKey),
		directory.SignerBackup:  newBackup.Public().(ed25519.PublicKey),
	}
	oldBackupFP := f.RootFP(directory.SignerBackup)
	release.RevokedRoots = []string{oldBackupFP}
	// 主根也一并换掉的发版。
	release2 := release
	release2.Keys = map[string]ed25519.PublicKey{
		directory.SignerPrimary: newPrimary.Public().(ed25519.PublicKey),
		directory.SignerBackup:  newBackup.Public().(ed25519.PublicKey),
	}
	for _, c := range []struct {
		name  string
		roots directory.Roots
		raw   []byte
	}{
		{"new backup root", release, listBy(t, f, newBackup, directory.SignerBackup, 10)},
		{"original primary root", release, listBy(t, f, f.Primary, directory.SignerPrimary, 10)},
		{"new primary root", release2, listBy(t, f, newPrimary, directory.SignerPrimary, 10)},
	} {
		for _, s := range []struct {
			name string
			st   directory.State
		}{{"poisoned client", st}, {"fresh client", directory.State{}}} {
			v, err := directory.Verify(c.raw, c.roots, s.st, f.Now)
			if err != nil {
				t.Errorf("%s, %s: %v", c.name, s.name, err)
				continue
			}
			// 内置吊销的旧备用根留下的记录什么都不再贡献，不该继续占着持久状态。
			for _, r := range v.NewState.Revocations {
				if r.Root == oldBackupFP {
					t.Errorf("%s, %s: state still carries the revoked backup root's record %+v", c.name, s.name, r)
				}
			}
		}
	}
	// 旧备用根的公钥已不在「backup」名下，攻击者的状态密钥授权验签就过不了；即便换个名字也命中内置吊销。
	for _, roots := range []directory.Roots{release, release2} {
		if _, err := directory.Verify(poisonRaw, roots, st, f.Now); !errors.Is(err, directory.ErrBadSignature) && !errors.Is(err, directory.ErrRevoked) {
			t.Errorf("attacker's list after the release: %v, want ErrBadSignature or ErrRevoked", err)
		}
	}
	// 发版时若沿用旧备用根公钥但列入内置吊销，同样拒收。
	kept := f.Roots
	kept.RevokedRoots = release.RevokedRoots
	if _, err := directory.Verify(poisonRaw, kept, st, f.Now); !errors.Is(err, directory.ErrRevoked) {
		t.Errorf("attacker's list with the leaked backup builtin-revoked: %v, want ErrRevoked", err)
	}
}

// 主根泄露、被备用根吊销后，主根留下的位置作废。但此前被取代的旧状态密钥（哪怕由备用根授权）不能借此重新生效：
// 吊销主根的同一份吊销列表要能声明「状态密钥序号低于 N 的一律作废」。
func TestRevokedPrimaryKeepsOlderStatusKeysObsolete(t *testing.T) {
	f := dirtest.New(t)
	s1, s2 := dirtest.SeedKey("s1"), dirtest.SeedKey("s2")
	backupEntry := func() directory.NodeEntry {
		return directory.NodeEntry{Cert: f.SignCertWith(f.Cert("A", nil), f.Backup, directory.SignerBackup), Status: directory.StatusActive, Weight: 100}
	}
	// 客户端见过主根授权的 S2（序号 200）签的列表。
	cur := f.Body(f.Entry("A"))
	cur.StatusKey = f.IssueStatus(200, s2, f.Primary, directory.SignerPrimary)
	cur.Serial = 30
	curRaw, err := directory.SignDirectory(s2, cur)
	if err != nil {
		t.Fatal(err)
	}
	st := mustVerify(t, f, curRaw, directory.State{}).NewState

	// 备用根吊销主根（公开发布的吊销列表），同时声明状态密钥序号下限 201；恢复用的新状态密钥序号从 201 起。
	revokePrimary := f.SignRevocations(directory.Revocations{
		Serial: 1, Roots: []string{f.RootFP(directory.SignerPrimary)}, MinStatusSerial: 201,
	}, f.Backup, directory.SignerBackup)

	// 攻击者手里有更早由备用根授权、早已被 S2 取代的 S1（序号 100），带上这份公开的吊销签一份列表。
	old := f.Body(backupEntry())
	old.StatusKey = f.IssueStatus(100, s1, f.Backup, directory.SignerBackup)
	old.Revocations = revokePrimary
	old.Serial = 1
	oldRaw, err := directory.SignDirectory(s1, old)
	if err != nil {
		t.Fatal(err)
	}
	_, err = verify(t, f, oldRaw, st)
	wantErr(t, err, directory.ErrRollback, "status_key")

	// 恢复用的新状态密钥照常可用。
	rec := f.Body(backupEntry())
	rec.StatusKey = f.IssueStatus(201, dirtest.SeedKey("s3"), f.Backup, directory.SignerBackup)
	rec.Revocations = revokePrimary
	rec.Serial = 1
	recRaw, err := directory.SignDirectory(dirtest.SeedKey("s3"), rec)
	if err != nil {
		t.Fatal(err)
	}
	mustVerify(t, f, recRaw, st)
}

// 同一情形，但客户端从没收到那份吊销列表，只靠发版内置吊销主根：主根名下的位置照样作废，序号下限只剩内置值。
// 所以发版写内置吊销根时必须同时抬高内置下限（spec 3.3），被取代的旧状态密钥才不会复活。
func TestBuiltinRevokedPrimaryKeepsOlderStatusKeysObsolete(t *testing.T) {
	f := dirtest.New(t)
	// 客户端见过主根授权的状态密钥（序号 200）签的列表。
	st := mustVerify(t, f, listBy(t, f, f.Primary, directory.SignerPrimary, 200), directory.State{}).NewState

	// 发版：内置吊销主根，内置下限抬到恢复用的新状态密钥的序号 201。
	f.Roots.RevokedRoots = []string{f.RootFP(directory.SignerPrimary)}
	f.Roots.MinStatusSerial = 201

	// 更早由备用根授权、早已被取代的状态密钥（序号 100）签的列表：拒收。
	_, err := verify(t, f, listBy(t, f, f.Backup, directory.SignerBackup, 100), st)
	wantErr(t, err, directory.ErrRollback, "status_key")
	// 恢复用的新状态密钥照常可用。
	mustVerify(t, f, listBy(t, f, f.Backup, directory.SignerBackup, 201), st)
}
