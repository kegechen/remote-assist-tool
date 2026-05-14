package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMCPEndToEnd 起本地 relay+share+help-mcp-stdio，断 read_file 返回 "world"
func TestMCPEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}

	// 计算工作目录绝对路径与二进制路径
	wd, _ := os.Getwd()
	repo := filepath.Clean(filepath.Join(wd, "..", ".."))
	relayBin := filepath.Join(repo, "bin", "relay"+exeExt())
	remoteBin := filepath.Join(repo, "bin", "remote"+exeExt())
	for _, p := range []string{relayBin, remoteBin} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary not built: %s (run `go build` first)", p)
		}
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0644)

	// 1. 生成自签证书 + 启动 relay
	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd := exec.Command(relayBin,
		"--listen", ":18443",
		"--cert", filepath.Join(certs, "server.crt"),
		"--key", filepath.Join(certs, "server.key"),
		"--stun", "", // 禁用 STUN，避免端口占用
	)
	relayOut := &lockedBuffer{}
	relayCmd.Stdout = relayOut
	relayCmd.Stderr = relayOut
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	defer func() {
		relayCmd.Process.Kill()
		relayCmd.Wait()
	}()
	time.Sleep(600 * time.Millisecond)

	// 2. 启动 share
	shareCmd := exec.Command(remoteBin, "share",
		"--server", "localhost:18443",
		"--insecure",
		"--root", dir,
		"--p2p", "disabled",
	)
	shareOut := &lockedBuffer{}
	shareCmd.Stdout = shareOut
	shareCmd.Stderr = shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	defer func() {
		shareCmd.Process.Kill()
		shareCmd.Wait()
	}()

	// 等协助码出来
	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("no code in share output:\n%s", shareOut.String())
	}
	t.Logf("got code: %s", code)

	// 3. 启动 help-mcp-stdio
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, remoteBin, "help",
		"--server", "localhost:18443",
		"--insecure",
		"--code", code,
		"--mcp-stdio",
		"--p2p", "disabled",
	)
	stdin, _ := helpCmd.StdinPipe()
	stdout, _ := helpCmd.StdoutPipe()
	helpErr := &lockedBuffer{}
	helpCmd.Stderr = helpErr
	if err := helpCmd.Start(); err != nil {
		t.Fatalf("help start: %v", err)
	}
	defer func() {
		helpCmd.Process.Kill()
		helpCmd.Wait()
	}()

	// 后台把 stdout 灌进 buffer
	stdoutBuf := &lockedBuffer{}
	go io.Copy(stdoutBuf, stdout)

	// 稍等 help 与 share 握手完成
	time.Sleep(800 * time.Millisecond)

	// 4. 喂 MCP 调用
	// 4.1 initialize
	send(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	time.Sleep(300 * time.Millisecond)
	// 4.2 tools/call read_file
	call := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "read_file",
			"arguments": map[string]any{"path": filepath.Join(dir, "hello.txt")},
		},
	}
	b, _ := json.Marshal(call)
	send(t, stdin, string(b))

	// 5. 等 stdout 含 "world"（或 base64 编码的 "world" = "d29ybGQ="）
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out := stdoutBuf.String()
		if strings.Contains(out, "world") || strings.Contains(out, "d29ybGQ=") {
			t.Logf("read_file returned expected content")
			return // PASS
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("did not see read_file result containing 'world' within 15s.\nstdout:\n%s\nhelp-stderr:\n%s\nshare:\n%s\nrelay:\n%s",
		stdoutBuf.String(), helpErr.String(), shareOut.String(), relayOut.String())
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func send(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := w.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// codeRE 匹配 "协助码: XXXX-XXXXXX" 格式，提取不含连字符的原始码
var codeRE = regexp.MustCompile(`协助码[:：]?\s*([A-Za-z0-9]{4}-[A-Za-z0-9]+)`)

func waitCode(t *testing.T, buf *lockedBuffer, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := codeRE.FindStringSubmatch(buf.String()); len(m) > 1 {
			// 去掉 formatCode 插入的连字符，还原原始 code
			return strings.ReplaceAll(m[1], "-", "")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

// lockedBuffer 是并发安全的 bytes.Buffer
type lockedBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
