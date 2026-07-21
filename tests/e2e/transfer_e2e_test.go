package e2e

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2eChunk 与 internal/client.fileTransferChunk 对齐（该常量私有，这里复制一份用于造跨多 chunk 的大文件）。
const e2eChunk = 512 * 1024

func md5hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

func waitFor(buf *lockedBuffer, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return true
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

// startRelay 起一个本地 relay，返回 cmd（调用方负责 kill）与输出 buffer。
func startRelay(t *testing.T, relayBin, certsDir, listen string) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	cmd := exec.Command(relayBin, "--listen", listen,
		"--cert", filepath.Join(certsDir, "server.crt"),
		"--key", filepath.Join(certsDir, "server.key"), "--stun", "")
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	return cmd, out
}

// callTool 发一条 tools/call。
func callTool(t *testing.T, stdin io.Writer, id int, name string, args map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	send(t, stdin, string(b))
}

// TestLargeFileUploadDownloadProgress 起本地 relay+share+help，真实进程传大文件（跨多 chunk +
// 并发窗口），验证 upload/download 字节级完整性（md5）与 stderr 进度日志。
func TestLargeFileUploadDownloadProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	wd, _ := os.Getwd()
	repo := filepath.Clean(filepath.Join(wd, "..", ".."))
	relayBin := relayBinPath(repo)
	remoteBin := cliBin(repo)
	for _, p := range []string{relayBin, remoteBin} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary not built: %s (run go build first)", p)
		}
	}

	dir := t.TempDir()
	// 大文件：12 chunk + 零头，跨并发窗口(8)
	srcData := make([]byte, e2eChunk*12+13579)
	for i := range srcData {
		srcData[i] = byte((i*131 + 7) % 251)
	}
	srcPath := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(srcPath, srcData, 0644); err != nil {
		t.Fatal(err)
	}
	wantMD5 := md5hex(srcData)

	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd, relayOut := startRelay(t, relayBin, certs, ":18447")
	defer func() { relayCmd.Process.Kill(); relayCmd.Wait() }()

	remoteDir := filepath.Join(dir, "remote")
	os.MkdirAll(remoteDir, 0755)
	shareCmd := exec.Command(remoteBin, "share", "--server", "localhost:18447", "--insecure", "--root", remoteDir, "--p2p", "disabled")
	shareOut := &lockedBuffer{}
	shareCmd.Stdout, shareCmd.Stderr = shareOut, shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	defer func() { shareCmd.Process.Kill(); shareCmd.Wait() }()
	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("no code:\n%s", shareOut.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, remoteBin, "help", "--server", "localhost:18447", "--insecure", "--mcp-stdio", "--p2p", "disabled")
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

	send(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	time.Sleep(200 * time.Millisecond)
	callTool(t, stdin, 2, "connect", map[string]any{"code": code})
	if !waitFor(stdoutBuf, "connected", 10*time.Second) {
		t.Fatalf("connect failed:\n%s\nshare:\n%s\nrelay:\n%s", stdoutBuf.String(), shareOut.String(), relayOut.String())
	}

	// upload
	remoteFile := filepath.Join(remoteDir, "big.bin")
	callTool(t, stdin, 3, "upload_file", map[string]any{"local_path": srcPath, "remote_path": remoteFile})
	if !waitFor(stdoutBuf, `"id":3`, 60*time.Second) {
		t.Fatalf("upload no response:\n%s\nhelp-stderr:\n%s", stdoutBuf.String(), helpErr.String())
	}
	got, err := os.ReadFile(remoteFile)
	if err != nil {
		t.Fatalf("read remote file: %v", err)
	}
	if md5hex(got) != wantMD5 {
		t.Fatalf("upload md5 mismatch: remote size=%d want=%d", len(got), len(srcData))
	}
	t.Logf("upload OK: %d bytes md5=%s", len(got), wantMD5)

	// download 回来
	dlPath := filepath.Join(dir, "dl.bin")
	callTool(t, stdin, 4, "download_file", map[string]any{"local_path": dlPath, "remote_path": remoteFile})
	if !waitFor(stdoutBuf, `"id":4`, 60*time.Second) {
		t.Fatalf("download no response:\n%s\nhelp-stderr:\n%s", stdoutBuf.String(), helpErr.String())
	}
	dlData, err := os.ReadFile(dlPath)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if md5hex(dlData) != wantMD5 {
		t.Fatalf("download md5 mismatch: got size=%d want=%d", len(dlData), len(srcData))
	}
	t.Logf("download OK: md5=%s", wantMD5)

	// 进度日志验证
	if !strings.Contains(helpErr.String(), "[upload]") {
		t.Errorf("缺少 upload 进度日志:\n%s", helpErr.String())
	}
	if !strings.Contains(helpErr.String(), "[download]") {
		t.Errorf("缺少 download 进度日志:\n%s", helpErr.String())
	}
}

