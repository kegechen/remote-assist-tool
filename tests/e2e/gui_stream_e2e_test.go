package e2e

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/gui"
)

// TestGUIExecStreamEndToEnd 验最后一公里：浏览器打到 GUI 的 /api/exec/stream 上，能不能
// 边跑边收到输出。前面的 e2e 只证到 MCP 通知抵达 host；这条把 GUI 后端（MCPClient 消费
// 通知 → NDJSON 流回浏览器）也串进来，即用户实际点终端时走的那条路。
func TestGUIExecStreamEndToEnd(t *testing.T) {
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
	certs := filepath.Join(dir, "certs")
	if err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).Run(); err != nil {
		t.Fatalf("gen-certs: %v", err)
	}
	relayCmd := exec.Command(relayBin,
		"--listen", ":18451",
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

	shareCmd := exec.Command(remoteBin, "share",
		"--server", "localhost:18451", "--insecure", "--p2p", "disabled")
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

	// GUI 后端跑在进程内（GUI 二进制只是多了自动开浏览器，不适合测试）
	guiSrv := gui.NewServer(remoteBin, "localhost:18451")
	ts := httptest.NewServer(guiSrv.Routes())
	defer ts.Close()

	// 像真实前端那样带上令牌：API 无令牌一律 403（防任意网页 CSRF 触发远端命令执行）
	post := func(path string, body any) (*http.Response, error) {
		b, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Auth-Token", guiSrv.Token())
		return http.DefaultClient.Do(req)
	}

	// 1. 连接
	resp, err := post("/api/connect", map[string]any{"code": code, "server": "localhost:18451"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var connRes struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&connRes)
	resp.Body.Close()
	if !connRes.OK {
		t.Fatalf("connect 失败: %s\nshare:\n%s\nrelay:\n%s", connRes.Error, shareOut.String(), relayOut.String())
	}

	// 2. 流式跑命令，边读边记时刻
	resp, err = post("/api/exec/stream", map[string]any{"argv": stagedArgv()})
	if err != nil {
		t.Fatalf("exec stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := bufio.NewReader(resp.Body).ReadString('\n')
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("Content-Type=%q，想要 application/x-ndjson", ct)
	}

	var firstAt, resultAt time.Time
	var text string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Type   string          `json:"type"`
			Stream string          `json:"stream"`
			Data   string          `json:"data"`
			Error  string          `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("NDJSON 行解析失败 %q: %v", line, err)
		}
		switch ev.Type {
		case "chunk":
			b, err := base64.StdEncoding.DecodeString(ev.Data)
			if err != nil {
				t.Fatalf("块数据不是 base64: %v", err)
			}
			text += string(b)
			if firstAt.IsZero() && strings.Contains(text, "first") {
				firstAt = time.Now()
			}
		case "error":
			t.Fatalf("流式 exec 报错: %s\nshare:\n%s", ev.Error, shareOut.String())
		case "result":
			resultAt = time.Now()
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读流: %v", err)
	}

	if resultAt.IsZero() {
		t.Fatalf("没收到收尾的 result 事件。已收到=%q", text)
	}
	if firstAt.IsZero() {
		t.Fatalf("一个 chunk 都没收到（GUI 这一段没接上）。已收到=%q\nshare:\n%s", text, shareOut.String())
	}
	if lead := resultAt.Sub(firstAt); lead < 500*time.Millisecond {
		t.Fatalf("第一块只比 result 早 %v（命令中途睡了 1.5s）：GUI 这里把流攒住了", lead)
	}
	if !strings.Contains(text, "first") || !strings.Contains(text, "second") {
		t.Fatalf("输出不全: %q", text)
	}
	t.Logf("GUI 流式贯通：第一块比 result 早 %v 到达；输出 %q", resultAt.Sub(firstAt), text)
}
