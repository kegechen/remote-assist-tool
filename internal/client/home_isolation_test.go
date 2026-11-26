package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain 把整个包的主目录指到临时目录：help 每次加入都会读、必要时创建 ~/.remote_assist_client_id，
// 节点列表缓存也放在主目录。机器上原本没有 ClientID 文件时，测试生成的那个会被之后的 share 拿去用。
// 需要特定主目录的用例照旧用 t.Setenv 覆盖。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "client-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestPackageHomeIsolated(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	if !strings.HasPrefix(strings.ToLower(home), strings.ToLower(tmp)) {
		t.Fatalf("测试进程的主目录 %q 不在临时目录 %q 下，测试会读写真实主目录", home, tmp)
	}
}
