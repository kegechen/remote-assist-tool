// Package update 实现"发现新版本"软提示：relay 在注册/加入响应里下发最新版本号与下载
// 链接模板，客户端与自身版本比较后打印提示。只做提示，从不拒绝连接——旧版本照常可用。
//
// relay 下发的内容不完全可信（--plain 部署、被劫持的 relay）：最新版本必须是严格的
// X.Y.Z，链接必须是 https 且不含控制字符，否则退回内置模板；版本号与平台解析不了
// （dev 构建、未知平台）就不提示或退回 Release 页，绝不因此报错。
package update

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	// DefaultDownloadURL 默认下载链接模板，对应 release.yml 产物命名
	// remote-assist-cli-<os>-<arch>[.exe]。锁定到提示的版本，而不是 latest：
	// 避免 Release 产物尚未上传、或 latest 指向别的版本时，提示与实际下载对不上。
	DefaultDownloadURL = "https://github.com/kegechen/remote-assist-tool/releases/download/{version}/remote-assist-cli-{os}-{arch}{ext}"
	// DefaultReleasePage 平台推断不出来时给的兜底链接。
	DefaultReleasePage = "https://github.com/kegechen/remote-assist-tool/releases/tag/{version}"
)

// releasedPlatforms release.yml 实际发布产物的平台；其余平台展开模板只会得到 404。
var releasedPlatforms = map[string]bool{
	"windows/amd64": true, "windows/arm64": true,
	"linux/amd64": true, "linux/arm64": true,
	"darwin/amd64": true, "darwin/arm64": true,
}

// describeSuffix git describe 追加的 "-<提交数>-g<哈希>"，表示比该 tag 更新，不是预发布。
var describeSuffix = regexp.MustCompile(`^-\d+-g[0-9a-f]+(-dirty)?$`)

// version 解析结果。pre 为 true 表示带预发布后缀（如 -rc1），低于同号正式版。
type version struct {
	num [3]int
	pre bool
}

func parse(s string) (v version, suffix string, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	core := s
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core, suffix = s[:i], s[i:]
	}
	parts := strings.Split(core, ".")
	// 恰好三段：挡住纯数字 commit 短哈希（git describe --always 在无 tag 时的输出）。
	if len(parts) != 3 {
		return v, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return v, "", false
		}
		v.num[i] = n
	}
	v.pre = suffix != "" && !strings.HasPrefix(suffix, "+") && !describeSuffix.MatchString(suffix)
	return v, suffix, true
}

// ParseVersion 解析 "1.2.3" / "v1.2.3" / "1.2.3-4-gabc" / "1.2.3-rc1"，必须恰好三段。
func ParseVersion(s string) (v [3]int, ok bool) {
	pv, _, ok := parse(s)
	return pv.num, ok
}

// IsRelease 报告 s 是否为不带任何后缀的正式版本号（X.Y.Z 或 vX.Y.Z）。
// 只有这样的版本才适合作为"最新版本"对外宣称。
func IsRelease(s string) bool {
	_, suffix, ok := parse(s)
	return ok && suffix == ""
}

// NormalizeRelease returns the canonical release tag form used by published
// artifacts. Configuration accepts both 1.2.3 and v1.2.3, while download and
// release URLs must use the repository tag form with the leading v.
func NormalizeRelease(s string) (string, bool) {
	v, suffix, ok := parse(s)
	if !ok || suffix != "" {
		return "", false
	}
	return fmt.Sprintf("v%d.%d.%d", v.num[0], v.num[1], v.num[2]), true
}

// NormalizeDefaultURLVersion keeps the built-in GitHub fallback compatible
// with release tags that use a leading v. Explicit templates, including a
// template with a literal v1.2.3 tag or a fixed download URL, are left alone;
// their owner is responsible for publishing a reachable URL.
func NormalizeDefaultURLVersion(latest, downloadURL, releasePage string) string {
	if (downloadURL != "" && downloadURL != DefaultDownloadURL && strings.Contains(downloadURL, "{version}")) ||
		(releasePage != "" && releasePage != DefaultReleasePage && strings.Contains(releasePage, "{version}")) {
		return latest
	}
	if normalized, ok := NormalizeRelease(latest); ok {
		return normalized
	}
	return latest
}

func normalizeTemplateVersion(latest, template string) string {
	if template == DefaultDownloadURL || template == DefaultReleasePage {
		if normalized, ok := NormalizeRelease(latest); ok {
			return normalized
		}
	}
	return latest
}

// Newer 报告 latest 是否严格高于 current。latest 必须是正式版本；current 无法解析
// （dev 构建）返回 false。同号时预发布版（-rc1）低于正式版。
func Newer(latest, current string) bool {
	if !IsRelease(latest) {
		return false
	}
	l, _, _ := parse(latest)
	c, _, ok := parse(current)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if l.num[i] != c.num[i] {
			return l.num[i] > c.num[i]
		}
	}
	return c.pre
}

