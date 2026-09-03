package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestListDir(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0644)
	os.MkdirAll(filepath.Join(root, "sub"), 0755)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewListDir(sb)
	args, _ := json.Marshal(map[string]any{"path": root})
	out, _ := tool.Run(context.Background(), args, nil)
	var r ListDirResult
	json.Unmarshal(out, &r)
	if len(r.Entries) != 2 {
		t.Fatalf("entries: %+v", r.Entries)
	}
}

func TestStatFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x")
	os.WriteFile(p, []byte("hi"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewStat(sb)
	args, _ := json.Marshal(map[string]any{"path": p})
	out, _ := tool.Run(context.Background(), args, nil)
	var r StatResult
	json.Unmarshal(out, &r)
	if r.Size != 2 || r.Kind != "file" {
		t.Fatalf("got %+v", r)
	}
}

func TestGlob(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "x.go"), nil, 0644)
	os.WriteFile(filepath.Join(root, "y.go"), nil, 0644)
	os.WriteFile(filepath.Join(root, "z.txt"), nil, 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewGlob(sb)
	args, _ := json.Marshal(map[string]any{"pattern": "*.go", "root": root})
	out, _ := tool.Run(context.Background(), args, nil)
	var r GlobResult
	json.Unmarshal(out, &r)
	if len(r.Paths) != 2 {
		t.Fatalf("paths: %+v", r.Paths)
	}
}

// 回归：glob 的 pattern 是先校验 root 再 Join，pattern 里的 ".." 会在 Join 时被一起
// Clean 掉，绕过那次校验（pattern:"../*.txt" 可列出 --root 外的条目）。这是本包唯一一处
// “先校验再拼接”，list_dir/stat/grep 都是拼好再校验。修复后逐个 match 补一次沙箱校验。
//
// 注：--root 本就只是防手滑护栏而非安全边界（见 Sandbox.ResolvePath 注释），这里修的是
// 护栏行为不一致，不是堵安全漏洞。
func TestGlobDoesNotEscapeRootViaPattern(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "inside.txt"), []byte("i"), 0o644)
	os.WriteFile(filepath.Join(base, "outside.txt"), []byte("o"), 0o644)

	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewGlob(sb)

	args, _ := json.Marshal(map[string]any{"pattern": "../*.txt", "root": root})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		// 直接报错也是可接受的收口方式
		return
	}
	var r GlobResult
	json.Unmarshal(out, &r)
	if len(r.Paths) != 0 {
		t.Fatalf("pattern \"../*.txt\" 逃出了 --root，返回 %v", r.Paths)
	}
}

// root 内的正常 glob 不受上面的收口影响。
func TestGlobStillMatchesInsideRoot(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(root, "b.log"), []byte("b"), 0o644)

	tool := NewGlob(agent.NewSandbox(agent.SandboxConfig{Root: root}))
	args, _ := json.Marshal(map[string]any{"pattern": "*.txt", "root": root})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatal(err)
	}
	var r GlobResult
	json.Unmarshal(out, &r)
	if len(r.Paths) != 1 || r.Paths[0] != "a.txt" {
		t.Fatalf("root 内 glob 结果=%v，期望 [a.txt]", r.Paths)
	}
}
