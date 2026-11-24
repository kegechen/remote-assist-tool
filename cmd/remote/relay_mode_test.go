package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/client"
)

// share 的显式/自动判定（spec 6.1）：写了 --server、设了 REMOTE_RELAY_SERVER、用了 --standalone 是显式，
// 其余都是自动。--plain 只用于开发，节点都要 TLS，也按显式处理。
func TestShareUsesNodeList(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"什么都没写", nil, "", true},
		{"写了 --server", []string{"--server", "relay.example.test"}, "", false},
		{"--server= 写法", []string{"--server=relay.example.test"}, "", false},
		{"写的正是内置地址也算显式", []string{"--server", defaultRelayServerAddr}, "", false},
		{"设了 REMOTE_RELAY_SERVER", nil, "relay.example.test:8443", false},
		{"REMOTE_RELAY_SERVER 只有空白", nil, "  ", true},
		{"--standalone", []string{"--standalone"}, "", false},
		{"--plain", []string{"--plain"}, "", false},
		{"--relay-hint 不改变自动模式", []string{"--relay-hint", "relay-w.example.test:8443"}, "", true},
		{"其他参数", []string{"--root", "/srv/app", "--transport", "quic"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REMOTE_RELAY_SERVER", tc.env)
			fs := flag.NewFlagSet("share", flag.ContinueOnError)
			fs.String("server", defaultRelayServer(), "")
			standalone := fs.Bool("standalone", false, "")
			plain := fs.Bool("plain", false, "")
			fs.String("relay-hint", "", "")
			fs.String("root", "", "")
			fs.String("transport", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := shareUsesNodeList(fs, *standalone, *plain); got != tc.want {
				t.Fatalf("shareUsesNodeList = %v，想要 %v", got, tc.want)
			}
		})
	}
}

// 升级继任者在隔离期内 HOME/USERPROFILE 指向暂存目录，节点列表缓存要用隔离前的家目录（os.UserHomeDir 看的那个变量）。
func TestUpgradeRealHome(t *testing.T) {
	key, real := "HOME", "/home/real"
	if runtime.GOOS == "windows" {
		key, real = "USERPROFILE", `C:\Users\real`
	}
	staged := stashOrigHomeEnv([]string{"HOME=/home/real", `USERPROFILE=C:\Users\real`}, "HOME", "USERPROFILE")
	staged = replaceEnvForTest(staged, key, "/tmp/upgrade/home")
	applyEnvForTest(t, staged)
	if got := upgradeRealHome(); got != real {
		t.Fatalf("upgradeRealHome = %q，想要 %q", got, real)
	}

	stash := upgradeOrigHomeEnv
	if runtime.GOOS == "windows" {
		stash = upgradeOrigUserProfileEnv
	}
	for _, v := range []string{"0", "x/bad", ""} {
		t.Setenv(stash, v)
		if got := upgradeRealHome(); got != "" {
			t.Errorf("%s=%q 时 upgradeRealHome = %q，想要空串（照常用 os.UserHomeDir）", stash, v, got)
		}
	}
	os.Unsetenv(stash)
	if got := upgradeRealHome(); got != "" {
		t.Errorf("不是升级继任者时 upgradeRealHome = %q，想要空串", got)
	}
}

