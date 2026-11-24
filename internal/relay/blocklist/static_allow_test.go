package blocklist

// --blocklist-allow：启动参数里写死的放行名单（IP 或 CIDR）。命中的来源永远不计分、不被拒绝，
// 也不能被手动封禁；启动时名单文件里已有的、完全落在放行范围内的封禁会被解除。
// 没有运行时管理入口的部署者靠它兜住「自己的出口 / 公司 NAT 被误封」。

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustPrefixes(t *testing.T, spec string) []netip.Prefix {
	t.Helper()
	ps, err := ParseStaticAllow(spec)
	if err != nil {
		t.Fatalf("ParseStaticAllow(%q): %v", spec, err)
	}
	return ps
}

func TestParseStaticAllowGood(t *testing.T) {
	for spec, want := range map[string]string{
		"":                                "",
		"  ":                              "",
		"198.51.100.7":                    "198.51.100.7/32",
		" 198.51.100.7 , 203.0.113.0/24 ": "198.51.100.7/32,203.0.113.0/24",
		"198.51.100.9/24":                 "198.51.100.0/24", // 主机位清零
		"2001:db8::1":                     "2001:db8::1/128",
		"2001:db8:1::/48":                 "2001:db8:1::/48",
		"::ffff:198.51.100.7":             "198.51.100.7/32", // v4-mapped 按 IPv4 处理
		"192.0.2.1,192.0.2.1":             "192.0.2.1/32,192.0.2.1/32",
		strings.Repeat("192.0.2.1,", 255) + "192.0.2.2": strings.TrimSuffix(strings.Repeat("192.0.2.1/32,", 255), ",") + ",192.0.2.2/32",
	} {
		ps, err := ParseStaticAllow(spec)
		if err != nil {
			t.Errorf("ParseStaticAllow(%.30q) 不应报错：%v", spec, err)
			continue
		}
		var got []string
		for _, p := range ps {
			got = append(got, p.String())
		}
		if strings.Join(got, ",") != want {
			t.Errorf("ParseStaticAllow(%.30q) = %v，want %s", spec, got, want)
		}
	}
}

// 很宽但不是 /0 的范围由使用者自己负责：接受，前缀长度原样保留（不写出具体网段，免得测试里出现真实地址）。
func TestParseStaticAllowWideButNotZero(t *testing.T) {
	ps := mustPrefixes(t, "203.0.113.0/1")
	if len(ps) != 1 || ps[0].Bits() != 1 {
		t.Fatalf("/1 应被接受：%v", ps)
	}
}

func TestParseStaticAllowBad(t *testing.T) {
	for spec, hint := range map[string]string{
		"abc":                          "abc",
		"198.51.100.7/33":              "198.51.100.7/33",
		"198.51.100.7/24/1":            "198.51.100.7/24/1",
		"0.0.0.0/0":                    "/0",
		"::/0":                         "/0",
		"198.51.100.7,,203.0.113.1":    "empty",
		"198.51.100.7,":                "empty",
		",198.51.100.7":                "empty",
		"fe80::1%eth0":                 "fe80::1%eth0",
		"::ffff:198.51.100.0/120":      "::ffff:198.51.100.0/120",
		"dev-0123456789abcdef01234567": "device",
		strings.Repeat("192.0.2.1,", 256) + "192.0.2.2": "at most",
	} {
		_, err := ParseStaticAllow(spec)
		if err == nil {
			t.Errorf("ParseStaticAllow(%.30q) 应报错", spec)
			continue
		}
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("ParseStaticAllow(%.30q) 错误应提到 %q：%v", spec, hint, err)
		}
	}
}

func openStatic(t *testing.T, spec string) (*List, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.StaticAllow = mustPrefixes(t, spec)
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, clk
}

// 放行范围内的来源怎么犯规都不封；范围外的邻居照常封。
func TestStaticAllowNeverBansAllowed(t *testing.T) {
	l, _ := openStatic(t, "198.51.100.7,203.0.113.0/24")
	for _, ip := range []string{"198.51.100.7", "203.0.113.1", "203.0.113.254", "::ffff:198.51.100.7"} { // 最后一个：双栈监听下的 v4-mapped 来源
		if hit(l, ip, KindJoinFailed, 5*l.Config().Threshold) {
			t.Errorf("%s 在放行范围内，不应被封", ip)
		}
		if !l.CheckAccept(ip) || l.IsBanned(ip) {
			t.Errorf("%s 在放行范围内，应放行", ip)
		}
	}
	for _, ip := range []string{"198.51.100.8", "192.0.2.77"} {
		if !hit(l, ip, KindJoinFailed, l.Config().Threshold) {
			t.Errorf("%s 不在放行范围内，应照常被封", ip)
		}
		if l.CheckAccept(ip) {
			t.Errorf("%s 应被拒绝", ip)
		}
	}
}

// 放行的来源不占跟踪表：不为它们建条目（否则扫描器可借此填满表）。
func TestStaticAllowDoesNotTrackViolations(t *testing.T) {
	l, _ := openStatic(t, "203.0.113.0/24")
	for i := 0; i < 50; i++ {
		l.RecordViolation(fmt.Sprintf("203.0.113.%d", i), KindJoinFailed, "x")
	}
	if n := trackedCount(l); n != 0 {
		t.Fatalf("放行范围内的违规不应建条目，实际 %d 条", n)
	}
}

