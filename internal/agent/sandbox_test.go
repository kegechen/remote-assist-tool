package agent

import (
	"os"
	"path/filepath"
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

func TestSandboxEmptyRootAllowsAnywhere(t *testing.T) {
	sb := NewSandbox(SandboxConfig{})
	outside := filepath.Join(t.TempDir(), "..", "elsewhere.txt")
	if _, err := sb.ResolvePath(outside); err != nil {
		t.Fatalf("empty root means no restriction, got %v", err)
	}
}

// 传了 --root 但构造时解析不出绝对路径（getwd 失败等罕见情形）时必须拒绝，
// 不能退化成“无限制”——那会把一个显式的限制请求静默变成放行。
func TestSandboxUnresolvedRootFailsClosed(t *testing.T) {
	sb := &Sandbox{cfg: SandboxConfig{Root: "some/root"}, restricted: true}
	if _, err := sb.ResolvePath(filepath.Join(t.TempDir(), "x.txt")); err == nil {
		t.Fatal("expected reject when --root was requested but could not be resolved")
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

// --unsafe-exec 只放开 exec 名单；显式传入的 --root 仍须生效，否则这个 flag 的名字就是骗人的。
func TestUnsafeExecDoesNotBypassRoot(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root, UnsafeExec: true})
	outside := filepath.Join(filepath.Dir(root), "elsewhere.txt")
	if _, err := sb.ResolvePath(outside); err == nil {
		t.Fatal("--unsafe-exec must not widen the --root path guard")
	}
}

func TestUnsafeExecDropsExecLists(t *testing.T) {
	sb := NewSandbox(SandboxConfig{DenyExec: []string{"rm"}, AllowExec: []string{"go"}, UnsafeExec: true})
	if err := sb.CheckExec([]string{"rm", "-rf", "/"}); err != nil {
		t.Fatalf("--unsafe-exec should drop the deny list, got %v", err)
	}
	if err := sb.CheckExec([]string{"curl"}); err != nil {
		t.Fatalf("--unsafe-exec should drop the allow list, got %v", err)
	}
}