// share 启动时的 relay 设定：自动模式要带上 --relay-hint（补默认端口）和升级隔离前的家目录。
func TestNewShareRelay(t *testing.T) {
	key, real := "HOME", "/home/real"
	stash := upgradeOrigHomeEnv
	if runtime.GOOS == "windows" {
		key, real = "USERPROFILE", `C:\Users\real`
		stash = upgradeOrigUserProfileEnv
	}
	parse := func(t *testing.T, args ...string) (*flag.FlagSet, bool, bool, string) {
		t.Helper()
		fs := flag.NewFlagSet("share", flag.ContinueOnError)
		fs.String("server", defaultRelayServer(), "")
		standalone := fs.Bool("standalone", false, "")
		plain := fs.Bool("plain", false, "")
		hint := fs.String("relay-hint", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs, *standalone, *plain, *hint
	}
	t.Setenv("REMOTE_RELAY_SERVER", "")
	t.Setenv(key, "/tmp/upgrade/home")
	t.Setenv(stash, string(upgradeEnvPresent)+real)

	for _, tc := range []struct {
		name      string
		args      []string
		successor bool
		want      shareRelay
	}{
		{"自动模式", nil, false, shareRelay{auto: true}},
		{"--relay-hint 补默认端口", []string{"--relay-hint", "relay-w.example.test"}, false, shareRelay{auto: true, hint: "relay-w.example.test:8443"}},
		{"--relay-hint 去掉首尾空白", []string{"--relay-hint", " relay-w.example.test:9443 "}, false, shareRelay{auto: true, hint: "relay-w.example.test:9443"}},
		{"升级继任者用隔离前的家目录", []string{"--relay-hint", "relay-w.example.test:8443"}, true, shareRelay{auto: true, hint: "relay-w.example.test:8443", listHome: real}},
		{"写了 --server 是显式模式", []string{"--server", "relay.example.test"}, true, shareRelay{}},
		{"--standalone", []string{"--standalone"}, false, shareRelay{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, standalone, plain, hint := parse(t, tc.args...)
			if got := newShareRelay(fs, standalone, plain, tc.successor, hint); got != tc.want {
				t.Fatalf("newShareRelay = %+v，想要 %+v", got, tc.want)
			}
		})
	}
}

// STUN 也是 UDP 中继的地址：自动模式下不能按内置默认节点推导，要留给 share 跟着实际连上的节点走。
func TestShareRelaySTUN(t *testing.T) {
	auto, explicit := shareRelay{auto: true}, shareRelay{}
	for _, tc := range []struct {
		name         string
		r            shareRelay
		stun, server string
		want         string
	}{
		{"自动模式不推导", auto, "", "relay.example.test:8443", ""},
		{"自动模式给了 --stun 就用它", auto, "stun.example.test:3478", "relay.example.test:8443", "stun.example.test:3478"},
		{"显式模式按 --server 推导", explicit, "", "relay.example.test:8443", "relay.example.test:3478"},
		{"显式模式给了 --stun 就用它", explicit, "stun.example.test:3478", "relay.example.test:8443", "stun.example.test:3478"},
		{"没有 relay 地址", explicit, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.stun(tc.stun, tc.server); got != tc.want {
				t.Fatalf("stun(%q, %q) = %q，想要 %q", tc.stun, tc.server, got, tc.want)
			}
		})
	}
}

type fakeNodeListUser struct{ calls [][2]string }

func (f *fakeNodeListUser) UseNodeList(hint, home string) {
	f.calls = append(f.calls, [2]string{hint, home})
}

// 自动模式才接上节点列表，显式模式下 share 只连 --server。
func TestShareRelayApply(t *testing.T) {
	var auto fakeNodeListUser
	shareRelay{auto: true, hint: "relay-w.example.test:8443", listHome: "/home/real"}.apply(&auto)
	if want := [][2]string{{"relay-w.example.test:8443", "/home/real"}}; !reflect.DeepEqual(auto.calls, want) {
		t.Fatalf("自动模式 UseNodeList 调用 = %v，想要 %v", auto.calls, want)
	}
	var explicit fakeNodeListUser
	shareRelay{}.apply(&explicit)
	if len(explicit.calls) != 0 {
		t.Fatalf("显式模式不该调 UseNodeList，调了 %v", explicit.calls)
	}
}

