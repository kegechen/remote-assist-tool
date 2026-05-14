package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSandboxAllowsInsideRoot(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root})
	inside := filepath.Join(root, "a", "b.txt")
	os.MkdirAll(filepath.Dir(inside), 0755)
	os.WriteFile(inside, []byte("x"), 0644)
	if _, err := sb.ResolvePath(inside); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}

func TestSandboxRejectsOutside(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root})
	outside := filepath.Join(filepath.Dir(root), "elsewhere.txt")
	if _, err := sb.ResolvePath(outside); err == nil {
		t.Fatal("expected reject for path outside root")
	}
}

func TestSandboxRejectsDotDotEscape(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root})
	escape := filepath.Join(root, "..", "..", "etc")
	if _, err := sb.ResolvePath(escape); err == nil {
		t.Fatal("expected reject for ..-escape")
	}
}

func TestExecPolicyDenyList(t *testing.T) {
	sb := NewSandbox(SandboxConfig{DenyExec: []string{"rm", "shutdown"}})
	if err := sb.CheckExec([]string{"rm", "-rf", "/"}); err == nil {
		t.Fatal("expected deny for rm")
	}
	if err := sb.CheckExec([]string{"ls"}); err != nil {
		t.Fatalf("expected allow ls, got %v", err)
	}
}

func TestExecPolicyAllowList(t *testing.T) {
	sb := NewSandbox(SandboxConfig{AllowExec: []string{"go", "git"}})
	if err := sb.CheckExec([]string{"go", "test"}); err != nil {
		t.Fatalf("expected allow go, got %v", err)
	}
	if err := sb.CheckExec([]string{"curl"}); err == nil {
		t.Fatal("expected deny curl not in allowlist")
	}
}

func TestSandboxUnsafeBypass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path semantics differ on windows; covered separately")
	}
	sb := NewSandbox(SandboxConfig{Unsafe: true})
	if _, err := sb.ResolvePath("/tmp"); err != nil {
		t.Fatalf("unsafe should allow anywhere, got %v", err)
	}
}
