package gui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildCandidate 交叉编译一个最小 Go 程序充当候选升级包；version 非空时像 scripts/build.sh
// 那样用 -ldflags 注入。程序本身不引用 version 包，-X 只会原样记进构建信息。
func buildCandidate(t *testing.T, goos, version string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("需要调用 go build")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("PATH 里没有 go，无法构建候选包")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module candidate\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "candidate.bin")
	args := []string{"build"}
	if version != "" {
		args = append(args, "-ldflags", "-X github.com/remote-assist/tool/internal/version.Version="+version)
	}
	cmd := exec.Command(goBin, append(args, "-o", out, ".")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=", "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建候选包失败: %v\n%s", err, b)
	}
	return out
}

func TestLocalCandidateVersionReadsLDFlags(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		path := buildCandidate(t, goos, "v1.1.0-3-gabcdef0-dirty")
		if got := localCandidateVersion(path); got != "v1.1.0-3-gabcdef0-dirty" {
			t.Fatalf("%s 候选包本地版本 = %q", goos, got)
		}
	}
	if got := localCandidateVersion(buildCandidate(t, "linux", "")); got != "" {
		t.Fatalf("未注入版本的候选包应返回空串，实际 %q", got)
	}
	if got := localCandidateVersion(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("读不到的文件应返回空串，实际 %q", got)
	}
}

func TestProbeOrderFollowsPeerHost(t *testing.T) {
	cases := map[string]string{
		"Administrator@dkt-aiteacher-window-01 Windows 10 amd64": "windows",
		"kechen12@kechen12 Windows 11 amd64":                     "windows",
		"seewo@seewo-pc kylin amd64":                             "linux",
		"u@windows-box kylin amd64":                              "linux",
		"windows":                                                "linux",
		"":                                                       "linux",
	}
	for host, first := range cases {
		order := probeOrder(host)
		if len(order) != 2 || order[0] != first {
			t.Fatalf("peer_host %q 的探测顺序 = %v，首选应为 %s", host, order, first)
		}
	}
}

type fakeToolCall struct {
	Name      string
	Argv      []string
	TimeoutMS int
}

// fakeRemote 在 fakeServer 上按工具名应答，并按顺序记下每次调用。handle 返回
// (结果 JSON 文本, 错误消息)；错误消息非空时回 JSON-RPC error。
type fakeRemote struct {
	mu    sync.Mutex
	calls []fakeToolCall
}

