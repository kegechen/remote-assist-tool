package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestFileMD5(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "image.bin")
	if err := os.WriteFile(path, []byte("remote image"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := NewFileMD5(agent.NewSandbox(agent.SandboxConfig{Root: root}))
	raw, err := tool.Run(context.Background(), json.RawMessage(`{"path":"`+filepath.ToSlash(path)+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got FileMD5Result
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.MD5 != "9b4317da41d54356e04431565b574da0" || got.Size != 12 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestFileMD5HonorsSandbox(t *testing.T) {
	root := t.TempDir()
	tool := NewFileMD5(agent.NewSandbox(agent.SandboxConfig{Root: root}))
	outside := filepath.Join(filepath.Dir(root), "outside.png")
	args, _ := json.Marshal(FileMD5Args{Path: outside})
	if _, err := tool.Run(context.Background(), args, nil); err == nil {
		t.Fatal("expected path outside root to be rejected")
	}
}
