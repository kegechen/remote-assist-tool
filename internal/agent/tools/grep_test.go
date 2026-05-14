package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestGrep(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\nfunc Foo() {}\n"), 0644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package main\nfunc Bar() {}\n"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewGrep(sb)
	args, _ := json.Marshal(map[string]any{"pattern": "Foo", "root": root, "glob": "*.go"})
	out, _ := tool.Run(context.Background(), args, nil)
	var r GrepResult
	json.Unmarshal(out, &r)
	if len(r.Matches) != 1 || r.Matches[0].Line != 2 {
		t.Fatalf("matches: %+v", r.Matches)
	}
}
