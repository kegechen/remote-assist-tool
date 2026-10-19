package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// MCP bootstrap 模式（--mcp-stdio 不带 --code）下的运行时降级。
//
// 这组测试要证明的是：遇到旧版 share 时，**不重启 MCP 进程**也能连上。
// 以前唯一的放宽手段是启动参数 --min-proto=1，而 MCP 进程由宿主（AI 客户端）托管，
// 改参数就得重启宿主或 MCP 服务；报错又指引"加 --min-proto=1 重启"，AI 于是去改配置、
// 另起进程。现在放宽挪到 connect 工具的 min_proto 入参上，只对这一次连接生效。
//
// 需要旧版 share 二进制，准备方式同 legacy_compat_e2e_test.go：
//
//	RAT_LEGACY_SHARE_V009=/tmp/rat009-cli.exe RAT_LEGACY_SHARE_V100=/tmp/rat100-cli.exe \
//	  go test ./tests/e2e/ -run BootstrapConnect -v

// mcpSession 驱动一个 help --mcp-stdio 子进程：按行写 JSON-RPC 请求，按 id 取响应。
type mcpSession struct {
	t      *testing.T
	stdin  io.WriteCloser
	stderr *lockedBuffer

	mu    sync.Mutex
	cond  *sync.Cond
	lines []string
}

func (s *mcpSession) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		s.mu.Lock()
		s.lines = append(s.lines, sc.Text())
		s.cond.Broadcast()
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.cond.Broadcast()
	s.mu.Unlock()
}

// call 发一条请求，等对应 id 的响应行，返回解析后的 result 文本与 error 消息（二者其一非空）。
func (s *mcpSession) call(id int, method string, params any, timeout time.Duration) (resultText, errMsg string) {
	s.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := io.WriteString(s.stdin, string(b)+"\n"); err != nil {
		s.t.Fatalf("write %s: %v", method, err)
	}
	deadline := time.Now().Add(timeout)
	go func() {
		time.Sleep(timeout)
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		for _, line := range s.lines {
			var resp struct {
				ID     json.RawMessage `json:"id"`
				Result *struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(line), &resp) != nil || string(resp.ID) != fmt.Sprint(id) {
				continue
			}
			if resp.Error != nil {
				return "", resp.Error.Message
			}
			if resp.Result != nil && len(resp.Result.Content) > 0 {
				return resp.Result.Content[0].Text, ""
			}
			return line, ""
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%s(id=%d) %v 内无响应\nstderr:\n%s", method, id, timeout, s.stderr.String())
		}
		s.cond.Wait()
	}
}

// startBootstrapAgainstShare 起 relay + 指定 share 二进制，再起一个**不带 --code、不带
// --min-proto** 的新版 help（MCP bootstrap 模式）。返回会话、协助码、relay 地址与 share 根目录。
func startBootstrapAgainstShare(t *testing.T, shareBin string, port int) (*mcpSession, string, string, string) {
	t.Helper()

	wd, _ := os.Getwd()
	repo := filepath.Clean(filepath.Join(wd, "..", ".."))
	relayBin := relayBinPath(repo)
	newCLI := cliBin(repo)
	for _, p := range []string{relayBin, newCLI} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary not built: %s (run `go build` first)", p)
		}
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0644)

	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	server := fmt.Sprintf("localhost:%d", port)
	relayCmd := exec.Command(relayBin, "--listen", fmt.Sprintf(":%d", port),
		"--cert", filepath.Join(certs, "server.crt"), "--key", filepath.Join(certs, "server.key"), "--stun", "")
	relayOut := &lockedBuffer{}
	relayCmd.Stdout, relayCmd.Stderr = relayOut, relayOut
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill(); relayCmd.Wait() })
	time.Sleep(600 * time.Millisecond)

	shareCmd := exec.Command(shareBin, "share",
		"--server", server, "--insecure", "--root", dir, "--p2p", "disabled")
	shareOut := &lockedBuffer{}
	shareCmd.Stdout, shareCmd.Stderr = shareOut, shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	t.Cleanup(func() { shareCmd.Process.Kill(); shareCmd.Wait() })

	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("share 未输出协助码:\n%s", shareOut.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	helpCmd := exec.CommandContext(ctx, newCLI, "help",
		"--server", server, "--insecure", "--trust-new-cert", "--mcp-stdio", "--p2p", "disabled")
	stdin, _ := helpCmd.StdinPipe()
	stdout, _ := helpCmd.StdoutPipe()
	helpErr := &lockedBuffer{}
	helpCmd.Stderr = helpErr
	if err := helpCmd.Start(); err != nil {
		t.Fatalf("help start: %v", err)
	}
	t.Cleanup(func() { helpCmd.Process.Kill(); helpCmd.Wait() })

	s := &mcpSession{t: t, stdin: stdin, stderr: helpErr}
	s.cond = sync.NewCond(&s.mu)
	go s.readLoop(stdout)

	s.call(1, "initialize", map[string]any{}, 5*time.Second)
	return s, code, server, dir
}

