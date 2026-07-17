package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stagedArgv 一条"先吐一行、停一会儿、再吐一行"的命令。中间那段停顿是断言的支点：
// 只要第一行在响应之前就到了，就证明输出是边跑边推的，而不是攒到结束一次性倒出来。
func stagedArgv() []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell", "-NoProfile", "-Command",
			"Write-Output first; Start-Sleep -Milliseconds 1500; Write-Output second"}
	}
	return []string{"sh", "-c", "echo first; sleep 1.5; echo second"}
}

// streamState 把 MCP stdout 上的行分成两类：流式块通知（累积解码后的文本）与本次调用的响应。
type streamState struct {
	text    string // 已解码的流式输出
	sawResp bool   // 本次调用的响应帧已出现
}

// scanStream 解析 MCP server 到目前为止写出的所有行，挑出 reqID 对应的块与响应。
// 只认完整行；最后一行可能还没写完，直接丢掉重来（下一轮 poll 会拿到完整的）。
func scanStream(out string, reqID float64) streamState {
	var st streamState
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var f struct {
			ID     *float64 `json:"id"`
			Method string   `json:"method"`
			Params struct {
				RequestID float64 `json:"requestId"`
				Stream    string  `json:"stream"`
				Data      string  `json:"data"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &f) != nil {
			continue // 半行/非 JSON
		}
		switch {
		case f.Method == "notifications/remote-assist/stream" && f.Params.RequestID == reqID:
			if b, err := base64.StdEncoding.DecodeString(f.Params.Data); err == nil {
				st.text += string(b)
			}
		case f.ID != nil && *f.ID == reqID:
			st.sawResp = true
		}
	}
	return st
}

// TestExecStreamEndToEnd 打通全链路验流式终端：真 relay + 真 share + 真 help(--mcp-stdio)。
// exec 的输出要在命令还没跑完时就一路 share→隧道→bridge→MCP 通知地到达 host——单测各自
// 覆盖了每一层，但两层之间的线接错（比如块的加密格式对不上，bridge 会静默丢光）只有整条
// 跑通才发现得了。
func TestExecStreamEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	wd, _ := os.Getwd()
	repo := filepath.Clean(filepath.Join(wd, "..", ".."))
	relayBin := filepath.Join(binDir(repo), "relay"+exeExt())
	remoteBin := filepath.Join(binDir(repo), "remote"+exeExt())
	for _, p := range []string{relayBin, remoteBin} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary not built: %s", p)
		}
	}

	dir := t.TempDir()

	// 1. relay
	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd := exec.Command(relayBin,
		"--listen", ":18450",
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
		"--server", "localhost:18450", "--insecure", "--p2p", "disabled")
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

	// 3. help --mcp-stdio（bootstrap，与 GUI 用的是同一条路径）
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	helpCmd := exec.CommandContext(ctx, remoteBin, "help",
		"--server", "localhost:18450", "--insecure", "--mcp-stdio", "--p2p", "disabled")
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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdoutBuf.String(), "connected") && strings.Contains(stdoutBuf.String(), "true") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(stdoutBuf.String(), "connected") {
		t.Fatalf("connect 没成功。\nstdout:\n%s\nhelp-stderr:\n%s\nshare:\n%s",
			stdoutBuf.String(), helpErr.String(), shareOut.String())
	}

	// 4. 流式 exec
	execCall, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "exec",
			"arguments": map[string]any{"argv": stagedArgv(), "stream": true},
		},
	})
	send(t, stdin, string(execCall))

	// 每 50ms 看一眼：分别记下"第一行到达"与"响应到达"的时刻。命令中途睡 1.5s，
	// 所以真流式下两者应相隔约 1.5s；若是攒完才发，两者会几乎同时出现。
	var firstAt, respAt time.Time
	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		st := scanStream(stdoutBuf.String(), 3)
		if firstAt.IsZero() && strings.Contains(st.text, "first") {
			firstAt = time.Now()
		}
		if st.sawResp {
			respAt = time.Now()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	final := scanStream(stdoutBuf.String(), 3)
	if respAt.IsZero() {
		t.Fatalf("25s 内没等到 exec 的响应。\nstdout:\n%s\nhelp-stderr:\n%s\nshare:\n%s",
			stdoutBuf.String(), helpErr.String(), shareOut.String())
	}
	if firstAt.IsZero() {
		t.Fatalf("一个流式块都没收到（隧道到 bridge 之间可能断了）。\n累积流式输出=%q\nstdout:\n%s\nshare:\n%s",
			final.text, stdoutBuf.String(), shareOut.String())
	}
	if lead := respAt.Sub(firstAt); lead < 500*time.Millisecond {
		t.Fatalf("第一行只比响应早 %v（命令中途睡了 1.5s，真流式应约 1.5s）：看起来是攒完才发的", lead)
	}
	// 两行都要到齐，确认收尾的块没被丢
	if !strings.Contains(final.text, "first") || !strings.Contains(final.text, "second") {
		t.Fatalf("流式输出不全: %q（想要同时含 first 与 second）", final.text)
	}
	t.Logf("流式贯通：第一行比最终响应早 %v 到达；累积输出 %q", respAt.Sub(firstAt), final.text)
}
