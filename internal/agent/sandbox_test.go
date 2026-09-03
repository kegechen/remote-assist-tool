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

// Windows 上文件名大小写不敏感且 `shutdown` 与 `shutdown.exe` 是同一个程序。
// 归一化之前，deny 列表里的 "shutdown" 只拦裸名，`shutdown.exe` / `SHUTDOWN.EXE` /
// 绝对路径写法全部放行——护栏在主平台上形同虚设。
func TestExecPolicyDenyListNormalizesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("扩展名/大小写归一化只在 Windows 生效")
	}
	sb := NewSandbox(SandboxConfig{DenyExec: []string{"rm", "shutdown"}})
	for _, argv0 := range []string{
		"shutdown.exe",
		"SHUTDOWN.EXE",
		"Shutdown.Exe",
		`C:\Windows\System32\shutdown.exe`,
	} {
		if err := sb.CheckExec([]string{argv0, "/s"}); err == nil {
			t.Errorf("expected deny for %q", argv0)
		}
	}
}

// 反向：allow 列表写 "git"，AI 按 Windows 习惯发来 git.exe 不该被误拒。
func TestExecPolicyAllowListNormalizesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("扩展名/大小写归一化只在 Windows 生效")
	}
	sb := NewSandbox(SandboxConfig{AllowExec: []string{"git", "go"}})
	for _, argv0 := range []string{"git.exe", "GIT.EXE", `C:\Program Files\Git\cmd\git.exe`} {
		if err := sb.CheckExec([]string{argv0, "status"}); err != nil {
			t.Errorf("expected allow for %q, got %v", argv0, err)
		}
	}
	if err := sb.CheckExec([]string{"curl.exe"}); err == nil {
		t.Fatal("expected deny for curl.exe not in allowlist")
	}
}

// 配置项本身带扩展名也应归一化，否则 --deny-exec shutdown.exe 拦不住裸 `shutdown`。
func TestExecPolicyNormalizesConfigSide(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("扩展名/大小写归一化只在 Windows 生效")
	}
	sb := NewSandbox(SandboxConfig{DenyExec: []string{"Shutdown.EXE"}})
	if err := sb.CheckExec([]string{"shutdown"}); err == nil {
		t.Fatal("expected deny for shutdown when deny list spells it Shutdown.EXE")
	}
}

// Unix 上大小写与扩展名都有意义，不能跟着归一化：foo.exe 与 foo 是两个不同的文件。
func TestExecPolicyKeepsUnixSemantics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("仅验证非 Windows 语义")
	}
	sb := NewSandbox(SandboxConfig{DenyExec: []string{"rm"}})
	if err := sb.CheckExec([]string{"RM"}); err != nil {
		t.Fatalf("Unix 区分大小写，RM 不该被 rm 的 deny 规则命中，got %v", err)
	}
	if err := sb.CheckExec([]string{"rm.exe"}); err != nil {
		t.Fatalf("Unix 上 rm.exe 是另一个文件，不该被 rm 的 deny 规则命中，got %v", err)
	}
}

// root 内以两点开头的条目是合法的（k8s ConfigMap 挂载就长这样），
// 不能被 HasPrefix(rel, "..") 误判成越界。
func TestSandboxAllowsDotDotPrefixedNameInsideRoot(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root})
	inside := filepath.Join(root, "..data", "cfg.txt")
	if err := os.MkdirAll(filepath.Dir(inside), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ResolvePath(inside); err != nil {
		t.Fatalf("..data 在 root 内，应放行，got %v", err)
	}
}
