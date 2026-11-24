package main

import (
	"flag"
	"os"
	"reflect"
	"runtime"
	"testing"
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
