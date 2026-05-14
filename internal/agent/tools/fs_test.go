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