// TestUploadResumeAfterRelayRestart 真实断网恢复：upload 成功 → kill relay（断网）→
// upload 应快速失败(tunnel_lost,不挂死) → 重启 relay、share 自动重连出新码、help 重新
// connect → 续传/重传成功、字节完整。覆盖"断网重连续传"链路。
func TestUploadResumeAfterRelayRestart(t *testing.T) {
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

	dir := t.TempDir()
	srcData := make([]byte, e2eChunk*8+999)
	for i := range srcData {
		srcData[i] = byte((i*37 + 11) % 251)
	}
	srcPath := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(srcPath, srcData, 0644); err != nil {
		t.Fatal(err)
	}
	wantMD5 := md5hex(srcData)

	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd, _ := startRelay(t, relayBin, certs, ":18448")
	relayKilled := false
	defer func() {
		if !relayKilled {
			relayCmd.Process.Kill()
			relayCmd.Wait()
		}
	}()

	remoteDir := filepath.Join(dir, "remote")
	os.MkdirAll(remoteDir, 0755)
	shareCmd := exec.Command(remoteBin, "share", "--server", "localhost:18448", "--insecure", "--root", remoteDir, "--p2p", "disabled")
	shareOut := &lockedBuffer{}
	shareCmd.Stdout, shareCmd.Stderr = shareOut, shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	defer func() { shareCmd.Process.Kill(); shareCmd.Wait() }()
	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("no code:\n%s", shareOut.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, remoteBin, "help", "--server", "localhost:18448", "--insecure", "--mcp-stdio", "--p2p", "disabled")
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

	send(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	time.Sleep(200 * time.Millisecond)
	callTool(t, stdin, 2, "connect", map[string]any{"code": code})
	if !waitFor(stdoutBuf, "connected", 10*time.Second) {
		t.Fatalf("connect failed:\n%s\nshare:\n%s", stdoutBuf.String(), shareOut.String())
	}

	// 1) 断网：kill relay
	relayCmd.Process.Kill()
	relayCmd.Wait()
	relayKilled = true
	time.Sleep(500 * time.Millisecond)

	// 2) 断网中 upload → 应快速失败（tunnel_lost / not_connected），不挂死
	remoteFile := filepath.Join(remoteDir, "f.bin")
	callTool(t, stdin, 3, "upload_file", map[string]any{"local_path": srcPath, "remote_path": remoteFile})
	if !waitFor(stdoutBuf, `"id":3`, 30*time.Second) {
		t.Fatalf("断网 upload 未在 30s 内返回(疑似挂死):\nhelp-stderr:\n%s", helpErr.String())
	}
	t.Logf("断网 upload 已快速返回(失败,符合预期)")

	// 3) 重启 relay（同端口），share 自动重连并打出新码
	shareLenBefore := len(shareOut.String())
	relayCmd2, _ := startRelay(t, relayBin, certs, ":18448")
	defer func() { relayCmd2.Process.Kill(); relayCmd2.Wait() }()

	// 等 share 重连后打印的新码
	var newCode string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		tail := shareOut.String()
		if len(tail) > shareLenBefore {
			if c := extractCode(tail[shareLenBefore:]); c != "" {
				newCode = c
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if newCode == "" {
		t.Fatalf("relay 重启后 share 未打印新码:\n%s", shareOut.String())
	}

	// 4) help 重新 connect 新码（旧 bridge 已 teardown，不应 already-has-helper）
	callTool(t, stdin, 4, "connect", map[string]any{"code": newCode})
	if !waitFor(stdoutBuf, `"id":4`, 15*time.Second) {
		t.Fatalf("重连 connect 无响应:\n%s", stdoutBuf.String())
	}
	if strings.Contains(lastResponse(stdoutBuf.String(), `"id":4`), "already has helper") {
		t.Fatalf("重连撞 already-has-helper(Bug1 回归):\n%s", stdoutBuf.String())
	}

	// 5) 重传 upload → 完整
	callTool(t, stdin, 5, "upload_file", map[string]any{"local_path": srcPath, "remote_path": remoteFile})
	if !waitFor(stdoutBuf, `"id":5`, 60*time.Second) {
		t.Fatalf("重连后 upload 无响应:\n%s\nhelp-stderr:\n%s", stdoutBuf.String(), helpErr.String())
	}
	got, err := os.ReadFile(remoteFile)
	if err != nil {
		t.Fatalf("read remote: %v", err)
	}
	if md5hex(got) != wantMD5 {
		t.Fatalf("重传后 md5 不符: got size=%d want=%d", len(got), len(srcData))
	}
	t.Logf("断网重连后重传完整: md5=%s", wantMD5)
}

// extractCode 从 share 输出片段里抓协助码（形如 ABCD-EFGHIJ）。
func extractCode(s string) string {
	for _, line := range strings.Split(s, "\n") {
		for _, tok := range strings.Fields(line) {
			tok = strings.TrimSpace(tok)
			if len(tok) == 11 && tok[4] == '-' {
				return tok
			}
		}
	}
	return ""
}

// lastResponse 返回包含 marker 的那段输出（粗略，用于断言错误信息）。
func lastResponse(out, marker string) string {
	idx := strings.LastIndex(out, marker)
	if idx < 0 {
		return ""
	}
	return out[idx:]
}
