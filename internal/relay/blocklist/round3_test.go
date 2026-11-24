package blocklist

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustInvariants(t *testing.T, l *List, step string) {
	t.Helper()
	if err := l.CheckInvariants(); err != nil {
		t.Fatalf("%s：结构不一致：%v", step, err)
	}
}

// C1：自动封禁打满上限时，刚封的条目不得被同一次淘汰顺手从主表删掉（RecordViolation 返回 true
// 但 IsBanned 放行），也不得在堆里留下孤儿——孤儿日后被弹出时会删掉同 IP 后来新建的条目，
// 连带删掉别人的生效封禁。
func TestAutoBanCapFullNoOrphanNoLostBan(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 64
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ip := func(i int) string { return fmt.Sprintf("10.50.%d.%d", i/250, i%250) }
	// 64 个扫描器一组轮询（正好占满可淘汰 LRU）：第一组打满自动封禁上限，第二组每次封禁都要淘汰堆顶。
	const group, scanners = 64, 128
	for g := 0; g < scanners/group; g++ {
		for round := 0; round < 12; round++ {
			for i := g * group; i < (g+1)*group; i++ {
				if l.RecordViolation(ip(i), KindProtoDecode, "bad_json") && !l.IsBanned(ip(i)) {
					t.Fatalf("第 %d 组第 %d 轮 %s：RecordViolation 报告已封禁，但 IsBanned 放行", g, round, ip(i))
				}
				mustInvariants(t, l, fmt.Sprintf("第 %d 组第 %d 轮 %s", g, round, ip(i)))
			}
		}
	}
	// 对当前未被封的扫描器做 1 小时手动封禁，再让更多新扫描器把自动封禁堆挤满，
	// 期间堆里的旧条目会被弹出——手动封禁绝不能因此消失。
	var manual []string
	for i := 0; i < scanners; i++ {
		if !l.IsBanned(ip(i)) {
			if _, err := l.Ban(ip(i), time.Hour, false, "op", "manual"); err != nil {
				t.Fatal(err)
			}
			manual = append(manual, ip(i))
		}
	}
	for round := 0; round < 12; round++ {
		for i := scanners; i < scanners+group; i++ {
			l.RecordViolation(ip(i), KindProtoDecode, "bad_json")
		}
		mustInvariants(t, l, fmt.Sprintf("新扫描器第 %d 轮", round))
	}
	for _, m := range manual {
		if !l.IsBanned(m) {
			t.Fatalf("%s 的 1 小时手动封禁被自动封禁淘汰删掉了", m)
		}
	}
}

// ---------- 随机化属性测试 ----------

type propModel struct {
	manual map[string]int64 // key -> until（-1 = 永久）
	allow  map[string]int64
	grace  map[string]int64
	auto   map[string]int64
}

func newPropModel() *propModel {
	return &propModel{manual: map[string]int64{}, allow: map[string]int64{}, grace: map[string]int64{}, auto: map[string]int64{}}
}

func (m *propModel) expire(now int64) {
	for _, mp := range []map[string]int64{m.manual, m.allow, m.grace, m.auto} {
		for k, u := range mp {
			if u != -1 && u <= now {
				delete(mp, k)
			}
		}
	}
}

func propConfig(clk *fakeClock, path string) Config {
	cfg := testConfig(clk)
	cfg.Path = path
	cfg.Threshold = 6
	cfg.Window = 10 * time.Minute
	cfg.BanBase = 30 * time.Minute
	cfg.BanMax = 2 * time.Hour
	cfg.OffenseDecay = 2 * time.Hour
	cfg.UnbanGrace = 30 * time.Minute
	cfg.MaxTracked = 4
	cfg.MaxAutoBans = 2
	cfg.MaxMemory = 8
	cfg.SaveInterval = time.Millisecond
	cfg.UrgentSaveInterval = time.Millisecond
	return cfg
}

func propKeys() []string {
	var ips []string
	for i := 0; i < 40; i++ {
		ips = append(ips, fmt.Sprintf("198.51.100.%d", i+1))
	}
	for i := 0; i < 12; i++ {
		ips = append(ips, fmt.Sprintf("2001:db8:%x::1", i+1), fmt.Sprintf("2001:db8:%x::2", i+1))
	}
	// 按单地址跟踪的特殊前缀及其 /64 内的边界地址（第四轮 N1：持久化关闭重开时键必须能被重新加载）
	ips = append(ips, "64:ff9b::1:0:1", "64:ff9b::c000:201", "2001::1:2", "2001:0:abcd::9", "fe80::9", "fe80::1:2")
	return ips
}

