package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// fakeMCPArgv 读假子进程（见 TestMain）记下的命令行参数，返回 help --mcp-stdio 之后的部分。
func fakeMCPArgv(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath + ".argv")
	if err != nil {
		t.Fatalf("假子进程没记下命令行: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatal(err)
	}
	if len(argv) < 2 || argv[0] != "help" || argv[1] != "--mcp-stdio" {
		t.Fatalf("子进程命令行 = %q，应以 help --mcp-stdio 开头", argv)
	}
	return argv[2:]
}

// fakeConnectArgs 读假子进程收到的 connect 参数。
func fakeConnectArgs(t *testing.T, logPath string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("假子进程没收到 connect: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

// fakeMCPServer 返回用假子进程的 Server；logPath 是假子进程记录 connect 参数的文件。
func fakeMCPServer(t *testing.T, defaultServer string) (*Server, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "connect-args.json")
	t.Setenv("RAT_GUI_FAKE_MCP_LOG", logPath)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(exe, defaultServer)
	// 像用户点「断开」一样收尾：守护线程看到的是主动断开，不会去自动重连（测试结束后环境变量已还原，再起子进程就不是假的了）。
	t.Cleanup(func() {
		s.handleDisconnect(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/disconnect", nil))
	})
	return s, logPath
}

// webui 的显式/自动判定（spec 6.1）：服务器栏填了地址（或协助码后面带 @地址）才是显式——子进程带 --server，connect 也带
// server。留空时都不带：子进程是自动模式，按协助码找节点。GUI 自己设了 REMOTE_RELAY_SERVER 时子进程带 --server（进程显式），
// connect 不带 server，连的就是它；只有空白不算设了（同 CLI）。
func TestConnectRelayModeFromWebUI(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		defaultServer string
		wantArgv      []string
		wantServer    any // connect 的 server 参数；nil 表示没带
	}{
		{"服务器栏留空", `{"code":"W234-5678-9ABC"}`, "", nil, nil},
		{"填了地址", `{"code":"W234-5678-9ABC","server":"relay.example.test:8443"}`, "",
			[]string{"--server", "relay.example.test:8443"}, "relay.example.test:8443"},
		{"协助码后面带 @地址", `{"code":"W234-5678-9ABC@relay.example.test:8443"}`, "",
			[]string{"--server", "relay.example.test:8443"}, "relay.example.test:8443"},
		{"GUI 设了 REMOTE_RELAY_SERVER", `{"code":"W234-5678-9ABC"}`, "relay-env.example.test:8443",
			[]string{"--server", "relay-env.example.test:8443"}, nil},
		{"填了地址也设了 REMOTE_RELAY_SERVER，以填的为准", `{"code":"W234-5678-9ABC","server":"relay.example.test:8443"}`, "relay-env.example.test:8443",
			[]string{"--server", "relay.example.test:8443"}, "relay.example.test:8443"},
		{"REMOTE_RELAY_SERVER 只有空白，不算设了", `{"code":"W234-5678-9ABC"}`, " \t", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, logPath := fakeMCPServer(t, tc.defaultServer)
			req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(tc.body))
			req.Header.Set("X-Auth-Token", s.Token())
			rec := httptest.NewRecorder()
			s.handleConnect(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("connect 失败: %d %s", rec.Code, rec.Body.String())
			}
			if got := fakeMCPArgv(t, logPath); !slices.Equal(got, tc.wantArgv) {
				t.Fatalf("子进程参数 = %q，想要 %q", got, tc.wantArgv)
			}
			if got := fakeConnectArgs(t, logPath)["server"]; got != tc.wantServer {
				t.Fatalf("connect 的 server = %v，想要 %v", got, tc.wantServer)
			}
		})
	}
}

