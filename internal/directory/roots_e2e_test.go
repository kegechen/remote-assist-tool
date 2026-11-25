//go:build remoteassist_e2e

package directory_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// TestE2EBuildUsesTestRoots：带 remoteassist_e2e 构建时（tests/e2e 自己编的 remote、relay），内置根换成 dirtest 的测试根，
// e2e 才能用同一套私钥签列表；吊销与序号下限清空，免得正式构建将来写进去的条目挡住测试列表。
func TestE2EBuildUsesTestRoots(t *testing.T) {
	r := directory.BuiltinRoots()
	for _, name := range []string{directory.SignerPrimary, directory.SignerBackup} {
		want := dirtest.SeedKey(name).Public().(ed25519.PublicKey)
		if !want.Equal(r.Keys[name]) {
			t.Errorf("内置根 %s 不是 dirtest 的测试根", name)
		}
	}
	if len(r.Keys) != 2 || len(r.RevokedRoots) != 0 || len(r.RevokedNodes) != 0 || r.MinStatusSerial != 0 {
		t.Errorf("测试构建的内置根应只有两把测试根、没有吊销和序号下限，得到 %+v", r)
	}
	f := dirtest.New(t)
	if _, err := directory.Verify(f.Sign(f.Body(f.Entry("A"))), r, directory.State{}, f.Now); err != nil {
		t.Errorf("测试根签的列表验不过: %v", err)
	}
}
