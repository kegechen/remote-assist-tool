//go:build remoteassist_e2e

package directory

import (
	"crypto/ed25519"
	"crypto/sha256"
)

// 只编进 e2e 测试构建（tests/e2e 用 -tags remoteassist_e2e 自己编的 remote 和 relay）：内置根换成 dirtest 的测试根，
// e2e 才能用测试私钥签出真二进制认的列表。派生方式与 dirtest.SeedKey 相同（这里不能 import dirtest，它依赖本包），
// TestE2EBuildUsesTestRoots 核对两边一致。正式构建（scripts/build.*）不带这个 tag，TestReleaseBuildDoesNotUseTestRoots 守着。

func init() {
	builtinRoots = Roots{Keys: map[string]ed25519.PublicKey{
		SignerPrimary: e2eSeedPub(SignerPrimary),
		SignerBackup:  e2eSeedPub(SignerBackup),
	}}
}

// e2eSeedPub 返回 dirtest.SeedKey(name) 的公钥。
func e2eSeedPub(name string) ed25519.PublicKey {
	seed := sha256.Sum256([]byte("dirtest-key:" + name))
	return ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
}
