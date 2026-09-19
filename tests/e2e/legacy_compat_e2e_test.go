package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 与真正的旧版客户端（0.0.x，工具协议 v1）互通的端到端验证。
//
// 为什么非要用真二进制：兼容层的全部实现都建立在「0.0.x 当时是怎么做的」这个推断上
// ——不带 AAD 加封、无参调用不发 args、空 result 不封、Hello 只看 version 字段。
// 推断写进代码之后，单测只会照着同一份推断去验证，推错了也是全绿。拿一个真的旧版
// 二进制来握手，是唯一能证伪这些推断的办法。
//
// 需要一个旧版 CLI，路径由 RAT_LEGACY_CLI 指定；没有就跳过（CI 上通常没有）。
// 本地可以这样准备：
//
//	git worktree add /tmp/rat009 0.0.9
//	cd /tmp/rat009 && go build -o /tmp/rat009-cli.exe ./cmd/remote
//	RAT_LEGACY_CLI=/tmp/rat009-cli.exe go test ./tests/e2e/ -run LegacyPeer -v

func legacyCLI(t *testing.T) string {
	t.Helper()
	p := os.Getenv("RAT_LEGACY_CLI")
	if p == "" {
		t.Skip("RAT_LEGACY_CLI 未设置，跳过与旧版客户端的互通验证")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("RAT_LEGACY_CLI 指向的文件不存在: %v", err)
	}
	return p
}

// legacyFixture 起 relay + 新版 share，返回协助码与 share 的输出缓冲。
type legacyFixture struct {
	dir      string
	code     string
	shareOut *lockedBuffer
}

func startLegacyFixture(t *testing.T, shareArgs ...string) *legacyFixture {
	t.Helper()

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
	t.Cleanup(func() {
		relayCmd.Process.Kill()
		relayCmd.Wait()
	})
	time.Sleep(600 * time.Millisecond)

	args := append([]string{"share",
		"--server", "localhost:18444",
		"--insecure",
		"--root", dir,
		"--p2p", "disabled",
	}, shareArgs...)
	shareCmd := exec.Command(remoteBin, args...)
	shareOut := &lockedBuffer{}
	shareCmd.Stdout = shareOut
	shareCmd.Stderr = shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	t.Cleanup(func() {
		shareCmd.Process.Kill()
		shareCmd.Wait()
	})

	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("no code in share output:\n%s", shareOut.String())
	}
	return &legacyFixture{dir: dir, code: code, shareOut: shareOut}
}

// startLegacyHelp 启动旧版 help（MCP stdio），返回其 stdin、stdout/stderr 缓冲与等待退出的函数。
func startLegacyHelp(t *testing.T, f *legacyFixture) (io.WriteCloser, *lockedBuffer, *lockedBuffer, func() error) {
	t.Helper()
	legacyBin := legacyCLI(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	helpCmd := exec.CommandContext(ctx, legacyBin, "help",
		"--server", "localhost:18444",
		"--insecure",
		"--code", f.code,
		"--mcp-stdio",
		"--p2p", "disabled",
	)
	stdin, _ := helpCmd.StdinPipe()
	stdout, _ := helpCmd.StdoutPipe()
	helpErr := &lockedBuffer{}
	helpCmd.Stderr = helpErr
	if err := helpCmd.Start(); err != nil {
		t.Fatalf("legacy help start: %v", err)
	}
	t.Cleanup(func() {
		helpCmd.Process.Kill()
		helpCmd.Wait()
	})

	stdoutBuf := &lockedBuffer{}
	go io.Copy(stdoutBuf, stdout)
	return stdin, stdoutBuf, helpErr, helpCmd.Wait
}

// callReadFile 喂一次 MCP read_file 调用。
//
// 这里刻意不用包内的 send()：它遇到写失败会 t.Fatalf，而握手被拒时旧版 help 会立刻退出、
// stdin 随之关闭，写失败恰恰是**预期结果**而不是测试故障。
func callReadFile(t *testing.T, stdin io.Writer, f *legacyFixture) {
	t.Helper()
	io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	time.Sleep(300 * time.Millisecond)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "read_file",
			"arguments": map[string]any{"path": filepath.Join(f.dir, "hello.txt")},
		},
	}
	b, _ := json.Marshal(call)
	io.WriteString(stdin, string(b)+"\n")
}

func gotFileContent(buf *lockedBuffer) bool {
	out := buf.String()
	return strings.Contains(out, "world") || strings.Contains(out, "d29ybGQ=")
}