// 自动重连沿用用户当初的意图：服务器栏留空连上的，重连时子进程和 connect 也都不带地址，仍按协助码找节点。
func TestTryReconnectKeepsAutoMode(t *testing.T) {
	for _, tc := range []struct {
		name, server, defaultServer string
		wantArgv                    []string
		wantServer                  any
	}{
		{"自动模式", "", "", nil, nil},
		{"填了地址", "relay.example.test:8443", "", []string{"--server", "relay.example.test:8443"}, "relay.example.test:8443"},
		{"GUI 设了 REMOTE_RELAY_SERVER", "", "relay-env.example.test:8443", []string{"--server", "relay-env.example.test:8443"}, nil},
		{"填了地址也设了 REMOTE_RELAY_SERVER", "relay.example.test:8443", "relay-env.example.test:8443",
			[]string{"--server", "relay.example.test:8443"}, "relay.example.test:8443"},
		{"REMOTE_RELAY_SERVER 只有空白", "", " \t", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, logPath := fakeMCPServer(t, tc.defaultServer)
			if got := s.tryReconnect(s.connGen, "W234-5678-9ABC", tc.server, false, ""); got != reconnectOK {
				t.Fatalf("tryReconnect = %v，想要 reconnectOK", got)
			}
			if got := fakeMCPArgv(t, logPath); !slices.Equal(got, tc.wantArgv) {
				t.Fatalf("子进程参数 = %q，想要 %q", got, tc.wantArgv)
			}
			if got := fakeConnectArgs(t, logPath)["server"]; got != tc.wantServer {
				t.Fatalf("connect 的 server = %v，想要 %v", got, tc.wantServer)
			}
		})
	}
}

// 通道内升级后连 new share：用户连 old 时是自动模式（服务器栏留空、GUI 也没设 REMOTE_RELAY_SERVER）就仍按新码找节点，
// 不用 new share 写下的地址——那可能是对方机器上的本机专用地址，这边连不上；节点换了证书时，这边按地址记的 TOFU
// 指纹也对不上。其余情况照旧连 new share 写下的地址。
func TestUpgradeReconnectServer(t *testing.T) {
	for _, tc := range []struct {
		name, old, def, written, want string
	}{
		{"自动模式", "", "", "192.0.2.7:8443", ""},
		{"服务器栏填了地址", "relay.example.test:8443", "", "192.0.2.7:8443", "192.0.2.7:8443"},
		{"GUI 设了 REMOTE_RELAY_SERVER", "", "relay-env.example.test:8443", "192.0.2.7:8443", "192.0.2.7:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := upgradeReconnectServer(tc.old, tc.def, tc.written); got != tc.want {
				t.Fatalf("upgradeReconnectServer(%q, %q, %q) = %q，想要 %q", tc.old, tc.def, tc.written, got, tc.want)
			}
		})
	}
}

// 升级全流程走到切换通道：连 new share 用的 server 按 upgradeReconnectServer 取，升级完成后记下的也是它（之后断线
// 自动重连沿用）。自动模式下 connect 不带 server、记下空串，显式时连、记的都是 new share 写下的地址。
func TestUpgradeSwitchesToNewShareByRelayMode(t *testing.T) {
	candidate := buildCandidate(t, "linux", "v1.2.1")
	const written = "192.0.2.7:8443"
	for _, tc := range []struct {
		name, lastServer, defaultServer string
		wantServer                      any // 连 new share 时 connect 的 server 参数；nil 表示没带
		wantLast                        string
	}{
		{"自动模式", "", "", nil, ""},
		{"服务器栏填了地址", "relay.example.test:8443", "", written, written},
		{"GUI 设了 REMOTE_RELAY_SERVER", "", "relay-env.example.test:8443", written, written},
		{"REMOTE_RELAY_SERVER 只有空白，仍是自动模式", "", " \t", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("nonexistent-remote-bin", tc.defaultServer)
			client, srv := newFakeClient()
			t.Cleanup(func() { client.Close() })
			remote := &fakeRemote{}
			old := upgradeRemote("linux", "v1.2.1", nil)
			remote.serveCalls(srv, func(c fakeToolCall) (string, string) {
				switch c.Name {
				case "read_file":
					path, _ := c.Args["path"].(string)
					switch {
					case strings.HasSuffix(path, "code.json"):
						return string(mustJSON(map[string]string{"text": `{"code":"CCCC-DDDDDD","server":"` + written + `"}`})), ""
					case strings.HasSuffix(path, "new.pid"):
						return string(mustJSON(map[string]string{"text": "5151\n"})), ""
					}
					return "", "unexpected read_file " + path
				case "connect":
					return string(mustJSON(connectMetadata{Connected: true, Server: "relay.example.test:8443", PeerVersion: "v1.2.1"})), ""
				}
				return old(c.Name, c.Argv)
			})
			w := postUpgradeFrom(t, s, s.Routes(), client, "v1.2.0", testLinuxHost, candidate, tc.lastServer, "relay.example.test:8443")
			if w.Code != http.StatusOK {
				t.Fatalf("升级应成功，实际 %d: %s", w.Code, w.Body.String())
			}
			var connects []fakeToolCall
			for _, c := range remote.snapshot() {
				if c.Name == "connect" {
					connects = append(connects, c)
				}
			}
			if len(connects) != 1 || connects[0].Args["code"] != "CCCC-DDDDDD" {
				t.Fatalf("应按新码连一次 new share，实际 connect 调用: %+v", connects)
			}
			if got := connects[0].Args["server"]; got != tc.wantServer {
				t.Fatalf("连 new share 的 server = %v，想要 %v", got, tc.wantServer)
			}
			s.mu.Lock()
			last := s.lastServer
			s.mu.Unlock()
			if last != tc.wantLast {
				t.Fatalf("升级后记下的地址 = %q，想要 %q", last, tc.wantLast)
			}
		})
	}
}

