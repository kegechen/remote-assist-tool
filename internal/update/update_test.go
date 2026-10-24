package update

import (
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.1.0", "0.0.7", true},
		{"1.1.0", "v1.0.1", true},
		{"v1.1.0", "1.1.0", false},
		{"v1.1.0", "1.1.0-3-gabcdef", false},       // describe 后缀比 tag 更新，不是预发布
		{"v1.1.0", "1.1.0-3-gabcdef-dirty", false}, // 同上
		{"v1.3.0", "v1.3.0-rc1", true},             // 预发布低于同号正式版
		{"v1.3.0", "v1.3.1-rc1", false},
		{"v1.0.1", "1.0.0", true},
		{"1.0.0", "v1.1.0", false}, // 客户端比 relay 新：不提示
		{"v1.1.0", "dev", false},
		{"v1.1.0", "dev (commit: abc1234)", false},
		{"", "0.0.7", false},
		{"garbage", "0.0.7", false},
		{"0.0.10", "0.0.9", true}, // 数字比较，不是字典序
		// latest 必须是正式版本：不完整、带后缀、纯数字 commit 哈希都不算
		{"v1.1", "1.0.9", false},
		{"v1.3.0-rc1", "1.2.0", false},
		{"v1.1.0-3-gabc", "1.0.0", false},
		{"1234567", "1.0.0", false},
		{"9.9.9-\x1b]0;x\x07", "1.0.0", false}, // 控制字符走不进来
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestIsRelease(t *testing.T) {
	for s, want := range map[string]bool{
		"v1.1.0": true, "1.0.0": true, "0.0.9": true,
		"dev": false, "1234567": false, "v1.1": false, "v1.1.0-rc1": false,
		"v1.1.0-2-gabc123": false, "v01.1.0": false, "": false,
	} {
		if got := IsRelease(s); got != want {
			t.Errorf("IsRelease(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestNormalizeRelease(t *testing.T) {
	for input, want := range map[string]string{"1.1.0": "v1.1.0", "v1.1.0": "v1.1.0", " 1.2.3 ": "v1.2.3"} {
		got, ok := NormalizeRelease(input)
		if !ok || got != want {
			t.Errorf("NormalizeRelease(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	for _, input := range []string{"dev", "v1.1.0-rc1", "v1.1"} {
		if got, ok := NormalizeRelease(input); ok {
			t.Errorf("NormalizeRelease(%q) = %q, true; want invalid", input, got)
		}
	}
}

func TestExpandURL(t *testing.T) {
	const base = "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-"
	if got := ExpandURL(DefaultDownloadURL, "v1.1.0", "windows", "amd64"); got != base+"windows-amd64.exe" {
		t.Errorf("windows: %s", got)
	}
	if got := ExpandURL(DefaultDownloadURL, "v1.1.0", "linux", "arm64"); got != base+"linux-arm64" {
		t.Errorf("linux: %s", got)
	}
	for _, p := range [][2]string{{"linux", "x86_64"}, {"linux", "aarch64"}, {"windows", "X86_64"}} {
		canonical := map[string]string{"x86_64": "amd64", "X86_64": "amd64", "aarch64": "arm64"}[p[1]]
		want := base + p[0] + "-" + canonical
		if p[0] == "windows" {
			want += ".exe"
		}
		if got := ExpandURL(DefaultDownloadURL, "v1.1.0", p[0], p[1]); got != want {
			t.Errorf("%s/%s = %s, want %s", p[0], p[1], got, want)
		}
	}
	// 未发布产物的平台不拼 404 链接
	for _, p := range [][2]string{{"", "amd64"}, {"linux", "loong64"}, {"linux", "386"}, {"freebsd", "amd64"}} {
		if got := ExpandURL(DefaultDownloadURL, "v1.1.0", p[0], p[1]); got != "" {
			t.Errorf("%s/%s 应返回空，得到 %s", p[0], p[1], got)
		}
	}
}

func TestSafeURL(t *testing.T) {
	for u, want := range map[string]bool{
		DefaultDownloadURL:                true,
		"https://code.iflytek.com/x/{os}": true,
		"http://evil/remote.exe":          false,
		"https://ok/a b":                  false,
		"https://ok/\x1b[2J":              false,
		"javascript:alert(1)":             false,
		"":                                false,
		"https://ok/\u009b31m":            false, // C1 控制字符
	} {
		if got := SafeURL(u); got != want {
			t.Errorf("SafeURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestPlatformFromHost(t *testing.T) {
	cases := []struct {
		host, os, arch string
		ok             bool
	}{
		{"kechen12@LAPTOP-X1 Windows 11 amd64", "windows", "amd64", true},
		{"uos@uos-PC uos arm64", "linux", "arm64", true},
		{"root@srv ubuntu amd64", "linux", "amd64", true},
		{"me@mac darwin arm64", "darwin", "arm64", true},
		{"windows-pc@box ubuntu amd64", "linux", "amd64", true}, // 主机名里的 windows 不算
		{"WINDOWS-BUILD01 uos amd64", "linux", "amd64", true},   // identity 只剩主机名
		{"Windows 10 amd64", "windows", "amd64", true},          // identity 采集失败
		{"pc Windows amd64", "windows", "amd64", true},          // RtlGetVersion 失败
		{"me@bsd freebsd amd64", "freebsd", "amd64", true},      // os_other 平台不当成 Linux
		{"root@srv ubuntu x86_64", "linux", "amd64", true},      // system arch name
		{"root@srv ubuntu aarch64", "linux", "arm64", true},     // system arch name
		{"u@pc uos loong64", "linux", "loong64", true},          // 能推断，但 ExpandURL 会拒绝
		{"", "", "", false},
		{"amd64", "", "", false},
		{"u@pc amd64", "", "", false},
	}
	for _, c := range cases {
		goos, goarch, ok := PlatformFromHost(c.host)
		if goos != c.os || goarch != c.arch || ok != c.ok {
			t.Errorf("PlatformFromHost(%q) = %q %q %v, want %q %q %v", c.host, goos, goarch, ok, c.os, c.arch, c.ok)
		}
	}
}

func TestCheckUnknown(t *testing.T) {
	n, ok := CheckUnknown("v1.1.0", "https://example.test/releases/{version}")
	if !ok || n.Current != "旧版客户端" || n.URL != "https://example.test/releases/v1.1.0" {
		t.Fatalf("unknown notice = %+v, ok=%v", n, ok)
	}
	if _, ok := CheckUnknown("dev", "https://example.test/releases/{version}"); ok {
		t.Fatal("dev relay must not announce an unknown-client notice")
	}
	if n, ok := CheckUnknown("v1.1.0", "http://insecure/{version}"); !ok || n.URL != DefaultReleasePage[:strings.LastIndex(DefaultReleasePage, "{version}")]+"v1.1.0" {
		t.Fatalf("unsafe release page must fall back to default: %+v, ok=%v", n, ok)
	}
}

func TestCheck(t *testing.T) {
	n, ok := Check("0.0.7", "v1.1.0", DefaultDownloadURL, DefaultReleasePage, "linux", "amd64")
	if !ok || n.URL != "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-linux-amd64" {
		t.Fatalf("got %+v %v", n, ok)
	}
	n, ok = Check("0.0.7", "1.1.0", DefaultDownloadURL, DefaultReleasePage, "windows", "amd64")
	if !ok || n.Latest != "1.1.0" || n.URL != "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-windows-amd64.exe" {
		t.Fatalf("unprefixed release should use canonical tag: %+v %v", n, ok)
	}
	n, ok = Check("0.0.7", "1.1.0", "https://mirror.example/{version}/{os}-{arch}{ext}", "https://mirror.example/releases/{version}", "windows", "amd64")
	if !ok || n.Latest != "1.1.0" || n.URL != "https://mirror.example/1.1.0/windows-amd64.exe" {
		t.Fatalf("custom template should preserve configured version: %+v %v", n, ok)
	}
	n, ok = Check("0.0.7", "1.1.0", "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-{os}-{arch}{ext}", "https://github.com/kegechen/remote-assist-tool/releases/tag/v1.1.0", "windows", "amd64")
	if !ok || n.URL != "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-windows-amd64.exe" {
		t.Fatalf("literal release tag template should remain unchanged: %+v %v", n, ok)
	}
	n, ok = CheckPeer("0.0.7", "u@pc unknown 386", "1.1.0", "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-{os}-{arch}{ext}", DefaultReleasePage)
	if !ok || n.URL != "https://github.com/kegechen/remote-assist-tool/releases/tag/v1.1.0" {
		t.Fatalf("mixed literal download/default release page should use the canonical tag: %+v %v", n, ok)
	}
	if _, ok := Check("v1.1.0", "v1.1.0", DefaultDownloadURL, DefaultReleasePage, "linux", "amd64"); ok {
		t.Fatal("同版本不应提示")
	}
	// 平台未知、或无发布产物（龙芯）退回 Release 页
	for _, host := range []string{"", "u@pc uos loong64"} {
		n, ok = CheckPeer("0.0.7", host, "v1.1.0", DefaultDownloadURL, DefaultReleasePage)
		if !ok || n.URL != "https://github.com/kegechen/remote-assist-tool/releases/tag/v1.1.0" {
			t.Fatalf("%q 应退回 release 页: %+v %v", host, n, ok)
		}
	}
	// relay 没下发 latest（旧 relay）：不提示
	if _, ok := CheckSelf("0.0.7", "", "", ""); ok {
		t.Fatal("无 latest 不应提示")
	}
	// relay 下发的不安全链接被换成内置模板
	n, ok = Check("0.0.7", "v1.1.0", "http://evil/x-{os}", "http://evil/page", "windows", "amd64")
	if !ok || n.URL != "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-windows-amd64.exe" {
		t.Fatalf("unsafe template should fall back: %+v", n)
	}
	n, _ = Check("0.0.7", "v1.1.0", "https://ok/{os}", "http://evil/page", "", "")
	if n.URL != "https://github.com/kegechen/remote-assist-tool/releases/tag/v1.1.0" {
		t.Fatalf("unsafe release page should fall back: %+v", n)
	}
	// 对端上报的版本里的控制字符被去掉
	n, ok = Check("0.0.7-\x1b[2J", "v1.1.0", DefaultDownloadURL, DefaultReleasePage, "linux", "amd64")
	if !ok || n.Current != "0.0.7-[2J" {
		t.Fatalf("current not sanitized: %+v %v", n, ok)
	}
}