// TestLegacyPeerRejectedByDefault 默认配置下，旧版 help 必须连不上，
// 且拿到的是一条**可操作**的提示，而不是干巴巴的版本错误。
//
// 这条提示是旧版用户唯一能看到的线索：它由新版 share 生成、经 HelloAck.ErrorMsg 传过去，
// 由旧版 help 原样打印。旧版代码改不了，所以提示能不能送达完全取决于我们塞进 ErrorMsg 的内容。
func TestLegacyPeerRejectedByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	legacyCLI(t) // 先判断是否跳过，避免白起 relay
	f := startLegacyFixture(t)

	stdin, stdoutBuf, stderrBuf, wait := startLegacyHelp(t, f)
	time.Sleep(1200 * time.Millisecond)
	callReadFile(t, stdin, f)

	// 被拒的 help 会自己退出；等它收场再看 stderr。
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
	}

	if gotFileContent(stdoutBuf) {
		t.Fatal("默认配置下旧版客户端不应连通")
	}
	// 旧版 help 的终端上要看到可操作的办法。这条提示由新版 share 生成、经 HelloAck.ErrorMsg
	// 传过去，由改不动的旧版代码原样打印 —— 能否送达完全取决于我们塞进 ErrorMsg 的内容。
	if !strings.Contains(stderrBuf.String(), "--min-proto=1") {
		t.Fatalf("旧版 help 未收到可操作提示，stderr:\n%s", stderrBuf.String())
	}
	// share 本机也要留痕 —— 那才是能加参数的机器。
	if !strings.Contains(f.shareOut.String(), "--min-proto=1") {
		t.Fatalf("share 本机未打印提示，输出:\n%s", f.shareOut.String())
	}
}

// TestLegacyPeerWorksWithMinProto1 这是整个向下兼容方案的最终证据：
// 开了 --min-proto=1 之后，一个真正的 0.0.x 客户端能完整跑通一次工具调用。
func TestLegacyPeerWorksWithMinProto1(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	legacyCLI(t)
	// 用 --p2p auto 而不是 disabled：那才是真实使用姿势，用户不会特意去关 P2P。
	f := startLegacyFixture(t, "--min-proto=1", "--p2p", "auto")

	stdin, stdoutBuf, stderrBuf, _ := startLegacyHelp(t, f)
	time.Sleep(1200 * time.Millisecond)
	callReadFile(t, stdin, f)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !gotFileContent(stdoutBuf) {
		time.Sleep(100 * time.Millisecond)
	}
	if !gotFileContent(stdoutBuf) {
		t.Fatalf("兼容模式下旧版客户端仍未跑通 read_file\nstdout:\n%s\nstderr:\n%s\nshare:\n%s",
			stdoutBuf.String(), stderrBuf.String(), f.shareOut.String())
	}
	// 降级必须留下醒目告警，否则用户不知道这条通道已经没有 AAD 与抗重放了。
	if !strings.Contains(f.shareOut.String(), "降级") {
		t.Fatalf("share 未就降级发出告警，输出:\n%s", f.shareOut.String())
	}
	// 必须**主动**停掉 P2P，而不是让它去打一轮注定失败的洞。
	//
	// 打洞认证是单向生效的：我们拒绝旧版不带 MAC 的包，但旧版只比对 sessionID、不认识
	// `mac` 字段，会照单全收我们的包并单方面认定 P2P 已通，然后把流量往一条我们这边
	// 没建起来的隧道上送。不发包是唯一能让两端状态保持一致的做法。
	if !strings.Contains(f.shareOut.String(), "已停止 P2P 尝试") {
		t.Fatalf("share 未主动停止 P2P，输出:\n%s", f.shareOut.String())
	}
	// 既然根本没发起，就不该出现打洞超时——那说明还是去打了。
	if strings.Contains(f.shareOut.String(), "P2P connection timed out") {
		t.Fatalf("share 仍在尝试打洞，输出:\n%s", f.shareOut.String())
	}
}

// ── 反方向：新版 help 连旧版 share ────────────────────────────────────────────
//
// 上面那组测的是「新 share + 旧 help」，兼容开关加在 share 端。这组反过来：开关加在
// help 端，走的是 proto.NewHello / NewFallbackHello 的两轮握手。
//
// 之所以必须用**两代**真实二进制而不是只测 0.0.x：兼容锚点是给"只认 Version 字段严格
// 相等"的旧实现看的，而这样的实现有两代，要求的常量却不同——0.0.x 要 "1"，已发布的
// 1.0.0 要 "2"，且两者都没有 Versions 字段。一个锚点值讨好不了两代人。若第一轮就发 "1"
// 去迁就 0.0.x，`--min-proto=1` 这个本意放宽兼容的开关反而会打断所有 1.0.0 对端——
// 比没有这个开关还糟。所以第一轮固定发最高版本，锚点留到第二轮重试。
//
// 需要旧版 share 二进制，路径由 RAT_LEGACY_SHARE_V009 / RAT_LEGACY_SHARE_V100 指定；
// 没有就跳过。本地可以这样准备：
//
//	git worktree add /tmp/rat009 0.0.9 && (cd /tmp/rat009 && go build -o /tmp/rat009-cli.exe ./cmd/remote)
//	git worktree add /tmp/rat100 1.0.0 && (cd /tmp/rat100 && go build -o /tmp/rat100-cli.exe ./cmd/remote)
//	RAT_LEGACY_SHARE_V009=/tmp/rat009-cli.exe RAT_LEGACY_SHARE_V100=/tmp/rat100-cli.exe \
//	  go test ./tests/e2e/ -run NewHelpAgainst -v