// SafeURL 报告 u 是否可以展示给用户：https、无控制字符与空白。
func SafeURL(u string) bool {
	if !strings.HasPrefix(u, "https://") || len(u) > 2048 {
		return false
	}
	for _, r := range u {
		if r <= 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// ExpandURL 把模板中的 {version}/{os}/{arch}/{ext} 替换为目标版本与平台。
// 平台不在已发布产物之列时返回 ""。
func ExpandURL(template, ver, goos, goarch string) string {
	goarch = normalizeArch(goarch)
	if template == "" || !releasedPlatforms[goos+"/"+goarch] {
		return ""
	}
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return expand(template, ver, goos, goarch, ext)
}

func expand(template, ver, goos, goarch, ext string) string {
	r := strings.NewReplacer("{version}", ver, "{os}", goos, "{arch}", goarch, "{ext}", ext)
	return r.Replace(template)
}

// normalizeArch converts names commonly reported by system tools to the
// architecture names used by release assets. Unknown names remain unchanged
// so ExpandURL can reject them and fall back to the release page.
func normalizeArch(arch string) string {
	arch = strings.ToLower(strings.TrimSpace(arch))
	switch arch {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return arch
	}
}

// PlatformFromHost 从 sysinfo.Summary 身份串（"user@host Windows 11 amd64"、
// "uos@pc uos arm64"）推断 GOOS/GOARCH。只看末尾：最后一个字段是架构，前面是
// osName——Windows 为 "Windows <版本>"，其余平台为单个字段（Linux 是发行版 ID，
// 其它平台是 GOOS）。identity 可能只剩主机名、不带 '@'，所以不从开头判断。
func PlatformFromHost(host string) (goos, goarch string, ok bool) {
	f := strings.Fields(host)
	if len(f) < 2 {
		return "", "", false
	}
	goarch = normalizeArch(f[len(f)-1])
	sys := f[:len(f)-1]
	last := sys[len(sys)-1]
	switch {
	case strings.EqualFold(last, "windows"),
		len(sys) >= 2 && strings.EqualFold(sys[len(sys)-2], "windows"):
		goos = "windows"
	case last == "darwin":
		goos = "darwin"
	case strings.Contains(last, "@"):
		return "", "", false // 缺 osName
	case isOtherGOOS(last):
		goos = last
	default:
		goos = "linux"
	}
	return goos, goarch, true
}

// isOtherGOOS os_other.go 直接返回 runtime.GOOS 的平台，不能当成 Linux 发行版 ID。
func isOtherGOOS(s string) bool {
	switch s {
	case "freebsd", "openbsd", "netbsd", "dragonfly", "solaris", "illumos", "aix", "plan9", "android", "ios", "js", "wasip1":
		return true
	}
	return false
}

// Notice 一条升级提示。
type Notice struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	URL     string `json:"download_url"`
}

// String 渲染成一行给人看的提示。
func (n Notice) String() string {
	return fmt.Sprintf("发现新版本 %s（当前 %s），建议升级，下载：%s", n.Latest, n.Current, n.URL)
}

// PeerString 渲染成针对被协助端的提示。
func (n Notice) PeerString() string {
	return fmt.Sprintf("被协助端版本 %s 低于最新版 %s，建议对方升级，下载：%s", n.Current, n.Latest, n.URL)
}

// Check 判断 current 是否落后于 latest；落后时按 goos/goarch 展开下载链接，
// 平台无发布产物时退回 releasePage。不安全的模板一律换成内置默认值。
func Check(current, latest, urlTemplate, releasePage, goos, goarch string) (Notice, bool) {
	if !Newer(latest, current) {
		return Notice{}, false
	}
	urlVersion := latest
	if urlTemplate == DefaultDownloadURL {
		urlVersion = normalizeTemplateVersion(latest, urlTemplate)
	}
	url := ExpandURL(urlTemplate, urlVersion, goos, goarch)
	if !SafeURL(url) {
		fallbackVersion := normalizeTemplateVersion(latest, DefaultDownloadURL)
		url = ExpandURL(DefaultDownloadURL, fallbackVersion, goos, goarch)
	}
	if url == "" {
		releaseVersion := normalizeTemplateVersion(latest, releasePage)
		url = expand(releasePage, releaseVersion, "", "", "")
		if !SafeURL(url) {
			fallbackVersion := normalizeTemplateVersion(latest, DefaultReleasePage)
			url = expand(DefaultReleasePage, fallbackVersion, "", "", "")
		}
	}
	return Notice{Current: printable(current), Latest: latest, URL: url}, true
}

// printable 去掉控制字符，current 可能来自对端身份信息。
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// CheckSelf 以本进程的平台检查 current。
func CheckSelf(current, latest, urlTemplate, releasePage string) (Notice, bool) {
	return Check(current, latest, urlTemplate, releasePage, runtime.GOOS, runtime.GOARCH)
}

// CheckPeer 以对端身份串推断平台检查 peerVersion。
func CheckPeer(peerVersion, peerHost, latest, urlTemplate, releasePage string) (Notice, bool) {
	goos, goarch, _ := PlatformFromHost(peerHost)
	return Check(peerVersion, latest, urlTemplate, releasePage, goos, goarch)
}

// CheckUnknown 为没有上报版本号的旧客户端生成通用提示。早期 0.0.x 客户端没有
// Version 字段，relay 无法判断它是否落后；只要 relay 明确宣布了正式版本，就给出
// Release 页面，确保用户仍能看到可操作的升级入口。
func CheckUnknown(latest, releasePage string) (Notice, bool) {
	if !IsRelease(latest) {
		return Notice{}, false
	}
	releaseVersion := normalizeTemplateVersion(latest, releasePage)
	url := expand(releasePage, releaseVersion, "", "", "")
	if !SafeURL(url) {
		fallbackVersion := normalizeTemplateVersion(latest, DefaultReleasePage)
		url = expand(DefaultReleasePage, fallbackVersion, "", "", "")
	}
	return Notice{Current: "旧版客户端", Latest: latest, URL: url}, true
}
