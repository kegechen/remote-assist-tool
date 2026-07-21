package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestReadFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x.txt")
	os.WriteFile(p, []byte("hello world"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)
	args, _ := json.Marshal(map[string]any{"path": p, "as_text": true})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var r ReadFileResult
	json.Unmarshal(out, &r)
	if string(r.Bytes) != "hello world" || !r.EOF || !r.AsText {
		t.Fatalf("got %+v", r)
	}
}

func TestReadFileOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)
	args, _ := json.Marshal(map[string]any{"path": filepath.Join(filepath.Dir(root), "evil")})
	if _, err := tool.Run(context.Background(), args, nil); err == nil {
		t.Fatal("expected path_outside_root")
	}
}

func TestWriteFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "out.txt")
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewWriteFile(sb)
	args, _ := json.Marshal(map[string]any{"path": p, "content": []byte("yo"), "create": true})
	if _, err := tool.Run(context.Background(), args, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "yo" {
		t.Fatalf("got %q", got)
	}
}
