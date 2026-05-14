package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestTailLogStatic(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "app.log")
	os.WriteFile(p, []byte("line1\nline2\nline3\n"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewTailLog(sb)
	args, _ := json.Marshal(map[string]any{"path": p, "lines": 2})
	sink := &captureSink{}
	_, err := tool.Run(context.Background(), args, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := string(sink.stdout)
	if !strings.Contains(got, "line2") || !strings.Contains(got, "line3") {
		t.Fatalf("got: %q", got)
	}
}

