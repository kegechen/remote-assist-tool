package directory_test

import (
	"crypto/ed25519"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// checkRoots 列出 r 的配置错误。内置根写错了运行时不会报任何错，只会静默失效：公钥长度不对的根被当作没配置，
// 指纹格式不对（大写、带前缀）的吊销谁也匹配不上，吊销了却没换掉的公钥让那个角色从此不能用。
func checkRoots(r directory.Roots) []string {
	var bad []string
	for name, pub := range r.Keys {
		switch {
		case name != directory.SignerPrimary && name != directory.SignerBackup:
			bad = append(bad, fmt.Sprintf("Keys has unknown signer %q", name))
		case len(pub) != ed25519.PublicKeySize:
			bad = append(bad, fmt.Sprintf("Keys[%q] is %d bytes, want %d", name, len(pub), ed25519.PublicKeySize))
		case slices.Contains(r.RevokedRoots, directory.RootFingerprint(pub)):
			bad = append(bad, fmt.Sprintf("Keys[%q] is listed in RevokedRoots; ship a new key for that role", name))
		}
	}
	// 两个角色填成同一把公钥：备用根吊销主根会被客户端当成「根吊销自己」拒收，主根泄露时没法恢复。
	if p, b := r.Keys[directory.SignerPrimary], r.Keys[directory.SignerBackup]; len(p) > 0 && p.Equal(b) {
		bad = append(bad, "Keys has the same public key for primary and backup")
	}
	for _, fp := range r.RevokedRoots {
		if !directory.ValidFingerprint(fp) {
			bad = append(bad, fmt.Sprintf("RevokedRoots entry %q is not a lowercase sha256 hex fingerprint (directory.RootFingerprint)", fp))
		}
	}
	// 发版内置吊销根时必须同时抬高内置状态密钥序号下限（spec 3.3）：新装的客户端只有内置吊销、没有吊销列表声明的下限，
	// 被吊销的根名下的位置作废后，下限只剩内置值，不抬就会重新接受被取代的旧状态密钥。
	// 这里只能挡住「完全忘了抬」；抬到多少按 spec 3.3 取值（relay-admin revoke --root 会打印）。
	if len(r.RevokedRoots) > 0 && r.MinStatusSerial == 0 {
		bad = append(bad, fmt.Sprintf("revokes %d root(s) but leaves MinStatusSerial at 0; raise it as spec 3.3 describes", len(r.RevokedRoots)))
	}
	if slices.Contains(r.RevokedNodes, 0) {
		bad = append(bad, "RevokedNodes contains serial 0")
	}
	return bad
}

func TestBuiltinRoots(t *testing.T) {
	for _, p := range checkRoots(directory.BuiltinRoots()) {
		t.Error("roots.go: " + p)
	}
}

// 内置根必须配齐主、备两把：空着时目录功能整体关闭，只配一把时恢复路径缺角，而这两种都不会在运行时报任何错。
// 发版换公钥时这个测试不用改，只要两个角色都在。
func TestBuiltinRootsHaveBothRoles(t *testing.T) {
	r := directory.BuiltinRoots()
	for _, name := range []string{directory.SignerPrimary, directory.SignerBackup} {
		if len(r.Keys[name]) != ed25519.PublicKeySize {
			t.Errorf("roots.go: Keys[%q] 没配置；用 relay-admin keygen 生成后把公钥填进 builtinRoots", name)
		}
	}
}

// checkRoots 自己得真能查出问题，否则 TestBuiltinRoots 形同虚设（内置根现在还是空的）。
func TestCheckRootsCatchesMistakes(t *testing.T) {
	pub := dirtest.SeedKey("root-a").Public().(ed25519.PublicKey)
	other := dirtest.SeedKey("root-b").Public().(ed25519.PublicKey)
	third := dirtest.SeedKey("root-c").Public().(ed25519.PublicKey)
	fp := directory.RootFingerprint(pub)
	keys := func(name string, k ed25519.PublicKey) map[string]ed25519.PublicKey {
		return map[string]ed25519.PublicKey{name: k}
	}
	cases := map[string]directory.Roots{
		"uppercase fingerprint":     {RevokedRoots: []string{strings.ToUpper(fp)}, MinStatusSerial: 1},
		"prefixed fingerprint":      {RevokedRoots: []string{"sha256:" + fp}, MinStatusSerial: 1},
		"short fingerprint":         {RevokedRoots: []string{fp[:63]}, MinStatusSerial: 1},
		"short key":                 {Keys: keys(directory.SignerPrimary, pub[:31])},
		"unknown signer":            {Keys: keys("Primary", pub)},
		"revoked key still shipped": {Keys: keys(directory.SignerBackup, pub), RevokedRoots: []string{fp}, MinStatusSerial: 1},
		"no status floor":           {RevokedRoots: []string{fp}},
		"node serial 0":             {RevokedNodes: []uint64{7, 0}},
		// 同一把公钥粘贴了两次：备用根吊销主根时会被当成「根吊销自己」拒收，主根泄露的恢复路径整个失效。
		"same key for both roles": {Keys: map[string]ed25519.PublicKey{directory.SignerPrimary: pub, directory.SignerBackup: pub}},
	}
	for name, r := range cases {
		if len(checkRoots(r)) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	good := directory.Roots{
		Keys:            map[string]ed25519.PublicKey{directory.SignerPrimary: other, directory.SignerBackup: third},
		MinStatusSerial: 5,
		RevokedNodes:    []uint64{7},
		RevokedRoots:    []string{fp},
	}
	if p := checkRoots(good); len(p) > 0 {
		t.Errorf("valid roots flagged: %v", p)
	}
	if p := checkRoots(directory.Roots{}); len(p) > 0 {
		t.Errorf("empty roots (feature off) flagged: %v", p)
	}
}
