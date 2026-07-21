package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

// writeCodeFile 必须把同一份协助码 JSON 写到主 code-file 和 mirror（升级后让 old 原
// --code-file 路径继续刷新）。两份内容应完全一致。
func TestWriteCodeFileMirrorsToBothPaths(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "code.json")
	mirror := filepath.Join(dir, "host", "code.json")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewShareMode(&Config{ServerAddr: "relay:8443"}, "127.0.0.1:22", agent.SandboxConfig{}, primary, mirror)
	s.code = "ABCDEFGHIJ"
	s.expiresAt = time.Unix(1_800_000_000, 0)
	s.writeCodeFile()

	want := map[string]any{"code": "ABCDEFGHIJ", "server": "relay:8443", "expiresAt": float64(1_800_000_000)}
	for _, path := range []string{primary, mirror} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if got["code"] != want["code"] || got["server"] != want["server"] || got["expiresAt"] != want["expiresAt"] {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
}

// mirror 为空时只写主文件，不误建其它文件。
func TestWriteCodeFileWithoutMirror(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "code.json")

	s := NewShareMode(&Config{ServerAddr: "relay:8443"}, "127.0.0.1:22", agent.SandboxConfig{}, primary, "")
	s.code = "ABCDEFGHIJ"
	s.expiresAt = time.Unix(1_800_000_000, 0)
	s.writeCodeFile()

	if _, err := os.Stat(primary); err != nil {
		t.Fatalf("primary code file missing: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the primary code file, got %d entries", len(entries))
	}
}