// help 的显式/自动判定（spec 6.1）：同 share，只是 help 没有 --standalone。--plain 只用于开发，节点都要 TLS，也按显式处理。
func TestNewHelpRelay(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"什么都没写", nil, "", true},
		{"只给协助码", []string{"--code", "W234-5678-9ABC"}, "", true},
		{"写了 --server", []string{"--server", "relay.example.test"}, "", false},
		{"--server= 写法", []string{"--server=relay.example.test"}, "", false},
		{"写的正是内置地址也算显式", []string{"--server", defaultRelayServerAddr}, "", false},
		{"设了 REMOTE_RELAY_SERVER", nil, "relay.example.test:8443", false},
		{"REMOTE_RELAY_SERVER 只有空白", nil, "  ", true},
		{"--plain", []string{"--plain"}, "", false},
		{"MCP 与其他参数", []string{"--mcp-stdio", "--no-auth", "--transport", "quic"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REMOTE_RELAY_SERVER", tc.env)
			fs := flag.NewFlagSet("help", flag.ContinueOnError)
			fs.String("server", defaultRelayServer(), "")
			fs.String("code", "", "")
			plain := fs.Bool("plain", false, "")
			fs.Bool("mcp-stdio", false, "")
			fs.Bool("no-auth", false, "")
			fs.String("transport", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got, want := newHelpRelay(fs, *plain), (helpRelay{auto: tc.want}); got != want {
				t.Fatalf("newHelpRelay = %+v，想要 %+v", got, want)
			}
		})
	}
}

// help 的 STUN 同 share：自动模式下不按内置默认节点推导，留给 help 跟着实际连上的节点走（client.HelpMode.p2pSTUN）。
func TestHelpRelaySTUN(t *testing.T) {
	auto, explicit := helpRelay{auto: true}, helpRelay{}
	for _, tc := range []struct {
		name         string
		r            helpRelay
		stun, server string
		want         string
	}{
		{"自动模式不推导", auto, "", "relay.example.test:8443", ""},
		{"自动模式给了 --stun 就用它", auto, "stun.example.test:3478", "relay.example.test:8443", "stun.example.test:3478"},
		{"显式模式按 --server 推导", explicit, "", "relay.example.test:8443", "relay.example.test:3478"},
		{"显式模式给了 --stun 就用它", explicit, "stun.example.test:3478", "relay.example.test:8443", "stun.example.test:3478"},
		{"没有 relay 地址", explicit, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.stun(tc.stun, tc.server); got != tc.want {
				t.Fatalf("stun(%q, %q) = %q，想要 %q", tc.stun, tc.server, got, tc.want)
			}
		})
	}
}

// relay 在私网/本机时 auto 跳过 P2P。显式模式按 --server 在连接前判断，与原来一样；自动模式这时还不知道会连哪台，
// 留给连上之后按实际地址判断（client.HelpMode.Run、MCP connect）。
func TestHelpRelaySkipLANP2P(t *testing.T) {
	auto, explicit := helpRelay{auto: true}, helpRelay{}
	for _, tc := range []struct {
		name         string
		r            helpRelay
		mode, server string
		want         bool
	}{
		{"显式、本机、auto", explicit, "auto", "127.0.0.1:8443", true},
		{"显式、localhost、auto", explicit, "auto", "localhost:8443", true},
		{"显式、公网域名", explicit, "auto", "relay.example.test:8443", false},
		{"显式、公网 IP", explicit, "auto", "192.0.2.1:8443", false},
		{"显式、本机、required", explicit, "required", "127.0.0.1:8443", false},
		{"显式、本机、disabled", explicit, "disabled", "127.0.0.1:8443", false},
		{"自动模式不在连接前判断", auto, "auto", "127.0.0.1:8443", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.skipLANP2P(tc.mode, tc.server); got != tc.want {
				t.Fatalf("skipLANP2P(%q, %q) = %v，想要 %v", tc.mode, tc.server, got, tc.want)
			}
		})
	}
}

type fakeHelpNodeListUser struct{ homes []string }

func (f *fakeHelpNodeListUser) UseNodeList(home string) { f.homes = append(f.homes, home) }