func connectParams(code, server string, extra map[string]any) map[string]any {
	args := map[string]any{"code": code, "server": server}
	for k, v := range extra {
		args[k] = v
	}
	return map[string]any{"name": "connect", "arguments": args}
}

// TestBootstrapConnectDowngradeWithoutRestart 核心诉求：同一个 MCP 进程内，
// 先被旧版 share 拒绝，再带 min_proto="1" 重试就能连上，全程不重启。
func TestBootstrapConnectDowngradeWithoutRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	bin := oldShareBin(t, "RAT_LEGACY_SHARE_V009")
	s, code, server, dir := startBootstrapAgainstShare(t, bin, 18471)

	// 1) 默认不降级：必须被拒，且报错要把 AI 引向 connect 的 min_proto，而不是重启。
	_, errMsg := s.call(2, "tools/call", connectParams(code, server, nil), 25*time.Second)
	if errMsg == "" {
		t.Fatalf("默认配置下不该连上 0.0.9\nstderr:\n%s", s.stderr.String())
	}
	if !strings.Contains(errMsg, "min_proto") {
		t.Fatalf("报错应指引 connect 的 min_proto 参数:\n%s", errMsg)
	}
	if strings.Contains(errMsg, "重启") || strings.Contains(errMsg, "--min-proto") {
		t.Fatalf("MCP 模式下报错不应引导重启或改启动参数:\n%s", errMsg)
	}

	// 2) 同一进程内带 min_proto="1" 重试：连上，且结果里明示 v1 兼容模式。
	text, errMsg := s.call(3, "tools/call", connectParams(code, server, map[string]any{"min_proto": "1"}), 25*time.Second)
	if errMsg != "" {
		t.Fatalf("带 min_proto=1 重试应连上 0.0.9: %s\nstderr:\n%s", errMsg, s.stderr.String())
	}
	if !strings.Contains(text, `"proto_version":"1"`) {
		t.Fatalf("connect 结果应标明 proto_version=1:\n%s", text)
	}
	if !strings.Contains(text, `"warning"`) {
		t.Fatalf("v1 兼容模式下 connect 结果应带 warning:\n%s", text)
	}

	// 3) 工具真的能用。
	text, errMsg = s.call(4, "tools/call", map[string]any{
		"name": "read_file", "arguments": map[string]any{"path": filepath.Join(dir, "hello.txt")},
	}, 15*time.Second)
	if errMsg != "" || (!strings.Contains(text, "world") && !strings.Contains(text, "d29ybGQ=")) {
		t.Fatalf("降级后 read_file 失败: text=%q err=%q", text, errMsg)
	}
}

// TestBootstrapConnectMinProto1KeepsV2 connect 上的放宽和启动参数一样，只是下限：
// 对端是 1.0.0（只认 v2）时必须照样谈成 v2，不带 warning。
func TestBootstrapConnectMinProto1KeepsV2(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	bin := oldShareBin(t, "RAT_LEGACY_SHARE_V100")
	s, code, server, _ := startBootstrapAgainstShare(t, bin, 18472)

	text, errMsg := s.call(2, "tools/call", connectParams(code, server, map[string]any{"min_proto": "1"}), 25*time.Second)
	if errMsg != "" {
		t.Fatalf("min_proto=1 不该打断与 1.0.0 的连接: %s\nstderr:\n%s", errMsg, s.stderr.String())
	}
	if !strings.Contains(text, `"proto_version":"2"`) {
		t.Fatalf("与 1.0.0 应谈成 v2:\n%s", text)
	}
	if strings.Contains(text, `"warning"`) || strings.Contains(s.stderr.String(), "降级") {
		t.Fatalf("谈成 v2 不该出现降级警告:\ntext=%s\nstderr=%s", text, s.stderr.String())
	}
}