// IPv6 的键是 /64：放行单个地址时，同 /64 的其他地址仍可被自动封（封的是 /64），被放行的那个地址不受影响。
func TestStaticAllowIPv6SingleAddressInsideSlash64(t *testing.T) {
	l, _ := openStatic(t, "2001:db8:1:2::5")
	if hit(l, "2001:db8:1:2::5", KindJoinFailed, 5*l.Config().Threshold) {
		t.Fatal("被放行的地址不应被封")
	}
	if !hit(l, "2001:db8:1:2::6", KindJoinFailed, l.Config().Threshold) {
		t.Fatal("同 /64 的邻居应照常被自动封禁")
	}
	if l.CheckAccept("2001:db8:1:2::6") {
		t.Error("被封 /64 里的邻居应被拒绝")
	}
	if !l.CheckAccept("2001:db8:1:2::5") {
		t.Error("被放行的地址即使所在 /64 被封也应放行")
	}
}

// 手动封禁完全落在放行范围内的键：明确拒绝，不能静默无效。
func TestStaticAllowRefusesManualBan(t *testing.T) {
	// 2001:db8:a:a::/96 包含该 /64 的基址，但窄于 /64：它没有覆盖整个 /64 键，手动封这个 /64 不应被拒。
	l, _ := openStatic(t, "198.51.100.7,203.0.113.0/24,2001:db8:1::/48,2001:db8:9:9::5,2001:db8:a:a::/96")
	for _, ip := range []string{"198.51.100.7", "203.0.113.9", "2001:db8:1:77::1", "2001:db8:1:77::/64"} {
		if _, err := l.Ban(ip, time.Hour, false, "op", ""); !errors.Is(err, ErrStaticAllowed) {
			t.Errorf("Ban(%s) 应返回 ErrStaticAllowed，got %v", ip, err)
		}
		if l.IsBanned(ip) {
			t.Errorf("%s 不应被封", ip)
		}
	}
	// 不在范围内，或范围只覆盖 /64 的一部分（放行的是单个地址）：手动封禁照常，放行的地址靠 CheckAccept 绕过。
	for _, ip := range []string{"198.51.100.8", "192.0.2.77", "2001:db8:9:9::7", "2001:db8:a:a::/64"} {
		if _, err := l.Ban(ip, time.Hour, false, "op", ""); err != nil {
			t.Errorf("Ban(%s) 应成功：%v", ip, err)
		}
	}
	if !l.CheckAccept("2001:db8:9:9::5") {
		t.Error("单个被放行的地址不受所在 /64 手动封禁影响")
	}
	if l.CheckAccept("2001:db8:9:9::7") {
		t.Error("同 /64 的其他地址应被拒绝")
	}
	if !l.CheckAccept("2001:db8:a:a::5") || l.CheckAccept("2001:db8:a:a:0:1::5") {
		t.Error("/96 之内的放行，/96 之外（同一被封 /64）的拒绝")
	}
}

// 重启时名单文件里已有的封禁：完全落在放行范围内的解除（记录以「人工解禁 / config」结束），范围外的保留。
func TestStaticAllowReleasesExistingBansOnStart(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	if _, err := env.l.Ban("198.51.100.7", 0, true, "op", "手动"); err != nil {
		t.Fatal(err)
	}
	autoBan(t, env.l, "203.0.113.9")
	autoBan(t, env.l, "192.0.2.1")
	env.close()

	env.mutate = func(c *Config) { c.StaticAllow = mustPrefixes(t, "198.51.100.0/24,203.0.113.9") }
	env.open()
	for _, ip := range []string{"198.51.100.7", "203.0.113.9"} {
		if env.l.IsBanned(ip) || !env.l.CheckAccept(ip) {
			t.Errorf("%s 在放行范围内，启动后应已解除", ip)
		}
		r := onlyRec(t, env.r, ip)
		if r.End != EndUnban || r.EndActor != StaticAllowActor {
			t.Errorf("%s 的记录应以 unban/%s 结束：%+v", ip, StaticAllowActor, r)
		}
	}
	if !env.l.IsBanned("192.0.2.1") {
		t.Error("范围外的封禁应保留")
	}
	if r := onlyRec(t, env.r, "192.0.2.1"); r.End != "" {
		t.Errorf("范围外的记录应仍在生效：%+v", r)
	}
	env.close()

	// 解除要落盘：再次重启（不带放行参数）也不会复活。
	env.mutate = nil
	env.open()
	if env.l.IsBanned("198.51.100.7") || env.l.IsBanned("203.0.113.9") {
		t.Error("解除应已持久化")
	}
}

// 没配放行名单时行为不变（回归）：这一整块逻辑对未配置的部署不能有任何影响。
func TestStaticAllowEmptyIsNoop(t *testing.T) {
	l := openMem(t, newFakeClock())
	if !hit(l, "198.51.100.7", KindJoinFailed, l.Config().Threshold) || l.CheckAccept("198.51.100.7") {
		t.Fatal("未配置时应照常封禁")
	}
	if _, err := l.Ban("198.51.100.8", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
}