func runPropertySeed(t *testing.T, seed int64, steps int) {
	rng := rand.New(rand.NewSource(seed))
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "blocklist.json")
	cfg := propConfig(clk, path)
	// 封禁记录一并开启（条数上限压小，让淘汰也被随机步覆盖）：CheckInvariants 每步核对
	// "每个封禁恰有一条未结束记录、每条未结束记录都对得上名单里的封禁"，重开时走完整的加载与对账。
	var recs *Records
	open := func() *List {
		r, err := OpenRecords(RecordsConfig{Path: SiblingPath(path, "records"), Now: clk.Now, MaxRecords: 50, SaveInterval: time.Millisecond})
		if err != nil {
			t.Fatalf("seed %d: OpenRecords: %v", seed, err)
		}
		recs = r
		c := cfg
		c.Records = r
		l, err := Open(c)
		if err != nil {
			t.Fatalf("seed %d: Open: %v", seed, err)
		}
		return l
	}
	l := open()
	defer func() {
		_ = l.Close()
		_ = recs.Close()
	}()
	evicted := map[string]bool{}
	bans, evictions := 0, 0
	hook := func(key string) { evicted[key] = true; evictions++ }
	l.autoBanEvictedHook = hook
	m := newPropModel()
	ips := propKeys()
	rng.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
	hot := ips[:6] // 热点键：让计分条目在小 LRU 里反复竞争，自动封禁堆频繁打满
	kinds := []Kind{KindTLSGarbage, KindTLSAborted, KindProtoDecode, KindProtoState, KindUnknownMessage, KindJoinFailed, KindJoinFailuresExceeded}
	now := func() int64 { return clk.Now().Unix() }
	until := func(d time.Duration, perm bool) int64 {
		if perm {
			return -1
		}
		return now() + int64(d/time.Second)
	}
	spans := []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 3 * time.Hour}

	for step := 0; step < steps; step++ {
		ip := ips[rng.Intn(len(ips))]
		op := rng.Intn(100)
		if op < 60 && rng.Intn(10) < 8 {
			ip = hot[rng.Intn(len(hot))] // 违规/连接/客户端集中在热点键；人工处置均匀分布
		}
		key, _ := KeyOf(ip)
		desc := ""
		switch {
		case op < 50:
			k := kinds[rng.Intn(len(kinds))]
			desc = fmt.Sprintf("violation %s %s", ip, k)
			if l.RecordViolation(ip, k, "p") {
				if !l.IsBanned(ip) {
					t.Fatalf("seed %d step %d %s：报告封禁但未生效", seed, step, desc)
				}
				d, ok := l.Detail(ip)
				if !ok || d.Ban == nil {
					t.Fatalf("seed %d step %d %s：封禁后查不到条目", seed, step, desc)
				}
				m.auto[key] = d.Ban.Until
				bans++
				delete(evicted, key)
			}
		case op < 55:
			desc = "connect " + ip
			l.ObserveConnect(ip)
		case op < 60:
			desc = "client " + ip
			l.ObserveClient(ip, ClientInfo{Role: "help", Version: "v1", Host: "h", ClientID: "c", Verified: rng.Intn(2) == 0})
		case op < 66:
			d, perm := spans[rng.Intn(len(spans))], rng.Intn(5) == 0
			desc = fmt.Sprintf("ban %s %v %v", ip, d, perm)
			if _, err := l.Ban(ip, d, perm, "op", "n"); err == nil {
				m.manual[key] = until(d, perm)
				delete(m.auto, key)
				delete(m.grace, key)
			} else if !errors.Is(err, ErrCapacity) {
				t.Fatalf("seed %d step %d %s: %v", seed, step, desc, err)
			}
		case op < 71:
			desc = "unban " + ip
			var spec *AllowSpec
			if rng.Intn(3) == 0 {
				spec = &AllowSpec{Duration: spans[rng.Intn(len(spans))]}
			}
			if err := l.Unban(ip, "op", "n", spec); err == nil {
				delete(m.manual, key)
				delete(m.auto, key)
				m.grace[key] = now() + int64(cfg.UnbanGrace/time.Second)
				if spec != nil {
					m.allow[key] = until(spec.Duration, false)
				}
			}
		case op < 76:
			d, perm, unban := spans[rng.Intn(len(spans))], rng.Intn(5) == 0, rng.Intn(2) == 0
			desc = fmt.Sprintf("allow %s unban=%v", ip, unban)
			wasBanned := l.IsBanned(ip)
			if _, err := l.AllowWithOptions(ip, AllowSpec{Duration: d, Permanent: perm}, "op", "n", unban); err == nil {
				m.allow[key] = until(d, perm)
				if unban && wasBanned {
					delete(m.manual, key)
					delete(m.auto, key)
					m.grace[key] = now() + int64(cfg.UnbanGrace/time.Second)
				}
			} else if !errors.Is(err, ErrCapacity) {
				t.Fatalf("seed %d step %d %s: %v", seed, step, desc, err)
			}
		case op < 79:
			desc = "disallow " + ip
			if err := l.Disallow(ip, "op", "n"); err == nil {
				delete(m.allow, key)
			}
		case op < 84:
			desc = "sweep"
			l.Sweep()
		case op < 98:
			d := time.Duration(1+rng.Intn(20)) * time.Second
			switch rng.Intn(20) {
			case 0:
				d = time.Duration(1+rng.Intn(4)) * time.Hour
			case 1, 2, 3:
				d = time.Duration(1+rng.Intn(15)) * time.Minute
			}
			desc = fmt.Sprintf("advance %v", d)
			clk.Advance(d)
		default:
			desc = "reopen"
			if err := l.Close(); err != nil {
				t.Fatalf("seed %d step %d Close: %v", seed, step, err)
			}
			if err := recs.Close(); err != nil {
				t.Fatalf("seed %d step %d Records.Close: %v", seed, step, err)
			}
			l = open()
			l.autoBanEvictedHook = hook
		}
		m.expire(now())
		if err := l.CheckInvariants(); err != nil {
			t.Fatalf("seed %d step %d (%s)：%v", seed, step, desc, err)
		}
		for k, u := range m.manual {
			if !l.IsBanned(k) {
				t.Fatalf("seed %d step %d (%s)：手动封禁 %s（到期 %d）丢失", seed, step, desc, k, u)
			}
		}
		for k := range m.auto {
			if !l.IsBanned(k) && !evicted[k] {
				t.Fatalf("seed %d step %d (%s)：自动封禁 %s 未到期、未被 ban_evicted 却消失", seed, step, desc, k)
			}
		}
		for k := range m.allow {
			if d, ok := l.Detail(k); !ok || d.Allow == nil {
				t.Fatalf("seed %d step %d (%s)：白名单 %s 丢失", seed, step, desc, k)
			}
		}
		for k := range m.grace {
			if _, banned := m.manual[k]; banned {
				continue
			}
			if d, ok := l.Detail(k); !ok || d.GraceUntil == 0 {
				t.Fatalf("seed %d step %d (%s)：宽限期 %s 丢失", seed, step, desc, k)
			}
		}
	}
	t.Logf("seed %d：%d 步，自动封禁 %d 次，ban_evicted %d 次", seed, steps, bans, evictions)
}

