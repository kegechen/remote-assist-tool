//go:build !remoteassist_e2e

package directory_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// TestReleaseBuildDoesNotUseTestRoots：正式构建（不带 remoteassist_e2e）的内置根绝不能是 dirtest 的测试根。
// 测试根的私钥由公开的种子派生，谁都能拿它签列表；e2e 用的替换只能编进带 tag 的构建（roots_e2e.go）。
func TestReleaseBuildDoesNotUseTestRoots(t *testing.T) {
	for signer, k := range directory.BuiltinRoots().Keys {
		for _, name := range []string{directory.SignerPrimary, directory.SignerBackup} {
			if dirtest.SeedKey(name).Public().(ed25519.PublicKey).Equal(k) {
				t.Errorf("内置根 %s 是 dirtest 的测试根 %s", signer, name)
			}
		}
	}
}