func (f *fakeRemote) serve(srv *fakeServer, handle func(name string, argv []string) (string, string)) {
	go func() {
		for srv.fromClient.Scan() {
			var req struct {
				ID     int64 `json:"id"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal(srv.fromClient.Bytes(), &req) != nil || req.ID == 0 {
				continue
			}
			call := fakeToolCall{Name: req.Params.Name}
			if ms, ok := req.Params.Arguments["timeout_ms"].(float64); ok {
				call.TimeoutMS = int(ms)
			}
			if raw, ok := req.Params.Arguments["argv"].([]any); ok {
				for _, a := range raw {
					s, _ := a.(string)
					call.Argv = append(call.Argv, s)
				}
			}
			f.mu.Lock()
			f.calls = append(f.calls, call)
			f.mu.Unlock()
			text, errMsg := handle(call.Name, call.Argv)
			var resp map[string]any
			if errMsg != "" {
				resp = map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": errMsg}}
			} else {
				resp = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}}
			}
			b, _ := json.Marshal(resp)
			srv.toClient.Write(append(b, '\n'))
		}
	}()
}

func (f *fakeRemote) snapshot() []fakeToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeToolCall(nil), f.calls...)
}

// called 是否出现过 argv[1] == sub 的 exec（如 upgrade-stage / upgrade-finalize）。
func (f *fakeRemote) called(sub string) []fakeToolCall {
	var out []fakeToolCall
	for _, c := range f.snapshot() {
		if c.Name == "exec" && len(c.Argv) > 1 && c.Argv[1] == sub {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeRemote) uploaded() bool {
	for _, c := range f.snapshot() {
		if c.Name == "upload_file" {
			return true
		}
	}
	return false
}

func execResult(stdout string) string {
	b, _ := json.Marshal(map[string]any{"exit_code": 0, "stdout": stdout})
	return string(b)
}

const (
	testLinuxHost   = "u@pc kylin amd64"
	testWindowsHost = "Administrator@srv Windows 10 amd64"
	testWindowsExe  = `C:\tools\remote.exe`
)

var testLinuxProbeStdout = "4242\n/home/u/remote\n/home/u\nLinux\nx86_64\n/tmp\n" +
	base64.StdEncoding.EncodeToString([]byte("/home/u/remote\x00share\x00")) + "\n"

func testWindowsProbeStdout() string {
	b, _ := json.Marshal(map[string]any{
		"pid": 4242, "exe": testWindowsExe, "cwd": `C:\tools`, "os": "windows", "arch": "amd64",
		"argv": []string{testWindowsExe, "share"}, "temp_dir": `C:\Temp\`,
	})
	return string(b)
}

// upgradeRemote 模拟一台 old share：两种探测各按对端系统应答，upload_file 成功，候选包
// --version 报 candidateVersion；argv[1] 命中 overrides 的 exec 用覆盖的应答。
func upgradeRemote(goos, candidateVersion string, overrides map[string][2]string) func(string, []string) (string, string) {
	return func(name string, argv []string) (string, string) {
		switch name {
		case "upload_file":
			return `{"bytes":1}`, ""
		case "exec":
		default:
			return "", "unexpected tool " + name
		}
		if len(argv) == 0 {
			return "", "empty argv"
		}
		if len(argv) > 1 {
			if r, ok := overrides[argv[1]]; ok {
				return r[0], r[1]
			}
			if argv[1] == "--version" {
				return execResult(candidateVersion + "\n"), ""
			}
		}
		switch argv[0] {
		case "sh":
			if goos != "linux" {
				return "", "exec: \"sh\": executable file not found"
			}
			return execResult(testLinuxProbeStdout), ""
		case "powershell.exe":
			if strings.Contains(argv[len(argv)-1], "Win32_Process") {
				if goos != "windows" {
					return "", "exec: \"powershell.exe\": executable file not found"
				}
				return execResult(testWindowsProbeStdout()), ""
			}
		}
		return execResult(""), ""
	}
}

// postUpgrade 以已连接状态把 binary 提交给 /api/upgrade。
func postUpgrade(t *testing.T, s *Server, h http.Handler, client *MCPClient, peerVersion, peerHost, binary string) *httptest.ResponseRecorder {
	t.Helper()
	s.mu.Lock()
	s.client, s.connected = client, true
	s.lastCode, s.lastServer = "AAAA-BBBBBB", "relay.example:8443"
	s.peerVersion, s.peerHost, s.effectiveSrv = peerVersion, peerHost, "relay.example:8443"
	s.mu.Unlock()
	content, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("binary", "remote-assist-cli")
	fw.Write(content)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/upgrade", &body)
	req.Host = "127.0.0.1:8731"
	req.Header.Set("X-Auth-Token", s.Token())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func runUpgrade(t *testing.T, goos, peerVersion, peerHost, binary string, handle func(string, []string) (string, string)) (*Server, *fakeRemote, *httptest.ResponseRecorder) {
	t.Helper()
	s, h := testServer(t)
	client, srv := newFakeClient()
	t.Cleanup(func() { client.Close() })
	remote := &fakeRemote{}
	remote.serve(srv, handle)
	return s, remote, postUpgrade(t, s, h, client, peerVersion, peerHost, binary)
}

// TestUpgradeRejectsStaleCandidateBeforeUpload 误选不比远端新的包时，必须在上传之前就拒绝。
// 过去要先传完整个包、在远端跑 --version 才发现，弱网下白等十分钟。
func TestUpgradeRejectsStaleCandidateBeforeUpload(t *testing.T) {
	for _, version := range []string{"v1.1.0-dirty", "v1.2.0"} {
		candidate := buildCandidate(t, "linux", version)
		_, remote, w := runUpgrade(t, "linux", "v1.2.0", testLinuxHost, candidate, upgradeRemote("linux", version, nil))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "无需升级") {
			t.Fatalf("候选包 %s 应返回 409 无需升级，实际 %d: %s", version, w.Code, w.Body.String())
		}
		if remote.uploaded() {
			t.Fatalf("候选包 %s 不应上传，调用序列: %+v", version, remote.snapshot())
		}
	}
}

// TestUpgradeWithoutLocalVersionStillUploads 读不到 ldflags 版本时不能拦，照旧上传后交给
// 远端 --version 判断。
func TestUpgradeWithoutLocalVersionStillUploads(t *testing.T) {
	candidate := buildCandidate(t, "linux", "")
	_, remote, w := runUpgrade(t, "linux", "v1.2.0", testLinuxHost, candidate, upgradeRemote("linux", "v1.1.0", nil))
	if !remote.uploaded() {
		t.Fatalf("未注入版本的候选包应照常上传，实际 %d: %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusConflict {
		t.Fatalf("上传后远端 --version 更旧应返回 409，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestUpgradeWindowsStageTransportLossReportsRecovery Windows 的 upgrade-stage 回执丢失时，
// 远端很可能已把原 exe 改名为备份（Defender 结束 old share 就是这种情形）。报错必须说出
// 备份路径与新协助码位置，不能再说"old 通道仍保留"，并把连接标记为丢失；对端自报
// Windows 时首个探测应是 PowerShell。
func TestUpgradeWindowsStageTransportLossReportsRecovery(t *testing.T) {
	candidate := buildCandidate(t, "windows", "v1.2.1")
	s, remote, w := runUpgrade(t, "windows", "v1.2.0", testWindowsHost, candidate, upgradeRemote("windows", "v1.2.1", map[string][2]string{
		"upgrade-stage": {"", "tunnel_lost: 隧道已断开，请重新 connect"},
	}))
	body := w.Body.String()
	if w.Code != http.StatusBadGateway {
		t.Fatalf("stage 回执丢失应返回 502，实际 %d: %s", w.Code, body)
	}
	if strings.Contains(body, "old 通道仍保留") {
		t.Fatalf("通道已断时不能声称 old 通道仍保留: %s", body)
	}
	for _, want := range []string{`C:\\tools\\remote.exe.old-`, `code.json`, "Defender"} {
		if !strings.Contains(body, want) {
			t.Fatalf("报错应包含 %s: %s", want, body)
		}
	}
	s.mu.Lock()
	connected := s.connected
	s.mu.Unlock()
	if connected {
		t.Fatal("通道已失联，应标记为未连接")
	}
	calls := remote.snapshot()
	if len(calls) == 0 || len(calls[0].Argv) == 0 || calls[0].Argv[0] != "powershell.exe" {
		t.Fatalf("Windows 对端首个探测应是 PowerShell，调用序列: %+v", calls)
	}
}

// TestUpgradeWindowsStageTimeoutRollsBack stage 超时被杀时来不及跑自己的 defer 还原，原 exe
// 可能停在备份名上。通道还在，就必须主动回滚；回滚成功时 old 确实完好，可以照旧这么说。
func TestUpgradeWindowsStageTimeoutRollsBack(t *testing.T) {
	candidate := buildCandidate(t, "windows", "v1.2.1")
	_, remote, w := runUpgrade(t, "windows", "v1.2.0", testWindowsHost, candidate, upgradeRemote("windows", "v1.2.1", map[string][2]string{
		"upgrade-stage": {"", "deadline_exceeded: exec timed out"},
	}))
	body := w.Body.String()
	if w.Code != http.StatusBadGateway || !strings.Contains(body, "old 通道仍保留") {
		t.Fatalf("回滚成功后应如实报 old 保留，实际 %d: %s", w.Code, body)
	}
	stage := remote.called("upgrade-stage")
	if len(stage) != 1 || stage[0].TimeoutMS < 120000 {
		t.Fatalf("应以不少于 120 秒的超时调用一次 upgrade-stage: %+v", stage)
	}
	rollback := remote.called("upgrade-finalize")
	if len(rollback) != 1 || !strings.Contains(strings.Join(rollback[0].Argv, " "), "--action rollback") {
		t.Fatalf("stage 超时后应回滚一次，调用序列: %+v", remote.snapshot())
	}
	if strings.Contains(body, ".old-") {
		t.Fatalf("回滚成功时不应再给手动恢复提示: %s", body)
	}
	if !strings.Contains(body, "残留") {
		t.Fatalf("stage 超时被杀后应提示可能残留候选进程: %s", body)
	}
}

// TestUpgradeWindowsStageRollbackFailureReportsRecovery 回滚本身失败时 old 文件状态不明，
// 必须给出备份路径。
func TestUpgradeWindowsStageRollbackFailureReportsRecovery(t *testing.T) {
	candidate := buildCandidate(t, "windows", "v1.2.1")
	_, _, w := runUpgrade(t, "windows", "v1.2.0", testWindowsHost, candidate, upgradeRemote("windows", "v1.2.1", map[string][2]string{
		"upgrade-stage":    {"", "deadline_exceeded: exec timed out"},
		"upgrade-finalize": {"", "restore old executable: access is denied"},
	}))
	body := w.Body.String()
	if w.Code != http.StatusBadGateway || !strings.Contains(body, `C:\\tools\\remote.exe.old-`) || !strings.Contains(body, "恢复旧文件失败") {
		t.Fatalf("回滚失败应给出备份路径，实际 %d: %s", w.Code, body)
	}
}

// TestUpgradeLinuxStageTransportLossHasNoWindowsHint Linux 的 stage 不改名原文件，失联时
// 不该出现 Windows 备份提示，但同样要承认远端状态未知。
func TestUpgradeLinuxStageTransportLossHasNoWindowsHint(t *testing.T) {
	candidate := buildCandidate(t, "linux", "v1.2.1")
	_, _, w := runUpgrade(t, "linux", "v1.2.0", testLinuxHost, candidate, upgradeRemote("linux", "v1.2.1", map[string][2]string{
		"upgrade-stage": {"", "peer_gone: 被协助端已断开连接，请重新 connect"},
	}))
	body := w.Body.String()
	if w.Code != http.StatusBadGateway || !strings.Contains(body, "远端状态未知") {
		t.Fatalf("Linux stage 失联应报远端状态未知，实际 %d: %s", w.Code, body)
	}
	if strings.Contains(body, ".old-") || strings.Contains(body, "Defender") {
		t.Fatalf("Linux 不应出现 Windows 备份提示: %s", body)
	}
}
