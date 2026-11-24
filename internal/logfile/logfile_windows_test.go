//go:build windows

package logfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// auto 目录被人换成了目录联接（mklink /J，不用管理员权限就能建）时不用它，也不往联接指向的目录写。
func TestOpenRefusesJunction(t *testing.T) {
	o := testOpts(t)
	target := t.TempDir()
	link := filepath.Join(o.tempDir, "remote-assist-"+userTag())
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("建不了目录联接: %v %q", err, out)
	}
	t.Cleanup(func() { os.Remove(link) }) // 只删联接本身
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("目录联接的 Lstat 模式: %v", fi.Mode())
	if f, err := Open(o); f != nil || err == nil {
		t.Fatalf("auto 目录是目录联接: %v, %v", f, err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("不该往目录联接指向的目录写: %v", entries)
	}
}