// runNewHelpAgainstOldShare 起「旧版 share + 新版 help(--min-proto=1)」，跑一次 read_file。
// 返回 help 的 stdout 与 stderr。
func runNewHelpAgainstOldShare(t *testing.T, oldShareBin string, port int) (stdoutText, stderrText string) {
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
	listen := fmt.Sprintf(":%d", port)
	server := fmt.Sprintf("localhost:%d", port)
	relayCmd := exec.Command(relayBin, "--listen", listen,
		"--cert", filepath.Join(certs, "server.crt"), "--key", filepath.Join(certs, "server.key"), "--stun", "")
	relayOut := &lockedBuffer{}
	relayCmd.Stdout, relayCmd.Stderr = relayOut, relayOut
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { relayCmd.Process.Kill(); relayCmd.Wait() })
	time.Sleep(600 * time.Millisecond)

	// 旧版 share：没有 --min-proto，也没有 --trust-new-cert。
	shareCmd := exec.Command(oldShareBin, "share",
		"--server", server, "--insecure", "--root", dir, "--p2p", "disabled")
	shareOut := &lockedBuffer{}
	shareCmd.Stdout, shareCmd.Stderr = shareOut, shareOut
	if err := shareCmd.Start(); err != nil {
		t.Fatalf("old share start: %v", err)
	}
	t.Cleanup(func() { shareCmd.Process.Kill(); shareCmd.Wait() })

	code := waitCode(t, shareOut, 8*time.Second)
	if code == "" {
		t.Fatalf("旧版 share 未输出协助码:\n%s", shareOut.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, newCLI, "help",
		"--server", server, "--insecure", "--trust-new-cert",
		"--code", code, "--mcp-stdio", "--p2p", "disabled", "--min-proto=1")
	stdin, _ := helpCmd.StdinPipe()
	stdout, _ := helpCmd.StdoutPipe()
	helpErr := &lockedBuffer{}
	helpCmd.Stderr = helpErr
	if err := helpCmd.Start(); err != nil {
		t.Fatalf("new help start: %v", err)
	}
	t.Cleanup(func() { helpCmd.Process.Kill(); helpCmd.Wait() })

	stdoutBuf := &lockedBuffer{}
	go io.Copy(stdoutBuf, stdout)

	time.Sleep(1500 * time.Millisecond)
	io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	time.Sleep(300 * time.Millisecond)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "read_file",
			"arguments": map[string]any{"path": filepath.Join(dir, "hello.txt")},
		},
	}
	b, _ := json.Marshal(call)
	io.WriteString(stdin, string(b)+"\n")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !gotFileContent(stdoutBuf) {
		time.Sleep(100 * time.Millisecond)
	}
	return stdoutBuf.String(), helpErr.String()
}

func oldShareBin(t *testing.T, env string) string {
	t.Helper()
	p := os.Getenv(env)
	if p == "" {
		t.Skipf("%s 未设置，跳过与旧版 share 的互通验证", env)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("%s 指向的文件不存在: %v", env, err)
	}
	return p
}

// TestNewHelpAgainstV100Share --min-proto=1 绝不能打断与**已发布的 1.0.0** 的连接。
//
// 这是最容易被兼容锚点搞反的一处，也是这组测试存在的主要理由：一个放宽兼容的开关
// 若反而断掉当前版本，用户把它写进 MCP 配置后会发现原本能连的对端全连不上了。
// 谈成的必须是 v2（第一轮就被接受），因此不该出现降级告警。
func TestNewHelpAgainstV100Share(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	bin := oldShareBin(t, "RAT_LEGACY_SHARE_V100")
	stdoutText, stderrText := runNewHelpAgainstOldShare(t, bin, 18462)
	if !strings.Contains(stdoutText, "world") && !strings.Contains(stdoutText, "d29ybGQ=") {
		t.Fatalf("--min-proto=1 打断了与 1.0.0 的连接\nstdout:\n%s\nstderr:\n%s", stdoutText, stderrText)
	}
	if strings.Contains(stderrText, "降级") {
		t.Fatalf("与 1.0.0 应谈成 v2，不该降级:\n%s", stderrText)
	}
}

// TestNewHelpAgainstV009Share 对 0.0.x 则要走第二轮兼容锚点，谈成 v1 并给出降级告警。
func TestNewHelpAgainstV009Share(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	bin := oldShareBin(t, "RAT_LEGACY_SHARE_V009")
	stdoutText, stderrText := runNewHelpAgainstOldShare(t, bin, 18463)
	if !strings.Contains(stdoutText, "world") && !strings.Contains(stdoutText, "d29ybGQ=") {
		t.Fatalf("--min-proto=1 未能连通 0.0.9\nstdout:\n%s\nstderr:\n%s", stdoutText, stderrText)
	}
	if !strings.Contains(stderrText, "降级") {
		t.Fatalf("与 0.0.9 应降级到 v1 并告警:\n%s", stderrText)
	}
}
