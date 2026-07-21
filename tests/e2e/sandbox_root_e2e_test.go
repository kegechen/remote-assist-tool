package e2e

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestShareWithoutRootReachesFilesOutsideCWD 锁住 --root 可选的语义：不传时文件工具不受
// 目录限制。share 的 CWD 与目标文件分处两棵不相干的子树，因此旧的“--root 未设则回退 CWD”
// 行为会让 read_file 返回 path_outside_root，新行为应当读到内容。
func TestShareWithoutRootReachesFilesOutsideCWD(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	wd, _ := os.Getwd()
	repo := filepath.Clean(filepath.Join(wd, "..", ".."))
	relayBin := relayBinPath(repo)
	remoteBin := cliBin(repo)
	for _, p := range []string{relayBin, remoteBin} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary not built: %s", p)
		}
	}

	shareCwd := t.TempDir()
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "outside.txt")
	os.WriteFile(target, []byte("reachable"), 0644)

	certs := filepath.Join(shareCwd, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd := exec.Command(relayBin, "--listen", ":18449",
		"--cert", filepath.Join(certs, "server.crt"), "--key", filepath.Join(certs, "server.key"), "--stun", "")
	relayOut := &lockedBuffer{}
	relayCmd.Stdout = relayOut
	relayCmd.Stderr = relayOut
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	defer func() { relayCmd.Process.Kill(); relayCmd.Wait() }()
	time.Sleep(600 * time.Millisecond)

	// 关键：不传 --root，且把 CWD 钉在与目标文件无关的子树
	shareCmd := exec.Command(remoteBin, "share", "--server", "localhost:18449", "--insecure", "--p2p", "disabled")
	shareCmd.Dir = shareCwd
	shareOut := &lockedBuffer{}
	shareCmd.Stdout = shareOut
	shareCmd.Stderr = shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	defer func() { shareCmd.Process.Kill(); shareCmd.Wait() }()

	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("no code in share output:\n%s", shareOut.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, remoteBin, "help", "--server", "localhost:18449",
		"--insecure", "--code", code, "--mcp-stdio", "--p2p", "disabled")
	stdin, _ := helpCmd.StdinPipe()
	stdout, _ := helpCmd.StdoutPipe()
	helpErr := &lockedBuffer{}
	helpCmd.Stderr = helpErr
	if err := helpCmd.Start(); err != nil {
		t.Fatalf("help start: %v", err)
	}
	defer func() { helpCmd.Process.Kill(); helpCmd.Wait() }()

	stdoutBuf := &lockedBuffer{}
	go io.Copy(stdoutBuf, stdout)
	time.Sleep(800 * time.Millisecond)

	send(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	time.Sleep(300 * time.Millisecond)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "read_file", "arguments": map[string]any{"path": target}},
	}
	b, _ := json.Marshal(call)
	send(t, stdin, string(b))

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out := stdoutBuf.String()
		// "reachable" 的 base64 = cmVhY2hhYmxl
		if strings.Contains(out, "reachable") || strings.Contains(out, "cmVhY2hhYmxl") {
			return // PASS
		}
		if strings.Contains(out, "path_outside_root") {
			t.Fatalf("share without --root must not restrict file access, got path_outside_root:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("read_file outside CWD returned nothing within 15s.\nstdout:\n%s\nhelp-stderr:\n%s\nshare:\n%s",
		stdoutBuf.String(), helpErr.String(), shareOut.String())
}