// 自动模式才接上节点列表；help 不会是升级继任者（通道内升级只起 share），列表缓存用当前用户的家目录。显式模式下只连 --server。
func TestHelpRelayApply(t *testing.T) {
	var auto fakeHelpNodeListUser
	helpRelay{auto: true}.apply(&auto)
	if want := []string{""}; !reflect.DeepEqual(auto.homes, want) {
		t.Fatalf("自动模式 UseNodeList 调用 = %q，想要 %q", auto.homes, want)
	}
	var explicit fakeHelpNodeListUser
	helpRelay{}.apply(&explicit)
	if len(explicit.homes) != 0 {
		t.Fatalf("显式模式不该调 UseNodeList，调了 %q", explicit.homes)
	}
}

// 三种 help（MCP 等 connect、MCP 带协助码、命令行）都要接上自动模式。接上时会读 REMOTE_RELAY_DIRECTORY，设成不合法的
// 地址会提示一次，借此看出接没接上；只建不跑，不会去连 relay。
func TestNewHelpRunnerAppliesRelayMode(t *testing.T) {
	t.Setenv("REMOTE_RELAY_DIRECTORY", "http://directory.example.test/v1/directory")
	for _, tc := range []struct {
		name, code string
		mcp        bool
	}{
		{"MCP 等 connect", "", true},
		{"MCP 带协助码", "W234-5678-9ABC", true},
		{"命令行", "W234-5678-9ABC", false},
	} {
		for _, auto := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/auto=%v", tc.name, auto), func(t *testing.T) {
				// newHelpRunner 会改 cfg（configure），每例各用一份。
				cfg := &client.Config{ServerAddr: "relay.example.test:8443", UseTLS: true}
				stderr := captureStderr(t, func() {
					newHelpRunner(cfg, tc.code, "127.0.0.1:0", tc.mcp, helpRelay{auto: auto})
				})
				if got := strings.Contains(stderr, "REMOTE_RELAY_DIRECTORY"); got != auto {
					t.Fatalf("接上节点列表 = %v，想要 %v；stderr:\n%s", got, auto, stderr)
				}
			})
		}
	}
}

// newHelpRunner 按 relay 方式补全 P2P 配置：自动模式下不按默认地址推导 STUN，也不在连接前做私网判断（连上之后按实际地址
// 判断）；显式模式与原来一样。
func TestNewHelpRunnerConfiguresP2P(t *testing.T) {
	for _, tc := range []struct {
		name             string
		auto             bool
		server, p2p      string
		stun             string
		wantSTUN, wantPM string
	}{
		{"自动模式不推导 STUN", true, "relay.example.test:8443", "auto", "", "", "auto"},
		{"自动模式不在连接前跳过 P2P", true, "127.0.0.1:8443", "auto", "", "", "auto"},
		{"显式模式按 --server 推导 STUN", false, "relay.example.test:8443", "auto", "", "relay.example.test:3478", "auto"},
		{"显式模式给了 --stun 就用它", false, "relay.example.test:8443", "auto", "stun.example.test:3478", "stun.example.test:3478", "auto"},
		{"显式模式 relay 在本机时跳过 P2P", false, "127.0.0.1:8443", "auto", "", "127.0.0.1:3478", "disabled"},
		{"显式模式 required 照做", false, "127.0.0.1:8443", "required", "", "127.0.0.1:3478", "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &client.Config{ServerAddr: tc.server, UseTLS: true, P2PMode: tc.p2p, STUNServer: tc.stun}
			captureStderr(t, func() { newHelpRunner(cfg, "W234-5678-9ABC", "127.0.0.1:0", true, helpRelay{auto: tc.auto}) })
			if cfg.STUNServer != tc.wantSTUN || cfg.P2PMode != tc.wantPM {
				t.Fatalf("STUN = %q、P2P = %q，想要 %q、%q", cfg.STUNServer, cfg.P2PMode, tc.wantSTUN, tc.wantPM)
			}
		})
	}
}

// captureStderr 跑 fn，返回它写到 os.Stderr 的内容。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = original }()
	out := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		r.Close()
		out <- data
	}()
	fn()
	os.Stderr = original
	w.Close()
	return string(<-out)
}