// 随机化属性测试：注入时钟，随机执行违规/连接/客户端/手动封禁解禁/加白移白/Sweep/推进时间/
// 持久化关闭重开，每步校验结构一致性与业务不变量。
//
// 默认 8 个种子（每个 4000 步）：CI 对每个包限时 120s（race 步骤 180s），40 个种子要两分多钟。
// -short 与 -race 下再减少。改动黑名单核心时用 RAT_BLOCKLIST_PROPERTY_SEEDS=40 跑满。
func TestBlocklistProperties(t *testing.T) {
	seeds, steps := 8, 4000
	switch {
	case testing.Short():
		seeds, steps = 6, 1500
	case raceEnabled:
		seeds, steps = 3, 1500
	}
	if v := os.Getenv("RAT_BLOCKLIST_PROPERTY_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("RAT_BLOCKLIST_PROPERTY_SEEDS=%q 不是正整数", v)
		}
		seeds = n
	}
	for s := 0; s < seeds; s++ {
		seed := int64(1000 + s)
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { runPropertySeed(t, seed, steps) })
	}
}

// P1：写盘预算不够时按优先级丢弃——非钉住历史与记忆先丢、自动封禁从最早到期的丢起；
// 手动封禁、白名单、宽限期全部保留，写出的文件始终在预算内且可重新加载。
func TestSaveBudgetDropsByPriority(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	cfg.SaveInterval = time.Hour
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 64 << 10
	l.saveBudget = budget
	var manual []string
	for i := 0; i < 20; i++ {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		if _, err := l.Ban(ip, 0, true, "op", strings.Repeat("备注", 50)); err != nil {
			t.Fatal(err)
		}
		manual = append(manual, ip)
	}
	if _, err := l.AllowWithOptions("192.0.2.100", AllowSpec{Permanent: true}, "op", "白名单", false); err != nil {
		t.Fatal(err)
	}
	// 400 个互不相同的地址，只用文档保留网段：前 250 个在 198.51.100.0/24，其余在 203.0.113.0/24。
	budgetIP := func(i int) string {
		if i < 250 {
			return fmt.Sprintf("198.51.100.%d", i)
		}
		return fmt.Sprintf("203.0.113.%d", i-250)
	}
	for i := 0; i < 400; i++ {
		clk.Advance(time.Second) // 后封的到期更晚
		hit(l, budgetIP(i), KindProtoDecode, 10)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if size := fileSize(t, path); size > budget {
		t.Fatalf("文件 %d 字节超过预算 %d", size, budget)
	}
	rep := l.lastSaveReport.Load()
	if rep == nil || rep.AutoDropped == 0 || rep.ManualDropped != 0 {
		t.Fatalf("预算裁剪报告不符合预期：%+v", rep)
	}
	l2, err := Open(cfg)
	if err != nil {
		t.Fatalf("预算内的文件应可加载：%v", err)
	}
	defer l2.Close()
	for _, ip := range manual {
		if !l2.IsBanned(ip) {
			t.Fatalf("手动封禁 %s 因预算被丢弃", ip)
		}
	}
	if e, ok := l2.Detail("192.0.2.100"); !ok || e.Allow == nil {
		t.Fatal("白名单因预算被丢弃")
	}
	// 保留下来的是最晚到期的自动封禁。
	if !l2.IsBanned(budgetIP(399)) || l2.IsBanned(budgetIP(0)) {
		t.Fatal("应优先保留最晚到期的自动封禁、先丢最早到期的")
	}
}
