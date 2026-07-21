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
	relayBin := relayBinPath(repo)
	remoteBin := cliBin(repo)
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

// TestMCPConcurrentNotHeadOfLineBlocked 验证并发分发：一条慢 exec 不阻塞后续快调用。
// 旧实现 MCP server 串行处理 stdin，慢调用会卡住整个读循环；修复后快调用应先返回。
func TestMCPConcurrentNotHeadOfLineBlocked(t *testing.T) {
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
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0644)

	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd := exec.Command(relayBin, "--listen", ":18445",
		"--cert", filepath.Join(certs, "server.crt"), "--key", filepath.Join(certs, "server.key"), "--stun", "")
	relayOut := &lockedBuffer{}
	relayCmd.Stdout = relayOut
	relayCmd.Stderr = relayOut
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	defer func() { relayCmd.Process.Kill(); relayCmd.Wait() }()
	time.Sleep(600 * time.Millisecond)

	shareCmd := exec.Command(remoteBin, "share", "--server", "localhost:18445", "--insecure", "--root", dir, "--p2p", "disabled")
	shareOut := &lockedBuffer{}
	shareCmd.Stdout = shareOut
	shareCmd.Stderr = shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	defer func() { shareCmd.Process.Kill(); shareCmd.Wait() }()

	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("no code:\n%s", shareOut.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, remoteBin, "help", "--server", "localhost:18445", "--insecure", "--mcp-stdio", "--p2p", "disabled")
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

	connCall, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "connect", "arguments": map[string]any{"code": code}},
	})
	send(t, stdin, string(connCall))
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdoutBuf.String(), "connected") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(stdoutBuf.String(), "connected") {
		t.Fatalf("connect failed:\n%s\nshare:\n%s", stdoutBuf.String(), shareOut.String())
	}

	// 慢 exec（id=10，ping ~4s）紧接着快 stat（id=11）
	slow, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 10, "method": "tools/call",
		"params": map[string]any{"name": "exec", "arguments": map[string]any{"argv": []string{"ping", "-n", "5", "127.0.0.1"}}},
	})
	quick, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 11, "method": "tools/call",
		"params": map[string]any{"name": "stat", "arguments": map[string]any{"path": filepath.Join(dir, "hello.txt")}},
	})
	send(t, stdin, string(slow))
	send(t, stdin, string(quick))

	// 快调用必须先于慢调用返回：轮询直到看到 id=11，此刻 id=10 应仍未出现。
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out := stdoutBuf.String()
		has11 := strings.Contains(out, `"id":11`)
		has10 := strings.Contains(out, `"id":10`)
		if has11 && !has10 {
			t.Logf("快调用(id=11)先于慢调用(id=10)返回——并发分发生效")
			return // PASS
		}
		if has10 && !has11 {
			t.Fatalf("慢调用(id=10)先返回、快调用(id=11)被阻塞——疑似仍串行(head-of-line blocking)\nstdout:\n%s", out)
		}
		time.Sleep(80 * time.Millisecond)
	}
	t.Fatalf("超时未观察到 id=11 先返回\nstdout:\n%s\nhelp-stderr:\n%s\nshare:\n%s", stdoutBuf.String(), helpErr.String(), shareOut.String())
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// binDir 返回存放 relay/remote 二进制的目录；默认 repo/bin，可用 RAT_E2E_BIN_DIR
// 覆盖（用于指向临时构建目录，避免覆盖正在运行中的 bin/remote.exe）。
func binDir(repo string) string {
	if d := os.Getenv("RAT_E2E_BIN_DIR"); d != "" {
		return d
	}
	return filepath.Join(repo, "bin")
}

// 产物名带 os-arch 后缀（见 build.sh / build.bat），这里照着拼——写死 "remote.exe"
// 会让 e2e 在找不到文件时**静默 skip**，看起来像全绿，其实一条都没跑。
func cliBin(repo string) string {
	return filepath.Join(binDir(repo), "remote-assist-cli-"+runtime.GOOS+"-"+runtime.GOARCH+exeExt())
}

func relayBinPath(repo string) string {
	return filepath.Join(binDir(repo), "remote-assist-relay-"+runtime.GOOS+"-"+runtime.GOARCH+exeExt())
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

// TestMCPBootstrapEndToEnd 验证 bootstrap 模式：help 不带 --code 启动，
// Claude 通过 connect 工具喂码后能调 read_file 拿到内容。
func TestMCPBootstrapEndToEnd(t *testing.T) {
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
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("bootstrap-world"), 0644)

	// 1. relay
	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd := exec.Command(relayBin,
		"--listen", ":18444",
		"--cert", filepath.Join(certs, "server.crt"),
		"--key", filepath.Join(certs, "server.key"),
		"--stun", "",
	)
	relayOut := &lockedBuffer{}
	relayCmd.Stdout = relayOut
	relayCmd.Stderr = relayOut
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	defer func() { relayCmd.Process.Kill(); relayCmd.Wait() }()
	time.Sleep(600 * time.Millisecond)

	// 2. share
	shareCmd := exec.Command(remoteBin, "share",
		"--server", "localhost:18444", "--insecure", "--root", dir, "--p2p", "disabled")
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
	t.Logf("got code: %s", code)

	// 3. help-mcp-stdio 不带 --code（bootstrap）
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, remoteBin, "help",
		"--server", "localhost:18444", "--insecure", "--mcp-stdio", "--p2p", "disabled")
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

	// 4. MCP initialize
	send(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	time.Sleep(200 * time.Millisecond)

	// 4.1 在 connect 之前调 read_file 应该返 not_connected
	preCall := map[string]any{
		"jsonrpc": "2.0", "id": 99, "method": "tools/call",
		"params": map[string]any{
			"name":      "read_file",
			"arguments": map[string]any{"path": filepath.Join(dir, "hello.txt")},
		},
	}
	pb, _ := json.Marshal(preCall)
	send(t, stdin, string(pb))
	time.Sleep(500 * time.Millisecond)
	if !strings.Contains(stdoutBuf.String(), "not_connected") {
		t.Fatalf("expected not_connected error before connect.\nstdout:\n%s", stdoutBuf.String())
	}

	// 4.2 connect with code
	connCall := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "connect",
			"arguments": map[string]any{"code": code},
		},
	}
	cb, _ := json.Marshal(connCall)
	send(t, stdin, string(cb))

	// 等 connect 完成（结果经 MCP text 包装后 JSON 会被转义，匹配 "connected" 关键词）
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdoutBuf.String(), "connected") && strings.Contains(stdoutBuf.String(), "true") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(stdoutBuf.String(), "connected") {
		t.Fatalf("connect did not succeed.\nstdout:\n%s\nhelp-stderr:\n%s\nshare:\n%s",
			stdoutBuf.String(), helpErr.String(), shareOut.String())
	}

	// 4.3 真正 read_file
	rfCall := map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "read_file",
			"arguments": map[string]any{"path": filepath.Join(dir, "hello.txt")},
		},
	}
	rfb, _ := json.Marshal(rfCall)
	send(t, stdin, string(rfb))

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := stdoutBuf.String()
		if strings.Contains(out, "bootstrap-world") || strings.Contains(out, "Ym9vdHN0cmFwLXdvcmxk") {
			return // PASS
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("read_file via bootstrap did not return content within 10s.\nstdout:\n%s\nhelp-stderr:\n%s\nshare:\n%s\nrelay:\n%s",
		stdoutBuf.String(), helpErr.String(), shareOut.String(), relayOut.String())
}
