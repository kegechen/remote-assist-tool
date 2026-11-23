//go:build windows

package directory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Go 在 Windows 上打开文件时不带 FILE_SHARE_DELETE，开着期间 rename 覆盖它会失败——杀毒软件、索引服务
// 扫描刚写好的文件时就是这样。这种占用很短，写缓存要等一等再试，不能直接报错丢掉这次更新。
func TestWriteFileAtomicWaitsForBriefOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relays.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		f.Close()
		close(released)
	}()
	err = writeFileAtomic(path, []byte("new"))
	<-released
	if err != nil {
		t.Fatalf("writeFileAtomic while the file was briefly open: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
}

// 一直被占着时不能无限等：重试有上限，放弃后返回错误、不留下临时文件。
func TestWriteFileAtomicGivesUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relays.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	if err := writeFileAtomic(path, []byte("new")); err == nil {
		t.Fatal("writeFileAtomic succeeded while the target stayed open")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("gave up after %s", d)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries after giving up, want only the original file", len(entries))
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("content = %q, want old", got)
	}
}
