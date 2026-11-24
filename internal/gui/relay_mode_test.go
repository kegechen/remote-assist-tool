package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
