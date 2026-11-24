package blocklist

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 可手动推进的时钟，让窗口、递增、衰减、宽限期都能确定性地测试。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testConfig(clk *fakeClock) Config {
	cfg := DefaultConfig()
	cfg.Now = clk.Now
	return cfg
}

func openMem(t *testing.T, clk *fakeClock) *List {
	t.Helper()
	l, err := Open(testConfig(clk))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func hit(l *List, ip string, kind Kind, n int) bool {
	banned := false
	for i := 0; i < n; i++ {
		if l.RecordViolation(ip, kind, "test") {
			banned = true
		}
	}
	return banned
}

func entryOf(t *testing.T, l *List, ip string) EntryView {
	t.Helper()
	e, ok := l.Detail(ip)
	if !ok {
		t.Fatalf("没有 %s 的详情", ip)
	}
	return e
}

func TestNormalizeIP(t *testing.T) {
	good := map[string]string{
		"203.0.113.7":          "203.0.113.7",
		" 203.0.113.7 ":        "203.0.113.7",
		"::ffff:203.0.113.7":   "203.0.113.7",
		"2001:DB8::1":          "2001:db8::1",
		"fe80::1%eth0":         "fe80::1",
		"2001:0db8:0000::0001": "2001:db8::1",
	}
	for in, want := range good {
		got, err := NormalizeIP(in)
		if err != nil || got != want {
			t.Errorf("NormalizeIP(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "10.0.0.0/8", "203.0.113.7:8443", "example.com", "1.2.3", "::1/128", "1.2.3.4 5"} {
		if _, err := NormalizeIP(bad); !errors.Is(err, ErrInvalidIP) {
			t.Errorf("NormalizeIP(%q) err=%v, want ErrInvalidIP", bad, err)
		}
	}
}

// 只有窗口内累计分值达到阈值才封；窗口外的命中自动失效。
func TestScoringThresholdAndWindow(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "198.51.100.1"

	if hit(l, ip, KindTLSGarbage, 19) {
		t.Fatal("19 分（阈值 20）不应封禁")
	}
	if l.IsBanned(ip) {
		t.Fatal("未达阈值却被封")
	}
	// 窗口过去，旧命中失效，再来 19 次仍不应封。
	clk.Advance(11 * time.Minute)
	if hit(l, ip, KindTLSGarbage, 19) {
		t.Fatal("窗口外的旧命中不应累计")
	}
	if got := entryOf(t, l, ip).Points; got != 19 {
		t.Fatalf("窗口内分值=%d，want 19", got)
	}
	if !hit(l, ip, KindTLSGarbage, 1) {
		t.Fatal("达到阈值应封禁")
	}
	if !l.IsBanned(ip) {
		t.Fatal("IsBanned 应为 true")
	}
	e := entryOf(t, l, ip)
	if e.Ban == nil || e.Ban.Reason != "auto" || e.Ban.Actor != "auto" || e.Status != "banned" {
		t.Fatalf("封禁信息不对: %+v", e.Ban)
	}
	if e.Points != 0 {
		t.Fatalf("封禁后应清空窗口分值，got %d", e.Points)
	}
	if e.Ban.Until-e.Ban.Since != int64(time.Hour/time.Second) {
		t.Fatalf("首次封禁应为 1h，got %ds", e.Ban.Until-e.Ban.Since)
	}
}

// 协议错误比握手失败重；Join 爆破第二条连接就该被封。
func TestKindWeights(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	if hit(l, "198.51.100.2", KindProtoDecode, 9) {
		t.Fatal("9 次 proto_decode=18 分，不应封")
	}
	if !hit(l, "198.51.100.2", KindProtoState, 1) {
		t.Fatal("proto_state 2 分，累计 20 应封")
	}
	// 爆破：每条连接 5 次 join_failed + 1 次 exceeded = 10 分。
	const brute = "198.51.100.3"
	hit(l, brute, KindJoinFailed, 5)
	if hit(l, brute, KindJoinFailuresExceeded, 1) {
		t.Fatal("第一条爆破连接（10 分）不应封")
	}
	hit(l, brute, KindJoinFailed, 5)
	if !hit(l, brute, KindJoinFailuresExceeded, 1) {
		t.Fatal("第二条爆破连接累计 20 分应封")
	}
}

func TestEscalationCapAndDecay(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "198.51.100.4"
	want := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for i, d := range want {
		if !hit(l, ip, KindProtoDecode, 10) {
			t.Fatalf("第 %d 次应封禁", i+1)
		}
		e := entryOf(t, l, ip)
		if got := time.Duration(e.Ban.Until-e.Ban.Since) * time.Second; got != d {
			t.Fatalf("第 %d 次封禁时长=%s，want %s", i+1, got, d)
		}
		if e.Offenses != i+1 || e.BanCount != i+1 {
			t.Fatalf("第 %d 次：offenses=%d bancount=%d", i+1, e.Offenses, e.BanCount)
		}
		clk.Advance(d + time.Second)
		l.Sweep()
		if l.IsBanned(ip) {
			t.Fatalf("第 %d 次封禁到期后仍被封", i+1)
		}
	}
	// 7 天没有新封禁：递增次数衰减清零，下次从 1h 开始。
	clk.Advance(7*24*time.Hour + time.Minute)
	l.Sweep()
	hit(l, ip, KindProtoDecode, 10)
	e := entryOf(t, l, ip)
	if got := time.Duration(e.Ban.Until-e.Ban.Since) * time.Second; got != time.Hour {
		t.Fatalf("衰减后封禁时长=%s，want 1h", got)
	}
	if e.Offenses != 1 || e.BanCount != len(want)+1 {
		t.Fatalf("衰减后 offenses=%d bancount=%d", e.Offenses, e.BanCount)
	}
	hasExpired := false
	for _, h := range e.History {
		if h.Action == "ban_expired" {
			hasExpired = true
		}
	}
	if !hasExpired {
		t.Fatal("到期解除应写入处置历史")
	}
}

func TestAllowlistNeverAutoBannedAndExpires(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "203.0.113.10"
	if err := l.Allow(ip, 30*24*time.Hour, false, "alice", "公司出口"); err != nil {
		t.Fatal(err)
	}
	if hit(l, ip, KindProtoDecode, 50) || l.IsBanned(ip) {
		t.Fatal("白名单 IP 不得被自动封禁")
	}
	e := entryOf(t, l, ip)
	if e.Status != "allowlisted" || e.Allow == nil || e.Allow.Actor != "alice" || e.Allow.Note != "公司出口" {
		t.Fatalf("白名单视图不对: %+v", e)
	}
	if len(e.Events) == 0 || e.Points != 0 || e.Events[0].Points != 0 {
		t.Fatal("白名单 IP 的违规仍应记录以便观察，但不计分（命中记录不增长）")
	}
	// 过期后恢复正常计分。
	clk.Advance(30*24*time.Hour + time.Minute)
	l.Sweep()
	if entryOf(t, l, ip).Allow != nil {
		t.Fatal("白名单到期应移除")
	}
	if !hit(l, ip, KindProtoDecode, 10) {
		t.Fatal("白名单到期后应恢复自动封禁")
	}
	// 永久白名单不过期。
	const perm = "203.0.113.11"
	if err := l.Allow(perm, 0, true, "alice", ""); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3650 * 24 * time.Hour)
	l.Sweep()
	if a := entryOf(t, l, perm).Allow; a == nil || !a.Permanent {
		t.Fatal("永久白名单不应过期")
	}
	if err := l.Disallow(perm, "bob", "离职"); err != nil {
		t.Fatal(err)
	}
	if entryOf(t, l, perm).Allow != nil {
		t.Fatal("移出白名单后不应再有白名单")
	}
	if err := l.Disallow(perm, "bob", ""); !errors.Is(err, ErrNotAllowlisted) {
		t.Fatalf("重复移出 err=%v", err)
	}
}

// 手动封禁覆盖白名单（带 warning），解禁后白名单仍然有效。
func TestManualBanOverridesAllowlist(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "203.0.113.12"
	if err := l.Allow(ip, 0, true, "alice", ""); err != nil {
		t.Fatal(err)
	}
	var closed []string
	l.SetOnManualBan(func(ip string) { closed = append(closed, ip) })
	res, err := l.Ban(ip, time.Hour, false, "bob", "排查中")
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning == "" {
		t.Fatal("封禁白名单 IP 应返回 warning")
	}
	if !l.IsBanned(ip) {
		t.Fatal("手动封禁应覆盖白名单")
	}
	if len(closed) != 1 || closed[0] != ip {
		t.Fatalf("手动封禁应回调断开现有连接: %v", closed)
	}
	e := entryOf(t, l, ip)
	if e.Ban.Reason != "manual" || e.Ban.Actor != "bob" || e.Ban.Note != "排查中" || e.Allow == nil {
		t.Fatalf("视图不对: ban=%+v allow=%+v", e.Ban, e.Allow)
	}
	if err := l.Unban(ip, "bob", "", nil); err != nil {
		t.Fatal(err)
	}
	if l.IsBanned(ip) || entryOf(t, l, ip).Allow == nil {
		t.Fatal("解禁后应不再封禁，且白名单保留")
	}
	// 永久手动封禁
	if _, err := l.Ban("203.0.113.13", 0, true, "bob", "x"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3650 * 24 * time.Hour)
	l.Sweep()
	if !l.IsBanned("203.0.113.13") {
		t.Fatal("永久封禁不应过期")
	}
	if entryOf(t, l, "203.0.113.13").Offenses != 0 {
		t.Fatal("手动封禁不计入递增次数")
	}
}

// 解禁 = 移除封禁 + 清零分值和递增 + 宽限期内不自动封；可选同时加白。
func TestUnbanResetsAndGrace(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "198.51.100.20"
	hit(l, ip, KindProtoDecode, 10) // 第 1 次
	clk.Advance(2 * time.Hour)
	l.Sweep()
	hit(l, ip, KindProtoDecode, 10) // 第 2 次，offenses=2
	if !l.IsBanned(ip) {
		t.Fatal("前置：应被封")
	}
	if err := l.Unban(ip, "alice", "确认是自己人", nil); err != nil {
		t.Fatal(err)
	}
	e := entryOf(t, l, ip)
	if l.IsBanned(ip) || e.Offenses != 0 || e.Points != 0 || e.Status != "grace" {
		t.Fatalf("解禁后状态不对: banned=%v offenses=%d points=%d status=%s", l.IsBanned(ip), e.Offenses, e.Points, e.Status)
	}
	if e.GraceUntil != clk.Now().Add(24*time.Hour).Unix() {
		t.Fatalf("宽限期到 %d，want +24h", e.GraceUntil)
	}
	last := e.History[len(e.History)-1]
	if last.Action != "unban" || last.Actor != "alice" || last.Note != "确认是自己人" {
		t.Fatalf("处置历史不对: %+v", last)
	}
	if hit(l, ip, KindProtoDecode, 50) {
		t.Fatal("宽限期内不得自动封禁")
	}
	clk.Advance(24*time.Hour + time.Minute)
	l.Sweep()
	if !hit(l, ip, KindProtoDecode, 10) {
		t.Fatal("宽限期过后应恢复自动封禁")
	}
	if got := entryOf(t, l, ip); got.Ban.Until-got.Ban.Since != int64(time.Hour/time.Second) {
		t.Fatal("解禁已清零递增，再封应从 1h 开始")
	}
	// 解禁同时加入白名单
	if err := l.Unban(ip, "alice", "", &AllowSpec{Duration: 30 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	e = entryOf(t, l, ip)
	if e.Allow == nil || e.Allow.Until != clk.Now().Add(30*24*time.Hour).Unix() {
		t.Fatalf("解禁并加白失败: %+v", e.Allow)
	}
	if err := l.Unban(ip, "alice", "", nil); !errors.Is(err, ErrNotBanned) {
		t.Fatalf("未封禁时解禁 err=%v", err)
	}
}

func TestClientIdentificationAndVerdict(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "203.0.113.30"
	// 先来 15 个不同的协助端（超过 10 个上限，最旧的被淘汰），再来被协助端。
	for i := 0; i < 15; i++ {
		clk.Advance(time.Second)
		l.ObserveClient(ip, ClientInfo{Role: "help", Version: "0.0.9", Host: "u" + string(rune('a'+i)) + "@pc windows amd64"})
	}
	l.ObserveConnect(ip)
	l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", Host: "alice@alice-pc kylin arm64", ClientID: "secret-client-id"})
	clk.Advance(time.Minute)
	l.ObserveConnect(ip)
	l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", Host: "alice@alice-pc kylin arm64", ClientID: "secret-client-id"})
	e := entryOf(t, l, ip)
	if e.Connections != 2 || e.FirstSeen == 0 || e.LastSeen <= e.FirstSeen {
		t.Fatalf("连接统计不对: %+v", e)
	}
	if len(e.Clients) != 10 {
		t.Fatalf("客户端列表应有界为 10，got %d", len(e.Clients))
	}
	if e.Clients[0].Role != "share" || e.Clients[0].Count != 2 {
		t.Fatalf("同一 ClientID 应合并且按最近出现排在最前: %+v", e.Clients[0])
	}
	for _, c := range e.Clients {
		if strings.Contains(c.ClientFP, "secret") {
			t.Fatal("客户端指纹不得包含原始 ClientID")
		}
	}
	if !strings.Contains(e.Verdict, "仅有未认证的注册（自报：被协助端 v1.1.0, alice@alice-pc kylin arm64") {
		t.Fatalf("仅注册时的判定提示=%q", e.Verdict)
	}
	// 被成功加入（完成配对）之后才算成功客户端；身份串仍注明为自报。
	l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", Host: "alice@alice-pc kylin arm64", ClientID: "secret-client-id", Verified: true})
	if v := entryOf(t, l, ip).Verdict; !strings.Contains(v, "曾有成功加入/完成配对的客户端（自报：被协助端 v1.1.0, alice@alice-pc kylin arm64）") {
		t.Fatalf("判定提示=%q", v)
	}
	// 同一 ClientID 指纹稳定。
	l.ObserveClient("203.0.113.31", ClientInfo{Role: "share", ClientID: "secret-client-id"})
	var fp1, fp2 string
	for _, c := range entryOf(t, l, ip).Clients {
		if c.Role == "share" {
			fp1 = c.ClientFP
		}
	}
	fp2 = entryOf(t, l, "203.0.113.31").Clients[0].ClientFP
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("指纹应稳定且非空: %q %q", fp1, fp2)
	}

	const scanner = "198.51.100.40"
	l.ObserveConnect(scanner)
	hit(l, scanner, KindTLSGarbage, 3)
	if v := entryOf(t, l, scanner).Verdict; !strings.Contains(v, "从未完成握手") {
		t.Fatalf("扫描器判定=%q", v)
	}
	const brute = "198.51.100.41"
	hit(l, brute, KindJoinFailed, 3)
	if v := entryOf(t, l, brute).Verdict; !strings.Contains(v, "爆破") {
		t.Fatalf("爆破判定=%q", v)
	}
	// 事件列表有界
	hit(l, "198.51.100.42", KindTLSGarbage, 19)
	clk.Advance(11 * time.Minute)
	hit(l, "198.51.100.42", KindTLSGarbage, 19)
	if n := len(entryOf(t, l, "198.51.100.42").Events); n != 20 {
		t.Fatalf("事件列表应有界为 20，got %d", n)
	}
}

func TestBannedRejectionsCounted(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "198.51.100.50"
	if !l.CheckAccept(ip) {
		t.Fatal("未封禁时应允许")
	}
	if _, err := l.Ban(ip, time.Hour, false, "bob", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if l.CheckAccept(ip) {
			t.Fatal("封禁时应拒绝")
		}
	}
	if got := entryOf(t, l, ip).Rejected; got != 3 {
		t.Fatalf("被拒计数=%d", got)
	}
	clk.Advance(time.Hour + time.Second)
	if !l.CheckAccept(ip) {
		t.Fatal("到期但尚未清扫的封禁应视为未封禁")
	}
}

func TestEvictionKeepsProtectedEntries(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 10
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Ban("192.0.2.1", time.Hour, false, "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Allow("192.0.2.2", 0, true, "a", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		clk.Advance(time.Second)
		l.ObserveConnect("10.0.0." + itoa(i))
	}
	all := trackedIPs(l)
	if len(all) > 12 {
		t.Fatalf("可淘汰条目应 ≤ 10（另有 2 个钉住条目），got %d", len(all))
	}
	seen := map[string]bool{}
	for _, ip := range all {
		seen[ip] = true
	}
	if !seen["192.0.2.1"] || !seen["192.0.2.2"] {
		t.Fatal("封禁和白名单条目不得被淘汰")
	}
	if !seen["10.0.0.49"] || seen["10.0.0.0"] {
		t.Fatal("应淘汰最久未出现的普通条目")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	clk := newFakeClock()
	dir := t.TempDir()
	cfg := testConfig(clk)
	cfg.Path = filepath.Join(dir, "blocklist.json")
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Persistent() {
		t.Fatal("指定路径应为持久化模式")
	}
	l.ObserveConnect("198.51.100.60")
	l.ObserveClient("198.51.100.60", ClientInfo{Role: "share", Version: "v1.1.0", Host: "x@y kylin arm64", ClientID: "cid-1"})
	hit(l, "198.51.100.60", KindProtoDecode, 10)
	if err := l.Allow("203.0.113.61", 0, true, "alice", "出口"); err != nil {
		t.Fatal(err)
	}
	hit(l, "198.51.100.62", KindProtoDecode, 10)
	if err := l.Unban("198.51.100.62", "alice", "误封", nil); err != nil {
		t.Fatal(err)
	}
	l.ObserveConnect("198.51.100.63") // 普通观察条目，不持久化
	before := entryOf(t, l, "198.51.100.60")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(cfg.Path); err != nil {
		t.Fatal(err)
	} else if !isWindows() && info.Mode().Perm() != 0600 {
		t.Fatalf("文件权限=%v，want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(cfg.Path)
	if strings.Contains(string(data), "cid-1") {
		t.Fatal("文件中不得出现原始 ClientID")
	}

	l2, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if !l2.IsBanned("198.51.100.60") {
		t.Fatal("重启后封禁应保留")
	}
	e := entryOf(t, l2, "198.51.100.60")
	if len(e.Clients) != 1 || e.Clients[0].Host != "x@y kylin arm64" || e.Offenses != 1 || len(e.Events) == 0 {
		t.Fatalf("识别快照未保留: %+v", e)
	}
	if e.Clients[0].ClientFP != before.Clients[0].ClientFP {
		t.Fatal("salt 应持久化，重启后指纹不变")
	}
	if a := entryOf(t, l2, "203.0.113.61").Allow; a == nil || !a.Permanent || a.Note != "出口" {
		t.Fatalf("白名单未保留: %+v", a)
	}
	g := entryOf(t, l2, "198.51.100.62")
	if g.Status != "grace" || g.History[len(g.History)-1].Actor != "alice" {
		t.Fatalf("宽限期/历史未保留: %+v", g)
	}
	for _, ip := range trackedIPs(l2) {
		if ip == "198.51.100.63" {
			t.Fatal("普通观察条目不应持久化")
		}
	}
}

// 第二个进程打不开同一文件；损坏文件不被覆盖，退回纯内存模式但仍可用。
func TestPersistenceLockAndCorruptFile(t *testing.T) {
	clk := newFakeClock()
	dir := t.TempDir()
	cfg := testConfig(clk)
	cfg.Path = filepath.Join(dir, "blocklist.json")
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := Open(cfg)
	if err == nil {
		t.Fatal("同一文件第二次打开应因锁失败")
	}
	if l2 == nil || l2.Persistent() {
		t.Fatal("打开失败时应返回可用的纯内存 List")
	}
	l2.Close()
	l.Close()

	if err := os.WriteFile(cfg.Path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	l3, err := Open(cfg)
	if err == nil {
		t.Fatal("损坏文件应报错")
	}
	if l3 == nil || l3.Persistent() {
		t.Fatal("损坏文件应退回纯内存模式")
	}
	hit(l3, "198.51.100.70", KindProtoDecode, 10)
	if !l3.IsBanned("198.51.100.70") {
		t.Fatal("纯内存模式下封禁仍应生效")
	}
	l3.Sweep()
	l3.Close()
	data, _ := os.ReadFile(cfg.Path)
	if string(data) != "{not json" {
		t.Fatal("损坏文件不得被覆盖")
	}
}

func TestInvalidIPRejectedByMutations(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	if _, err := l.Ban("10.0.0.0/8", time.Hour, false, "a", ""); !errors.Is(err, ErrInvalidIP) {
		t.Fatalf("Ban CIDR err=%v", err)
	}
	if err := l.Allow("host.example", time.Hour, false, "a", ""); !errors.Is(err, ErrInvalidIP) {
		t.Fatalf("Allow host err=%v", err)
	}
	if l.RecordViolation("not-an-ip", KindProtoDecode, "") {
		t.Fatal("非法 IP 不应被记录或封禁")
	}
	// mapped IPv6 与 IPv4 是同一条目
	if _, err := l.Ban("::ffff:198.51.100.90", time.Hour, false, "a", ""); err != nil {
		t.Fatal(err)
	}
	if !l.IsBanned("198.51.100.90") {
		t.Fatal("IPv4-mapped 应与 IPv4 归一")
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

func isWindows() bool { return runtime.GOOS == "windows" }

// 扫描洪峰下自动封禁条目有上限（MaxAutoBans，默认 = MaxTracked），超出淘汰最早到期的；手动封禁不受影响。
func TestAutoBanCap(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 2
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Ban("192.0.2.200", time.Hour, false, "a", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		clk.Advance(time.Second)
		hit(l, "10.1.0."+itoa(i), KindProtoDecode, 10)
	}
	auto := autoBannedIPs(l)
	if len(auto) != 2 {
		t.Fatalf("自动封禁应被限制为 MaxAutoBans（默认 = MaxTracked = 2）条，got %d", len(auto))
	}
	if !l.IsBanned("10.1.0.11") || l.IsBanned("10.1.0.0") {
		t.Fatal("应淘汰最早到期的自动封禁、保留最新的")
	}
	if !l.IsBanned("192.0.2.200") {
		t.Fatal("手动封禁不得被淘汰")
	}
}

// 已持久化条目上的新观察（客户端、连接、事件）不能只等到正常退出才落盘：
// 每分钟的 Sweep 发现脏数据就异步写盘，进程被强杀也最多丢一分钟的识别信息。
func TestSweepFlushesDirtyObservations(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.Path = filepath.Join(t.TempDir(), "blocklist.json")
	cfg.SaveInterval = 50 * time.Millisecond
	cfg.UrgentSaveInterval = 10 * time.Millisecond
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Allow("203.0.113.70", 0, true, "alice", ""); err != nil {
		t.Fatal(err)
	}
	waitFile := func(want string) bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if data, _ := os.ReadFile(cfg.Path); strings.Contains(string(data), want) {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}
	if !waitFile("203.0.113.70") {
		t.Fatal("前置：白名单应已写盘")
	}
	time.Sleep(100 * time.Millisecond) // 让 Allow 触发的那次写盘彻底完成
	l.ObserveClient("203.0.113.70", ClientInfo{Role: "share", Version: "v1.1.0", Host: "late@pc kylin arm64", ClientID: "cid"})
	if data, _ := os.ReadFile(cfg.Path); strings.Contains(string(data), "late@pc") {
		t.Fatal("前置：观察本身不应同步写盘")
	}
	l.Sweep()
	if !waitFile("late@pc kylin arm64") {
		t.Fatal("Sweep 后脏的识别信息应已写盘")
	}
}