// 连 new share 写下的地址失败后要不要、按什么再试（见 TestUpgradeRetriesOldServerWhenWrittenUnreachable）。
// 只有连 relay 这一步网络不通（连不上、被拒、超时）才再试：加入、握手阶段超时说明已经连到了写下的地址，
// 被 relay 封禁、协助码不对时换地址也没用，多连一次还让 relay 多记一次失败。
func TestUpgradeRetryServer(t *testing.T) {
	refused := errors.New("relay connect failed: dial tcp 192.0.2.7:8443: connection refused")
	for _, tc := range []struct {
		name, old, def, tried string
		err                   error
		want, used            string // 再试用的 server 参数、实际连的地址
		ok                    bool
	}{
		{"服务器栏填了地址", "relay.example.test:8443", "", "192.0.2.7:8443", refused, "relay.example.test:8443", "relay.example.test:8443", true},
		{"GUI 设了 REMOTE_RELAY_SERVER", "", "relay-env.example.test:8443", "192.0.2.7:8443", refused, "", "relay-env.example.test:8443", true},
		{"同一个地址（大小写不同）", "Relay.Example.Test:8443", "", "relay.example.test:8443", refused, "", "", false},
		{"同一个地址（没写端口）", "relay.example.test", "", "relay.example.test:8443", refused, "", "", false},
		{"同一个地址（前后有空白）", " relay.example.test ", "", "relay.example.test:8443", refused, "", "", false},
		{"同 REMOTE_RELAY_SERVER", "", "relay-env.example.test:8443", "relay-env.example.test:8443", refused, "", "", false},
		{"同 REMOTE_RELAY_SERVER（没写端口）", "", "relay-env.example.test", "relay-env.example.test:8443", refused, "", "", false},
		{"自动模式", "", "", "", refused, "", "", false},
		{"new share 没写地址，连的就是 help 的默认地址", "relay.example.test:8443", "", "", refused, "", "", false},
		{"超时也再试", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: dial tcp 192.0.2.7:8443: i/o timeout"), "relay.example.test:8443", "relay.example.test:8443", true},
		{"找不到主机也再试", "relay.example.test:8443", "", "relay-w.example.test:8443", errors.New("relay connect failed: dial tcp: lookup relay-w.example.test: no such host"), "relay.example.test:8443", "relay.example.test:8443", true},
		{"被 relay 封禁", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: " + proto.BlockedMessage), "", "", false},
		{"协助码不对", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("join failed: session not found"), "", "", false},
		{"加入超时（已经连到了写下的地址）", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("join failed: read tcp 198.51.100.2:50000->192.0.2.7:8443: i/o timeout"), "", "", false},
		{"握手超时", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("handshake failed: i/o timeout"), "", "", false},
		{"等结果超时（不知道卡在哪一步）", "relay.example.test:8443", "", "192.0.2.7:8443", context.DeadlineExceeded, "", "", false},
		// 证书、TLS 的错误也带 relay connect failed 前缀，但说明已经连到了写下的地址、对方的证书不对（可能是中间人）：
		// 换地址连上会把这个警告盖过去。
		{"证书指纹与首次连接时不一致", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: relay certificate fingerprint changed: 192.0.2.7:8443 的证书指纹与首次连接时不一致"), "", "", false},
		{"证书与节点列表不符", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: 192.0.2.7:8443 出示的证书（指纹 abababababababab…）与节点列表不符，可能被劫持"), "", "", false},
		{"证书不受信任", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: tls: failed to verify certificate: x509: certificate signed by unknown authority"), "", "", false},
		{"建不了 TLS 配置", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to create TLS config: failed to parse CA cert"), "", "", false},
		{"连上后被断开", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: EOF"), "", "", false},
		// 地址是用户写的，里面碰巧有网络错误的字样也不能让证书错误被当成网络不通。
		{"证书错误（地址里碰巧有 refused 字样）", "relay.example.test:8443", "", "refused.example.test:8443", errors.New("relay connect failed: failed to connect: relay certificate fingerprint changed: refused.example.test:8443 的证书指纹与首次连接时不一致"), "", "", false},
		{"被拒（Windows 的写法）", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: dial tcp 192.0.2.7:8443: connectex: No connection could be made because the target machine actively refused it."), "relay.example.test:8443", "relay.example.test:8443", true},
		{"网络不可达", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: dial tcp 192.0.2.7:8443: connect: network is unreachable"), "relay.example.test:8443", "relay.example.test:8443", true},
		{"QUIC 超时", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: timeout: no recent network activity"), "relay.example.test:8443", "relay.example.test:8443", true},
		{"连 relay 时到期", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: context deadline exceeded"), "relay.example.test:8443", "relay.example.test:8443", true},
		{"Linux 连接超时", "relay.example.test:8443", "", "192.0.2.7:8443", errors.New("relay connect failed: failed to connect: dial tcp 192.0.2.7:8443: connect: connection timed out"), "relay.example.test:8443", "relay.example.test:8443", true},
		{"已取消", "relay.example.test:8443", "", "192.0.2.7:8443", context.Canceled, "", "", false},
		{"连上了", "relay.example.test:8443", "", "192.0.2.7:8443", nil, "", "", false},
	} {
		got, used, ok := upgradeRetryServer(tc.old, tc.def, tc.tried, tc.err)
		if got != tc.want || used != tc.used || ok != tc.ok {
			t.Errorf("%s: upgradeRetryServer(%q, %q, %q, %v) = %q, %q, %v，想要 %q, %q, %v", tc.name, tc.old, tc.def, tc.tried, tc.err, got, used, ok, tc.want, tc.used, tc.ok)
		}
	}
}

// 显式模式下 new share 写的是它所在节点在列表里的公开地址（spec 6.7），这边不一定连得上（比如这边只能经内网地址连
// 那个节点）：连不上就按连 old 时用的地址再试一次——这个地址这边刚连通过，new share 多半还在同一台 relay 上。
// 同一个地址、换地址也没用的错误（如被 relay 封禁）不重试。两次都失败时照常回连 old、终止候选 share，两次的错误都要报出来。
func TestUpgradeRetriesOldServerWhenWrittenUnreachable(t *testing.T) {
	candidate := buildCandidate(t, "linux", "v1.2.1")
	const (
		written = "192.0.2.7:8443"
		refused = "relay connect failed: dial tcp " + written + ": connection refused"
		timeout = "relay connect failed: dial tcp relay.example.test:8443: i/o timeout"
	)
	for _, tc := range []struct {
		name, lastServer, defaultServer string
		firstErr, retryErr              string // 连 new share 写的地址、改用连 old 的地址再试时的错误；retryErr 为空表示再试连得上
		wantServers                     []any  // 依次连 new share 时 connect 的 server 参数；nil 表示没带
		wantOK                          bool
	}{
		{"服务器栏填了地址", "relay.example.test:8443", "", refused, "", []any{written, "relay.example.test:8443"}, true},
		{"GUI 设了 REMOTE_RELAY_SERVER", "", "relay-env.example.test:8443", refused, "", []any{written, nil}, true},
		{"连 old 用的就是这个地址，不重试", written, "", refused, "", []any{written}, false},
		{"连 old 用的地址只是没写端口，也不重试", "192.0.2.7", "", refused, "", []any{written}, false},
		{"被 relay 封禁，换地址也没用", "relay.example.test:8443", "", "relay connect failed: " + proto.BlockedMessage, "", []any{written}, false},
		{"两次都失败", "relay.example.test:8443", "", refused, timeout, []any{written, "relay.example.test:8443"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("nonexistent-remote-bin", tc.defaultServer)
			events := make(chan string, 256)
			s.sseMu.Lock()
			s.sseSubs[events] = struct{}{}
			s.sseMu.Unlock()
			client, srv := newFakeClient()
			t.Cleanup(func() { client.Close() })
			remote := &fakeRemote{}
			old := upgradeRemote("linux", "v1.2.1", nil)
			remote.serveCalls(srv, func(c fakeToolCall) (string, string) {
				switch c.Name {
				case "read_file":
					path, _ := c.Args["path"].(string)
					switch {
					case strings.HasSuffix(path, "code.json"):
						return string(mustJSON(map[string]string{"text": `{"code":"CCCC-DDDDDD","server":"` + written + `"}`})), ""
					case strings.HasSuffix(path, "new.pid"):
						return string(mustJSON(map[string]string{"text": "5151\n"})), ""
					}
					return "", "unexpected read_file " + path
				case "connect":
					if c.Args["code"] == "CCCC-DDDDDD" && c.Args["server"] == written {
						return "", tc.firstErr
					}
					if c.Args["code"] == "CCCC-DDDDDD" && tc.retryErr != "" {
						return "", tc.retryErr
					}
					return string(mustJSON(connectMetadata{Connected: true, Server: "relay.example.test:8443", PeerVersion: "v1.2.1"})), ""
				}
				return old(c.Name, c.Argv)
			})
			w := postUpgradeFrom(t, s, s.Routes(), client, "v1.2.0", testLinuxHost, candidate, tc.lastServer, "relay.example.test:8443")
			if ok := w.Code == http.StatusOK; ok != tc.wantOK {
				t.Fatalf("升级结果 %d，想要成功 = %v: %s", w.Code, tc.wantOK, w.Body.String())
			}
			var servers []any
			for _, c := range remote.snapshot() {
				if c.Name == "connect" && c.Args["code"] == "CCCC-DDDDDD" {
					servers = append(servers, c.Args["server"])
				}
			}
			if !slices.Equal(servers, tc.wantServers) {
				t.Fatalf("连 new share 的 server 依次为 %v，想要 %v", servers, tc.wantServers)
			}
			// 再试之前的进度提示要说清第一次为什么失败、改连哪个地址：再试成功时第一次的错误就只出现在这里。
			var progress []string
			for drained := false; !drained; {
				select {
				case ev := <-events:
					if strings.HasPrefix(ev, "event: upgrade-progress\n") {
						progress = append(progress, ev)
					}
				default:
					drained = true
				}
			}
			used := tc.lastServer
			if used == "" {
				used = tc.defaultServer
			}
			note := ""
			for _, ev := range progress {
				if strings.Contains(ev, "再试") {
					note = ev
				}
			}
			if retried := len(tc.wantServers) > 1; retried != (note != "") {
				t.Fatalf("再试 = %v，进度提示里的再试说明 = %q", retried, note)
			}
			if note != "" && (!strings.Contains(note, tc.firstErr) || !strings.Contains(note, used)) {
				t.Fatalf("再试的进度提示 %q 应带上第一次的错误 %q 和改连的地址 %q", note, tc.firstErr, used)
			}
			if tc.wantOK {
				s.mu.Lock()
				last := s.lastServer
				s.mu.Unlock()
				if last != tc.lastServer {
					t.Fatalf("升级后记下的地址 = %q，应为实际连上 new share 用的 %q", last, tc.lastServer)
				}
				return
			}
			var resp struct {
				Error string `json:"error"`
			}
			if w.Code != http.StatusBadGateway || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
				t.Fatalf("连不上 new share 应回 502: %d %s", w.Code, w.Body.String())
			}
			for _, want := range []string{tc.firstErr, tc.retryErr} {
				if !strings.Contains(resp.Error, want) {
					t.Fatalf("失败消息 %q 应含 %q", resp.Error, want)
				}
			}
			var olds []any
			for _, c := range remote.snapshot() {
				if c.Name == "connect" && c.Args["code"] == "AAAA-BBBBBB" {
					olds = append(olds, c.Args["server"])
				}
			}
			if want := []any{tc.lastServer}; !slices.Equal(olds, want) {
				t.Fatalf("回连 old 的 server 依次为 %v，想要 %v", olds, want)
			}
			if kills := remote.called("-TERM"); len(kills) != 1 || kills[0].Argv[2] != "5151" {
				t.Fatalf("应终止候选 share（pid 5151），实际 %v", kills)
			}
		})
	}
}
