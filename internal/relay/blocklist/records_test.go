package blocklist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 测试辅助 ----------

// openRecMem 纯内存的名单 + 封禁记录。
func openRecMem(t *testing.T, clk *fakeClock, mutate func(*Config), rc RecordsConfig) (*List, *Records) {
	t.Helper()
	rc.Now = clk.Now
	r, err := OpenRecords(rc)
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	cfg := testConfig(clk)
	cfg.Records = r
	if mutate != nil {
		mutate(&cfg)
	}
	l, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = l.Close()
		_ = r.Close()
	})
	return l, r
}

func recsFor(r *Records, key string) []Record {
	var out []Record
	for _, x := range r.Snapshot() {
		if x.Key == key {
			out = append(out, x)
		}
	}
	return out
}

func onlyRec(t *testing.T, r *Records, key string) Record {
	t.Helper()
	rs := recsFor(r, key)
	if len(rs) != 1 {
		t.Fatalf("%s 应恰有 1 条记录，实际 %d 条：%+v", key, len(rs), rs)
	}
	return rs[0]
}

func autoBan(t *testing.T, l *List, ip string) {
	t.Helper()
	if !hit(l, ip, KindJoinFailed, l.Config().Threshold) {
		t.Fatalf("%s 应被自动封禁", ip)
	}
}

// recEnv 持久化的名单 + 封禁记录，可反复关闭重开（模拟重启）。
type recEnv struct {
	t        *testing.T
	clk      *fakeClock
	listPath string
	recPath  string
	rc       RecordsConfig
	mutate   func(*Config)
	l        *List
	r        *Records
}

func newRecEnv(t *testing.T) *recEnv {
	dir := t.TempDir()
	e := &recEnv{t: t, clk: newFakeClock(), listPath: filepath.Join(dir, "blocklist.json")}
	e.recPath = SiblingPath(e.listPath, "records")
	t.Cleanup(func() {
		if e.l != nil {
			_ = e.l.Close()
		}
		if e.r != nil {
			_ = e.r.Close()
		}
	})
	return e
}

func (e *recEnv) open() {
	e.t.Helper()
	rc := e.rc
	rc.Path, rc.Now = e.recPath, e.clk.Now
	r, err := OpenRecords(rc)
	if err != nil {
		e.t.Fatalf("OpenRecords: %v", err)
	}
	cfg := testConfig(e.clk)
	cfg.Path, cfg.Records = e.listPath, r
	if e.mutate != nil {
		e.mutate(&cfg)
	}
	l, err := Open(cfg)
	if err != nil {
		_ = r.Close()
		e.t.Fatalf("Open: %v", err)
	}
	e.l, e.r = l, r
}

func (e *recEnv) close() {
	e.t.Helper()
	if err := e.l.Close(); err != nil {
		e.t.Fatalf("List.Close: %v", err)
	}
	if err := e.r.Close(); err != nil {
		e.t.Fatalf("Records.Close: %v", err)
	}
	e.l, e.r = nil, nil
}

func (e *recEnv) now() int64 { return e.clk.Now().Unix() }

// ---------- 开记录 ----------

// 自动封禁开一条记录：对象、原因、第几次、计划起止、触发分项、封禁时最近 3 个客户端（新的在前）。
func TestRecordAutoBanOpens(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	const ip = "198.51.100.7"
	for i := 0; i < 4; i++ {
		l.ObserveClient(ip, ClientInfo{Role: "help", Version: fmt.Sprintf("v1.2.%d", i), Host: fmt.Sprintf("u@h%d windows amd64", i), ClientID: fmt.Sprintf("c%d", i)})
		clk.Advance(time.Second)
	}
	since := clk.Now().Unix()
	for i := 0; i < 20; i++ {
		l.RecordViolation(ip, KindJoinFailed, "code_invalid")
	}
	rec := onlyRec(t, r, ip)
	if rec.ID <= 0 || rec.Target != TargetIP || rec.Reason != "auto" || rec.Actor != "auto" || rec.Offense != 1 {
		t.Fatalf("记录基本字段不对: %+v", rec)
	}
	if rec.Since != since || rec.Until != since+3600 || rec.Permanent {
		t.Fatalf("计划起止不对: since=%d until=%d perm=%v，want since=%d until=%d", rec.Since, rec.Until, rec.Permanent, since, since+3600)
	}
	if rec.End != "" || rec.EndedAt != 0 || rec.EndActor != "" {
		t.Fatalf("新记录不应已结束: %+v", rec)
	}
	wantTrigger := []TriggerItem{{Kind: "join_failed", Detail: "code_invalid", Count: 20, Points: 20}}
	if !reflect.DeepEqual(rec.Trigger, wantTrigger) {
		t.Fatalf("触发分项=%+v，want %+v", rec.Trigger, wantTrigger)
	}
	if rec.TriggerPartial {
		t.Fatal("20 条事件正好凑够 20 分，分项是完整的")
	}
	wantClients := []RecordClient{
		{Role: "help", Version: "v1.2.3", Host: "u@h3 windows amd64"},
		{Role: "help", Version: "v1.2.2", Host: "u@h2 windows amd64"},
		{Role: "help", Version: "v1.2.1", Host: "u@h1 windows amd64"},
	}
	if !reflect.DeepEqual(rec.Clients, wantClients) {
		t.Fatalf("客户端=%+v，want %+v", rec.Clients, wantClients)
	}
	if rec.Note == "" {
		t.Fatal("自动封禁记录应带原因说明")
	}

	// 到期后再犯：第 2 次，时长翻倍，新开一条。
	clk.Advance(2 * time.Hour)
	l.Sweep()
	since2 := clk.Now().Unix()
	autoBan(t, l, ip)
	rs := recsFor(r, ip)
	if len(rs) != 2 || rs[1].Offense != 2 || rs[1].Since != since2 || rs[1].Until != since2+7200 || rs[1].ID <= rs[0].ID {
		t.Fatalf("第二次自动封禁记录不对: %+v", rs)
	}
}

// 触发分项只统计构成这次封禁的那些事件：被加白清零的旧分值不算；按分值排序，最多 3 项。
func TestRecordTriggerSummary(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	const ip = "198.51.100.8"
	for i := 0; i < 5; i++ {
		l.RecordViolation(ip, KindTLSGarbage, "old")
	}
	if err := l.Allow(ip, time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Disallow(ip, "op", ""); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	seq := []struct {
		kind   Kind
		detail string
		n      int
	}{
		{KindProtoDecode, "bad", 2},          // 4 分
		{KindUnknownMessage, "m", 3},         // 3 分
		{KindProtoState, "s", 1},             // 2 分
		{KindTLSGarbage, "y", 1},             // 1 分
		{KindJoinFailuresExceeded, "max", 2}, // 10 分，累计 20 → 封
	}
	banned := false
	for _, s := range seq {
		for i := 0; i < s.n; i++ {
			banned = l.RecordViolation(ip, s.kind, s.detail) || banned
		}
	}
	if !banned {
		t.Fatal("累计 20 分应封禁")
	}
	want := []TriggerItem{
		{Kind: "join_failures_exceeded", Detail: "max", Count: 2, Points: 10},
		{Kind: "proto_decode", Detail: "bad", Count: 2, Points: 4},
		{Kind: "unknown_message", Detail: "m", Count: 3, Points: 3},
	}
	if got := onlyRec(t, r, ip).Trigger; !reflect.DeepEqual(got, want) {
		t.Fatalf("触发分项=%+v，want %+v", got, want)
	}
}

// ---------- 结束方式 ----------

// 到期：ended_at 取计划到期时间，而不是被惰性发现的时刻。
func TestRecordExpiredEndsAtUntil(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	const a, b = "198.51.100.9", "198.51.100.10"
	autoBan(t, l, a)
	autoBan(t, l, b)
	untilA, untilB := onlyRec(t, r, a).Until, onlyRec(t, r, b).Until
	clk.Advance(5 * time.Hour)
	l.ObserveConnect(a) // 惰性发现
	l.Sweep()           // 清扫发现
	for ip, until := range map[string]int64{a: untilA, b: untilB} {
		rec := onlyRec(t, r, ip)
		if rec.End != EndExpired || rec.EndedAt != until || rec.EndActor != "auto" {
			t.Fatalf("%s 到期记录不对: end=%q ended_at=%d end_actor=%q，want expired/%d/auto", ip, rec.End, rec.EndedAt, rec.EndActor, until)
		}
	}
}

// 人工解禁（含加白时一并解禁）：end=unban，处置人与时间是解禁那一刻的。
func TestRecordUnbanEnds(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	const a, b = "198.51.100.11", "198.51.100.12"
	autoBan(t, l, a)
	if _, err := l.Ban(b, time.Hour, false, "carol", "可疑"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Minute)
	if err := l.Unban(a, "alice", "误封", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AllowWithOptions(b, AllowSpec{Duration: time.Hour}, "bob", "", true); err != nil {
		t.Fatal(err)
	}
	now := clk.Now().Unix()
	for ip, actor := range map[string]string{a: "alice", b: "bob"} {
		rec := onlyRec(t, r, ip)
		if rec.End != EndUnban || rec.EndedAt != now || rec.EndActor != actor {
			t.Fatalf("%s 解禁记录不对: end=%q ended_at=%d end_actor=%q，want unban/%d/%s", ip, rec.End, rec.EndedAt, rec.EndActor, now, actor)
		}
	}
}

// 自动封禁超出配额：被提前解除的那条 end=evicted。
func TestRecordEvictedByCap(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, func(c *Config) { c.MaxAutoBans = 2 }, RecordsConfig{})
	ips := []string{"198.51.100.21", "198.51.100.22", "198.51.100.23"}
	for _, ip := range ips {
		autoBan(t, l, ip)
		clk.Advance(time.Minute)
	}
	now := clk.Now().Unix() - 60
	first := onlyRec(t, r, ips[0])
	if first.End != EndEvicted || first.EndedAt != now || first.EndActor != "auto" {
		t.Fatalf("最早到期的应被提前解除: %+v", first)
	}
	for _, ip := range ips[1:] {
		if rec := onlyRec(t, r, ip); rec.End != "" {
			t.Fatalf("%s 不应结束: %+v", ip, rec)
		}
	}
}

// 加载时按当前上限裁剪自动封禁：被裁掉的那条 end=evicted。
func TestRecordEvictedOnLoadTrim(t *testing.T) {
	env := newRecEnv(t)
	env.mutate = func(c *Config) { c.MaxAutoBans = 3 }
	env.open()
	ips := []string{"198.51.100.31", "198.51.100.32", "198.51.100.33"}
	for _, ip := range ips {
		autoBan(t, env.l, ip)
		env.clk.Advance(time.Minute)
	}
	env.close()
	env.mutate = func(c *Config) { c.MaxAutoBans = 2 }
	env.open()
	rec := onlyRec(t, env.r, ips[0])
	if rec.End != EndEvicted || rec.EndedAt != env.now() {
		t.Fatalf("加载时被裁掉的封禁应记 evicted: %+v", rec)
	}
	if err := env.l.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// 手动封禁覆盖已有封禁：旧记录 end=replaced，再新开一条手动记录。
func TestRecordManualBanReplaces(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	const ip = "198.51.100.40"
	autoBan(t, l, ip)
	clk.Advance(time.Minute)
	t1 := clk.Now().Unix()
	if _, err := l.Ban(ip, 2*time.Hour, false, "alice", "复核后延长"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	t2 := clk.Now().Unix()
	if _, err := l.Ban(ip, 0, true, "bob", ""); err != nil {
		t.Fatal(err)
	}
	rs := recsFor(r, ip)
	if len(rs) != 3 {
		t.Fatalf("应有 3 条记录: %+v", rs)
	}
	if rs[0].Reason != "auto" || rs[0].End != EndReplaced || rs[0].EndedAt != t1 || rs[0].EndActor != "alice" {
		t.Fatalf("自动封禁被覆盖的记录不对: %+v", rs[0])
	}
	m := rs[1]
	if m.Reason != "manual" || m.Actor != "alice" || m.Note != "复核后延长" || m.Since != t1 || m.Until != t1+7200 || m.Permanent || m.Offense != 0 || m.Trigger != nil {
		t.Fatalf("手动封禁记录不对: %+v", m)
	}
	if m.End != EndReplaced || m.EndedAt != t2 || m.EndActor != "bob" {
		t.Fatalf("手动封禁被覆盖的记录不对: %+v", m)
	}
	if p := rs[2]; p.Reason != "manual" || p.Actor != "bob" || !p.Permanent || p.Until != 0 || p.Since != t2 || p.End != "" {
		t.Fatalf("永久封禁记录不对: %+v", p)
	}
}

// 没有封禁时的操作（白名单、移白、未封禁时解禁）不产生记录。
func TestRecordNoBanNoRecord(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	const ip = "198.51.100.41"
	l.ObserveConnect(ip)
	hit(l, ip, KindTLSGarbage, 5)
	_ = l.Allow(ip, time.Hour, false, "op", "")
	_ = l.Disallow(ip, "op", "")
	_ = l.Unban(ip, "op", "", nil)
	if n := len(r.Snapshot()); n != 0 {
		t.Fatalf("没有封禁不应有记录，实际 %d 条", n)
	}
}

// ---------- 持久化与对账 ----------

// 重启后记录原样还在，id 继续递增。
func TestRecordsPersistAcrossRestart(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	autoBan(t, env.l, "198.51.100.50")
	if _, err := env.l.Ban("198.51.100.51", time.Hour, false, "alice", "备注"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.l.Ban("2001:db8:1::5", 0, true, "bob", ""); err != nil {
		t.Fatal(err)
	}
	env.clk.Advance(time.Minute)
	if err := env.l.Unban("2001:db8:1::5", "bob", "", nil); err != nil {
		t.Fatal(err)
	}
	before := env.r.Snapshot()
	if len(before) != 3 {
		t.Fatalf("应有 3 条记录: %+v", before)
	}
	env.close()

	env.open()
	after := env.r.Snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("重启后记录不一致:\nbefore=%+v\nafter =%+v", before, after)
	}
	if _, err := env.l.Ban("198.51.100.52", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if got := onlyRec(t, env.r, "198.51.100.52").ID; got != before[2].ID+1 {
		t.Fatalf("重启后 id 应继续递增：got %d want %d", got, before[2].ID+1)
	}
}

// 对账：记录显示生效中、名单里却没有对应封禁——计划已过的按到期补记（ended_at=until），
// 否则记 lost（ended_at=加载时刻）。
func TestRecordsReconcileLostAndExpired(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	const long, short = "198.51.100.60", "198.51.100.61"
	if _, err := env.l.Ban(long, 10*time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := env.l.Ban(short, time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	shortUntil := onlyRec(t, env.r, short).Until
	env.close()
	if err := os.Remove(env.listPath); err != nil { // 名单文件丢了这些封禁
		t.Fatal(err)
	}
	env.clk.Advance(2 * time.Hour)
	env.open()
	if rec := onlyRec(t, env.r, long); rec.End != EndLost || rec.EndedAt != env.now() {
		t.Fatalf("名单里没有的生效封禁应记 lost@加载时刻: %+v", rec)
	}
	if rec := onlyRec(t, env.r, short); rec.End != EndExpired || rec.EndedAt != shortUntil {
		t.Fatalf("计划已过的应按到期补记: %+v", rec)
	}
	if err := env.l.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// 对账的另一面：名单里生效的封禁没有对应记录（例如首次升级、记录文件丢失），补开一条。
func TestRecordsReconcileBackfillsActiveBans(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	const manual, auto = "198.51.100.70", "198.51.100.71"
	if _, err := env.l.Ban(manual, 0, true, "alice", "长期封禁"); err != nil {
		t.Fatal(err)
	}
	autoBan(t, env.l, auto)
	env.close()
	if err := os.Remove(env.recPath); err != nil {
		t.Fatal(err)
	}
	env.open()
	m := onlyRec(t, env.r, manual)
	d := entryOf(t, env.l, manual)
	if m.End != "" || m.Reason != "manual" || m.Actor != "alice" || m.Note != "长期封禁" || !m.Permanent || m.Since != d.Ban.Since {
		t.Fatalf("补开的手动封禁记录不对: %+v", m)
	}
	a := onlyRec(t, env.r, auto)
	da := entryOf(t, env.l, auto)
	if a.End != "" || a.Reason != "auto" || a.Offense != 1 || a.Since != da.Ban.Since || a.Until != da.Ban.Until {
		t.Fatalf("补开的自动封禁记录不对: %+v", a)
	}
	// 名单文件里自动封禁只留最近 1 条事件，据此汇总的分项会失真，补开时不带。
	if a.Trigger != nil || a.Note == "" {
		t.Fatalf("补开的自动封禁不应带触发分项、应带封禁说明: %+v", a)
	}
	if err := env.l.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// ---------- 保留与预算 ----------

// 条数上限只约束已结束的记录：超出先丢最旧的，生效中的（哪怕最旧）不丢、也不占名额。
func TestRecordsRetentionByCount(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{MaxRecords: 5})
	if _, err := l.Ban("198.51.100.80", 0, true, "op", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		if _, err := l.Ban(ip, time.Hour, false, "op", ""); err != nil {
			t.Fatal(err)
		}
		if err := l.Unban(ip, "op", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	rs := r.Snapshot()
	if len(rs) != 6 {
		t.Fatalf("应保留 1 条生效 + 最新 5 条已结束，实际 %d 条", len(rs))
	}
	if rs[0].Key != "198.51.100.80" || rs[0].End != "" {
		t.Fatalf("生效中的最旧记录不应被丢: %+v", rs[0])
	}
	for i, x := range rs[1:] {
		if want := fmt.Sprintf("192.0.2.%d", i+4); x.Key != want || x.End != EndUnban {
			t.Fatalf("应保留最新的已结束记录：第 %d 条 %s，want %s", i+1, x.Key, want)
		}
	}
}

// 天数上限：结束超过保留期的记录被丢；生效中的永久封禁再久也保留。
func TestRecordsRetentionByAge(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	if _, err := env.l.Ban("198.51.100.81", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.l.Unban("198.51.100.81", "op", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.l.Ban("198.51.100.82", 0, true, "op", ""); err != nil {
		t.Fatal(err)
	}
	env.clk.Advance(89 * 24 * time.Hour)
	if _, err := env.l.Ban("198.51.100.83", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.l.Unban("198.51.100.83", "op", "", nil); err != nil {
		t.Fatal(err)
	}
	env.close()
	env.clk.Advance(2 * 24 * time.Hour) // 第一条结束已 91 天，第三条 2 天
	env.open()
	keys := map[string]bool{}
	for _, x := range env.r.Snapshot() {
		keys[x.Key] = true
	}
	if keys["198.51.100.81"] || !keys["198.51.100.82"] || !keys["198.51.100.83"] {
		t.Fatalf("按天数淘汰不对: %v", keys)
	}
	// 没有新记录也按天数淘汰：写盘时裁剪。
	env.clk.Advance(91 * 24 * time.Hour)
	env.close()
	raw, err := os.ReadFile(env.recPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "198.51.100.83") || !strings.Contains(string(raw), "198.51.100.82") {
		t.Fatalf("写盘时应按天数淘汰: %s", raw)
	}
}

// 字节预算：已结束的记录从最新的往前装、不超预算，丢的是最旧的；生效中的全部写入，不占已结束的预算。
func TestRecordsByteBudget(t *testing.T) {
	env := newRecEnv(t)
	const budget = 8 << 10
	env.rc.saveBudget = budget
	env.open()
	open := []string{"198.51.100.90", "198.51.100.91", "198.51.100.92"}
	for _, ip := range open {
		if _, err := env.l.Ban(ip, 0, true, "op", strings.Repeat("长", 50)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i+1)
		if _, err := env.l.Ban(ip, time.Hour, false, "op", strings.Repeat("备", 30)); err != nil {
			t.Fatal(err)
		}
		if err := env.l.Unban(ip, "op", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	env.close()
	info, err := os.Stat(env.recPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(env.recPath)
	if err != nil {
		t.Fatal(err)
	}
	var fd recordsFile
	if err := json.Unmarshal(raw, &fd); err != nil {
		t.Fatal(err)
	}
	endedBytes := 0
	for _, x := range fd.Records {
		if x.End != "" {
			b, _ := json.Marshal(x)
			endedBytes += len(b) + 1
		}
	}
	if endedBytes > budget || endedBytes < budget/2 {
		t.Fatalf("已结束记录占 %d 字节，应在预算 %d 内且装得比较满（info=%d）", endedBytes, budget, info.Size())
	}
	for i := 1; i < len(fd.Records); i++ {
		if fd.Records[i].ID <= fd.Records[i-1].ID {
			t.Fatalf("文件里的记录应按 id 升序: %d 之后是 %d", fd.Records[i-1].ID, fd.Records[i].ID)
		}
	}
	env.open()
	rs := env.r.Snapshot()
	var ended []Record
	got := map[string]bool{}
	for _, x := range rs {
		got[x.Key] = x.End == ""
		if x.End != "" {
			ended = append(ended, x)
		}
	}
	for _, ip := range open {
		if !got[ip] {
			t.Fatalf("生效中的 %s 不应因预算被丢", ip)
		}
	}
	if len(ended) == 0 || len(ended) >= 100 {
		t.Fatalf("预算内应保留部分已结束记录，实际 %d 条", len(ended))
	}
	if last := ended[len(ended)-1].Key; last != fmt.Sprintf("203.0.113.%d", 100) {
		t.Fatalf("应保留最新的已结束记录，最后一条是 %s", last)
	}
	for i := 1; i < len(ended); i++ {
		if ended[i].ID != ended[i-1].ID+1 {
			t.Fatalf("保留的已结束记录应是最新的连续一段: %d 之后是 %d", ended[i-1].ID, ended[i].ID)
		}
	}
}

// 硬上限：生效中的记录多到放不下时丢最旧的，保证文件永远能被重新加载；重启对账时按名单补回。
func TestRecordsHardBudgetBackfilledOnRestart(t *testing.T) {
	env := newRecEnv(t)
	env.rc.saveBudget, env.rc.hardBudget = 1<<10, 2<<10
	env.open()
	var ips []string
	for i := 0; i < 30; i++ {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		ips = append(ips, ip)
		if _, err := env.l.Ban(ip, 0, true, "op", ""); err != nil {
			t.Fatal(err)
		}
	}
	env.close()
	if info, err := os.Stat(env.recPath); err != nil || info.Size() > 2<<10 {
		t.Fatalf("记录文件应在硬上限内: %v %v", info, err)
	}
	env.open()
	for _, ip := range ips {
		if rec := onlyRec(t, env.r, ip); rec.End != "" || !rec.Permanent {
			t.Fatalf("%s 的生效记录应在重启后补回: %+v", ip, rec)
		}
	}
	if err := env.l.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// 写盘预算只约束已结束的记录：生效中的记录超过预算也全部写进文件（只受硬上限约束）。
func TestRecordsOpenNotLimitedBySaveBudget(t *testing.T) {
	env := newRecEnv(t)
	env.rc.saveBudget = 1 << 10
	env.open()
	// 先有 3 条已结束的审计记录；随后大量封禁生效，把生效记录撑过已结束预算——已结束的不能因此被挤出文件。
	for i := 0; i < 3; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i+1)
		if _, err := env.l.Ban(ip, time.Hour, false, "op", ""); err != nil {
			t.Fatal(err)
		}
		if err := env.l.Unban(ip, "op", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		if _, err := env.l.Ban(fmt.Sprintf("192.0.2.%d", i+1), 0, true, "op", ""); err != nil {
			t.Fatal(err)
		}
	}
	env.close()
	raw, err := os.ReadFile(env.recPath)
	if err != nil {
		t.Fatal(err)
	}
	var fd recordsFile
	if err := json.Unmarshal(raw, &fd); err != nil {
		t.Fatal(err)
	}
	active, ended := 0, 0
	for _, x := range fd.Records {
		if x.End == "" {
			active++
		} else {
			ended++
		}
	}
	if active != 30 || ended != 3 || len(raw) <= 1<<10 {
		t.Fatalf("30 条生效 + 3 条已结束应全部写入（%d 字节），实际生效 %d 已结束 %d", len(raw), active, ended)
	}
}

// 条数上限不把生效记录算进去：大规模封禁时生效记录再多，也不会把已结束的审计历史挤光。
func TestRecordsActiveDoNotEvictEnded(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{MaxRecords: 5})
	for i := 0; i < 8; i++ { // 生效中的已经超过 MaxRecords
		if _, err := l.Ban(fmt.Sprintf("192.0.2.%d", i+1), 0, true, "op", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 7; i++ { // 已结束的超过 MaxRecords，触发裁剪
		ip := fmt.Sprintf("203.0.113.%d", i+1)
		if _, err := l.Ban(ip, time.Hour, false, "op", ""); err != nil {
			t.Fatal(err)
		}
		if err := l.Unban(ip, "op", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	active, ended := 0, 0
	var oldest string
	for _, x := range r.Snapshot() {
		if x.End == "" {
			active++
			continue
		}
		if ended++; oldest == "" {
			oldest = x.Key
		}
	}
	if active != 8 || ended != 5 || oldest != "203.0.113.3" {
		t.Fatalf("生效记录不占 MaxRecords 名额：应剩 8 条生效、最新的 5 条已结束（从 .3 起）：生效 %d 已结束 %d 最旧 %s", active, ended, oldest)
	}
}

// 超出上限不逐条裁剪（裁剪是 O(n)，而开记录时持有名单的写锁）：留 1/8 的余量，写盘与加载时再精确裁剪。
func TestRecordsTrimIsBatched(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "records.json")
	r, err := OpenRecords(RecordsConfig{Path: path, Now: clk.Now, MaxRecords: 80})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() }) // 中途失败也要释放文件锁，临时目录才删得掉
	since := clk.Now().Unix()
	for i := 1; i <= 200; i++ {
		key := fmt.Sprintf("192.0.2.%d", i)
		r.start(Record{Target: TargetIP, Key: key, Reason: "manual", Actor: "op", Since: since, Permanent: true})
		r.end(TargetIP, key, banRef{since: since, permanent: true}, EndUnban, since+1, "op")
		n := len(r.Snapshot())
		if n > 90 {
			t.Fatalf("第 %d 条之后内存里有 %d 条，应不超过上限 80 加余量 10", i, n)
		}
		if i == 85 && n != 85 {
			t.Fatalf("85 条没超出余量，不应裁剪：实际 %d 条", n)
		}
	}
	if err := r.Close(); err != nil { // 写盘时精确裁剪
		t.Fatal(err)
	}
	r2, err := OpenRecords(RecordsConfig{Path: path, Now: clk.Now, MaxRecords: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	rs := r2.Snapshot()
	if len(rs) != 80 || rs[0].Key != "192.0.2.121" || rs[79].Key != "192.0.2.200" {
		t.Fatalf("写盘后应精确保留最新的 80 条：%d 条", len(rs))
	}
}

// 文件内容不可信：id 或 next_id 接近 int64 上限时，新 id 会溢出成负数，打乱按 id 的二分查找，记录从此结束不了。
func TestRecordsHugeIDsIgnored(t *testing.T) {
	env := newRecEnv(t)
	rec := func(id string, key string) string {
		return fmt.Sprintf(`{"id":%s,"target":"ip","key":%q,"reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`, id, key)
	}
	content := fmt.Sprintf(`{"schema":1,"next_id":9223372036854775807,"saved_at":0,"records":[%s,%s]}`,
		rec("5", "198.51.100.130"), rec("9223372036854775807", "198.51.100.131"))
	if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if rs := r.Snapshot(); len(rs) != 1 || rs[0].ID != 5 {
		t.Fatalf("id 越界的记录应被丢弃: %+v", rs)
	}
	s := env.now()
	r.start(Record{Target: TargetIP, Key: "198.51.100.132", Reason: "manual", Actor: "op", Since: s, Permanent: true})
	r.start(Record{Target: TargetIP, Key: "198.51.100.133", Reason: "manual", Actor: "op", Since: s, Permanent: true})
	r.end(TargetIP, "198.51.100.133", banRef{since: s, permanent: true}, EndUnban, s, "op")
	a, b := onlyRec(t, r, "198.51.100.132"), onlyRec(t, r, "198.51.100.133")
	if a.ID != 6 || b.ID != 7 || b.End != EndUnban {
		t.Fatalf("越界的 next_id 应被忽略，新记录接着现有最大 id，且能正常结束: %+v %+v", a, b)
	}
}

// 对账按 since 加计划时长关联：同一秒内自动封禁被手动永久封禁覆盖、崩溃后名单文件停在覆盖之前，
// since 相同但不是同一次封禁，永久记录不能被当成「自动封禁到期」。
func TestRecordsReconcileMatchesWholePlan(t *testing.T) {
	clk := newFakeClock()
	r, err := OpenRecords(RecordsConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	const a, b, c = "198.51.100.134", "198.51.100.135", "198.51.100.141"
	r.start(Record{Target: TargetIP, Key: a, Reason: "manual", Actor: "op", Since: 100, Permanent: true})
	r.start(Record{Target: TargetIP, Key: b, Reason: "manual", Actor: "op", Since: 100, Until: 9000})
	r.start(Record{Target: TargetIP, Key: c, Reason: "manual", Actor: "op", Since: 100, Until: 9000})
	missing := r.reconcile(TargetIP, map[string]banRef{
		a: {since: 100, until: 3700},                  // 同一秒开始的 1 小时自动封禁
		b: {since: 100, until: 9000, permanent: true}, // 同一秒开始、计划时长不同的永久封禁
		c: {since: 100, until: 3700},                  // 同样不是永久，只是截止时间不同
	}, 200)
	if !reflect.DeepEqual(missing, []string{a, b, c}) {
		t.Fatalf("since 相同但计划不同的封禁应各自补开: %v", missing)
	}
	for _, k := range []string{a, b, c} {
		if rec := onlyRec(t, r, k); rec.End != EndLost {
			t.Fatalf("%s 的旧记录应记 lost，而不是按到期处理: %+v", k, rec)
		}
	}
	// 计划完全一致才算同一次封禁。
	r.start(Record{Target: TargetIP, Key: a, Reason: "manual", Actor: "op", Since: 300, Until: 9000})
	if missing := r.reconcile(TargetIP, map[string]banRef{a: {since: 300, until: 9000}}, 400); len(missing) != 0 {
		t.Fatalf("计划完全一致的封禁不应补开: %v", missing)
	}
}

// 名单文件被篡改出的非法封禁开不出合法记录：不补开，否则每次重启都丢一条、补一条，id 不断变化。
func TestRecordsBackfillSkipsInvalidBans(t *testing.T) {
	for name, tamper := range map[string]func(ban map[string]any){
		"原因非法":     func(ban map[string]any) { ban["reason"] = "weird" },
		"since 非正": func(ban map[string]any) { ban["since"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			env := newRecEnv(t)
			env.open()
			const ip = "198.51.100.136"
			if _, err := env.l.Ban(ip, 0, true, "op", ""); err != nil {
				t.Fatal(err)
			}
			env.close()
			raw, err := os.ReadFile(env.listPath)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			for _, e := range doc["entries"].([]any) {
				if m := e.(map[string]any); m["ip"] == ip {
					tamper(m["ban"].(map[string]any))
				}
			}
			raw, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(env.listPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(env.recPath); err != nil {
				t.Fatal(err)
			}
			env.open()
			if e := entryOf(t, env.l, ip); e.Ban == nil {
				t.Fatal("测试前提：被篡改的封禁应仍在名单里")
			}
			if rs := env.r.Snapshot(); len(rs) != 0 {
				t.Fatalf("非法封禁不应补开记录: %+v", rs)
			}
		})
	}
}

// Snapshot 的副本不与内部记录共享切片：调用方改动不能影响内部记录（否则与锁外编码的写盘有数据竞争）。
func TestRecordsSnapshotIsDeepCopy(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	const ip = "198.51.100.137"
	l.ObserveClient(ip, ClientInfo{Role: "help", Version: "v1", Host: "host-a", ClientID: "c1"})
	autoBan(t, l, ip)
	snap := r.Snapshot()
	if len(snap) != 1 || len(snap[0].Trigger) == 0 || len(snap[0].Clients) == 0 {
		t.Fatalf("测试前提：记录应带触发分项与客户端: %+v", snap)
	}
	snap[0].Trigger[0].Kind = "tampered"
	snap[0].Clients[0].Host = "tampered"
	again := r.Snapshot()[0]
	if again.Trigger[0].Kind == "tampered" || again.Clients[0].Host == "tampered" {
		t.Fatalf("改动 Snapshot 副本影响了内部记录: %+v", again)
	}
}

// 记录文件超过加载上限：拒绝加载、转纯内存、不覆盖原文件。
func TestRecordsFileTooLarge(t *testing.T) {
	env := newRecEnv(t)
	// 内容本身是合法的空记录文件，只是尾部空白把它撑过了上限：不拒绝大小的话它能照常加载。
	head := `{"schema":1,"next_id":1,"saved_at":0,"records":[]}`
	content := append([]byte(head), bytes.Repeat([]byte(" "), maxRecordsFileBytes+1-len(head))...)
	if err := os.WriteFile(env.recPath, content, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
	if err == nil || r == nil || r.Persistent() {
		t.Fatalf("过大的记录文件应报错并转纯内存: %v %v", err, r)
	}
	r.start(Record{Target: TargetIP, Key: "198.51.100.138", Reason: "manual", Actor: "op", Since: env.now(), Permanent: true})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(env.recPath); err != nil || info.Size() != maxRecordsFileBytes+1 {
		t.Fatalf("原文件不应被覆盖: %v %v", info, err)
	}
}

// 事件只留最近 20 条：阈值调高后，分项凑不够封禁时的累计分值，要标出只是部分构成。
func TestRecordTriggerPartial(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, func(c *Config) { c.Threshold = 40 }, RecordsConfig{})
	const ip = "198.51.100.139"
	if !hit(l, ip, KindJoinFailed, 40) {
		t.Fatal("累计 40 分应封禁")
	}
	rec := onlyRec(t, r, ip)
	want := []TriggerItem{{Kind: "join_failed", Detail: "test", Count: 20, Points: 20}}
	if !reflect.DeepEqual(rec.Trigger, want) || !rec.TriggerPartial {
		t.Fatalf("事件被截断时应标记分项不完整：%+v partial=%v，want %+v partial=true", rec.Trigger, rec.TriggerPartial, want)
	}
}

// 同键重复开记录（名单文件加载失败、名单退回纯内存时合理）用记录自己的一次性日志标志，不占名单不变量的那一个。
func TestRecordsOverlapDoesNotConsumeInvariantFlag(t *testing.T) {
	oldInv, oldRec := invariantLogged.Load(), recordsOverlapLogged.Load()
	t.Cleanup(func() { invariantLogged.Store(oldInv); recordsOverlapLogged.Store(oldRec) })
	invariantLogged.Store(false)
	recordsOverlapLogged.Store(false)
	clk := newFakeClock()
	r, err := OpenRecords(RecordsConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	const ip = "198.51.100.140"
	r.start(Record{Target: TargetIP, Key: ip, Reason: "manual", Actor: "op", Since: 100, Permanent: true})
	r.start(Record{Target: TargetIP, Key: ip, Reason: "manual", Actor: "op", Since: 200, Permanent: true})
	if invariantLogged.Load() {
		t.Fatal("记录的重叠不应消耗名单不变量的一次性日志标志")
	}
	if !recordsOverlapLogged.Load() {
		t.Fatal("重叠应以自己的标志记一次日志")
	}
}

// 名单文件加载失败（纯内存）时不对账：记录里仍生效的封禁保持原样，修好名单文件重启后还能对上。
func TestRecordsNotReconciledWhenListFailsToLoad(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	const ip = "198.51.100.110"
	if _, err := env.l.Ban(ip, 0, true, "op", ""); err != nil {
		t.Fatal(err)
	}
	env.close()
	if err := os.WriteFile(env.listPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(env.clk)
	cfg.Path, cfg.Records = env.listPath, r
	l, err := Open(cfg)
	if err == nil {
		t.Fatal("损坏的名单文件应报错")
	}
	env.l, env.r = l, r // 交给 Cleanup 关闭
	if rec := onlyRec(t, r, ip); rec.End != "" {
		t.Fatalf("名单没加载成功时不应结束记录: %+v", rec)
	}
}

// 对账只管本名单的对象类型：IP 名单不动设备记录。
func TestRecordsReconcileIgnoresOtherTargets(t *testing.T) {
	env := newRecEnv(t)
	dev := "dev-" + strings.Repeat("ab", 12)
	content := fmt.Sprintf(`{"schema":1,"next_id":2,"saved_at":0,"records":[{"id":1,"target":"device","key":%q,"reason":"auto","actor":"auto","since":1790000000,"until":1990000000,"permanent":false}]}`, dev)
	if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	env.open()
	if rec := onlyRec(t, env.r, dev); rec.End != "" || rec.Target != TargetDevice {
		t.Fatalf("IP 名单的对账不应结束设备记录: %+v", rec)
	}
}

// 文件里同一对象有多条未结束记录（只可能来自篡改或旧 bug）：只认最新一条，其余按 lost 结束。
func TestRecordsLoadDuplicateOpen(t *testing.T) {
	env := newRecEnv(t)
	const ip = "198.51.100.111"
	rec := func(id, since int64) string {
		return fmt.Sprintf(`{"id":%d,"target":"ip","key":%q,"reason":"manual","actor":"op","since":%d,"until":0,"permanent":true}`, id, ip, since)
	}
	// next_id 比文件里最大的 id 还小（篡改或旧 bug）：新 id 仍接着最大的 id 往后。
	content := fmt.Sprintf(`{"schema":1,"next_id":1,"saved_at":0,"records":[%s,%s]}`, rec(1, 1790000000), rec(2, 1790000100))
	if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rs := recsFor(r, ip)
	if len(rs) != 2 || rs[0].End != EndLost || rs[0].EndedAt != env.now() || rs[1].End != "" {
		t.Fatalf("重复的未结束记录应只留最新一条: %+v", rs)
	}
	const other = "198.51.100.121"
	r.start(Record{Target: TargetIP, Key: other, Reason: "manual", Actor: "op", Since: env.now(), Permanent: true})
	if id := onlyRec(t, r, other).ID; id != 3 {
		t.Fatalf("新 id 应接着文件里最大的 id，got %d", id)
	}
}

// 停机期间已到期的封禁不补开记录：先清扫、再对账。
func TestRecordsBackfillSkipsBansExpiredWhileDown(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	const auto, manual = "198.51.100.112", "198.51.100.113"
	autoBan(t, env.l, auto)
	if _, err := env.l.Ban(manual, 0, true, "op", ""); err != nil {
		t.Fatal(err)
	}
	env.close()
	if err := os.Remove(env.recPath); err != nil {
		t.Fatal(err)
	}
	env.clk.Advance(2 * time.Hour)
	env.open()
	if rs := recsFor(env.r, auto); len(rs) != 0 {
		t.Fatalf("停机期间已到期的封禁不应补开记录: %+v", rs)
	}
	if rec := onlyRec(t, env.r, manual); rec.End != "" {
		t.Fatalf("仍生效的封禁应补开记录: %+v", rec)
	}
}

// 对账按 since 关联：记录文件比名单旧（同一对象后来解禁又被封）时，旧记录记 lost，新封禁补开一条。
func TestRecordsReconcileSinceMismatch(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	const ip = "198.51.100.114"
	if _, err := env.l.Ban(ip, 0, true, "op", "第一次"); err != nil {
		t.Fatal(err)
	}
	s1 := onlyRec(t, env.r, ip).Since
	env.close()
	stale, err := os.ReadFile(env.recPath)
	if err != nil {
		t.Fatal(err)
	}
	env.open()
	env.clk.Advance(time.Minute)
	if err := env.l.Unban(ip, "op", "", nil); err != nil {
		t.Fatal(err)
	}
	env.clk.Advance(time.Minute)
	if _, err := env.l.Ban(ip, 0, true, "op", "第二次"); err != nil {
		t.Fatal(err)
	}
	s2 := env.now()
	env.close()
	if err := os.WriteFile(env.recPath, stale, 0600); err != nil { // 记录文件停在第一次封禁时
		t.Fatal(err)
	}
	env.clk.Advance(time.Minute)
	env.open()
	rs := recsFor(env.r, ip)
	if len(rs) != 2 || rs[0].Since != s1 || rs[0].End != EndLost || rs[0].EndedAt != env.now() ||
		rs[1].Since != s2 || rs[1].End != "" || rs[1].Note != "第二次" {
		t.Fatalf("since 对不上的旧记录应记 lost、新封禁应补开: %+v", rs)
	}
	if err := env.l.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// 纯内存名单（没有任何封禁）也对账：记录里仍显示生效的封禁记 lost。
func TestRecordsReconcileMemoryList(t *testing.T) {
	env := newRecEnv(t)
	const ip = "198.51.100.115"
	content := fmt.Sprintf(`{"schema":1,"next_id":2,"saved_at":0,"records":[{"id":1,"target":"ip","key":%q,"reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}]}`, ip)
	if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(env.clk)
	cfg.Records = r
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env.l, env.r = l, r
	if rec := onlyRec(t, r, ip); rec.End != EndLost || rec.EndedAt != env.now() {
		t.Fatalf("纯内存名单里没有的封禁应记 lost: %+v", rec)
	}
}

// Records 自身的防御：计划（since、截止时间、永久与否）对不上的结束请求不动记录；同键重复开记录时旧的记 lost；结束时间不早于开始时间。
func TestRecordsStartEndGuards(t *testing.T) {
	clk := newFakeClock()
	r, err := OpenRecords(RecordsConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	const a, b = "198.51.100.116", "198.51.100.117"
	r.start(Record{Target: TargetIP, Key: a, Reason: "manual", Actor: "op", Since: 100, Until: 50})
	for _, ref := range []banRef{
		{since: 99, until: 50},                   // since 不同
		{since: 100, until: 51},                  // 截止时间不同
		{since: 100, until: 50, permanent: true}, // 永久与否不同
	} {
		r.end(TargetIP, a, ref, EndUnban, 120, "op")
		if rec := onlyRec(t, r, a); rec.End != "" {
			t.Fatalf("计划对不上（%+v）的结束请求不应结束记录: %+v", ref, rec)
		}
	}
	r.reconcile(TargetIP, nil, 300)
	if rec := onlyRec(t, r, a); rec.End != EndExpired || rec.EndedAt != 100 {
		t.Fatalf("到期时间早于开始时间时，结束时间应取开始时间: %+v", rec)
	}
	r.start(Record{Target: TargetIP, Key: b, Reason: "manual", Actor: "op", Since: 400, Permanent: true})
	r.start(Record{Target: TargetIP, Key: b, Reason: "manual", Actor: "op", Since: 500, Permanent: true})
	rs := recsFor(r, b)
	if len(rs) != 2 || rs[0].End != EndLost || rs[0].EndedAt != 500 || rs[1].End != "" || rs[1].ID <= rs[0].ID {
		t.Fatalf("同键重复开记录时旧的应记 lost: %+v", rs)
	}
}

// 已过保留期的记录不占条数名额：超出条数上限时，过期的先丢，剩下的没超就不再丢未过期的。
func TestRecordsRetentionAgeCountsTowardCap(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{MaxRecords: 3})
	banUnban := func(ip string) {
		t.Helper()
		if _, err := l.Ban(ip, time.Hour, false, "op", ""); err != nil {
			t.Fatal(err)
		}
		if err := l.Unban(ip, "op", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	ipN := func(n int) string { return fmt.Sprintf("192.0.2.%d", n) }
	banUnban(ipN(1))
	clk.Advance(91 * 24 * time.Hour)
	for i := 2; i <= 4; i++ {
		banUnban(ipN(i))
	}
	if _, err := l.Ban(ipN(5), time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, x := range r.Snapshot() {
		keys = append(keys, x.Key)
	}
	want := []string{ipN(2), ipN(3), ipN(4), ipN(5)}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("保留的记录=%v，want %v", keys, want)
	}
}

// 不关闭也会按间隔写盘（崩溃时最多丢一个间隔的记录）。
func TestRecordsSavedWithoutClose(t *testing.T) {
	env := newRecEnv(t)
	env.rc.SaveInterval = time.Millisecond
	// Windows 上轮询读文件会和写盘的 rename 撞车（Access is denied），重试间隔缩短，别让测试白等 5 秒退避。
	env.rc.retryBackoff = []time.Duration{10 * time.Millisecond}
	env.open()
	const ip = "198.51.100.118"
	waitFor := func(substr string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if raw, err := os.ReadFile(env.recPath); err == nil && strings.Contains(string(raw), substr) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("记录文件 5 秒内没写入 %s", substr)
	}
	if _, err := env.l.Ban(ip, time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(ip)
	if err := env.l.Unban(ip, "op", "", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(`"end":"unban"`)
}

// 关闭时做最后一次写盘：写盘间隔内积压的变更不丢。间隔设成一小时，第一次写盘之后的变更只能靠 Close 带走。
func TestRecordsCloseFlushesPending(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "records.json")
	r, err := OpenRecords(RecordsConfig{Path: path, Now: clk.Now, SaveInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	const a, b = "198.51.100.122", "198.51.100.123"
	s := clk.Now().Unix()
	r.start(Record{Target: TargetIP, Key: a, Reason: "manual", Actor: "op", Since: s, Permanent: true})
	deadline := time.Now().Add(5 * time.Second)
	for r.saveCount.Load() < 1 { // saver 的第一次写盘不受间隔限制
		if time.Now().After(deadline) {
			t.Fatal("第一次写盘 5 秒内没发生")
		}
		time.Sleep(time.Millisecond)
	}
	r.start(Record{Target: TargetIP, Key: b, Reason: "manual", Actor: "op", Since: s, Permanent: true})
	r.end(TargetIP, a, banRef{since: s, permanent: true}, EndUnban, s+10, "op")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := OpenRecords(RecordsConfig{Path: path, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if ra, rb := onlyRec(t, r2, a), onlyRec(t, r2, b); ra.End != EndUnban || rb.End != "" {
		t.Fatalf("Close 应带走间隔内积压的变更: %+v %+v", ra, rb)
	}
}

// 已结束的记录从新到旧装，遇到第一条装不下的就停：不会跳过大的、把更旧更小的塞进去，
// 否则文件里的已结束记录会出现空洞。
func TestEncodeRecordsStopsAtFirstOverflow(t *testing.T) {
	mk := func(id int64, note string) *Record {
		return &Record{ID: id, Target: TargetIP, Key: "198.51.100.1", Reason: "manual", Actor: "op", Note: note,
			Since: 10, Permanent: true, EndedAt: 20, End: EndUnban, EndActor: "op"}
	}
	small, big := mk(1, "s"), mk(2, strings.Repeat("b", 400))
	data, openDropped, endedDropped, err := encodeRecords(3, 30, []*Record{small, big}, 300, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var fd recordsFile
	if err := json.Unmarshal(data, &fd); err != nil {
		t.Fatal(err)
	}
	if len(fd.Records) != 0 || openDropped != 0 || endedDropped != 2 {
		t.Fatalf("装不下最新的一条后应停止：写入 %d 条，丢弃 生效 %d 已结束 %d", len(fd.Records), openDropped, endedDropped)
	}
	// 预算够装下大的：两条都写入，按 id 升序。
	data, _, endedDropped, err = encodeRecords(3, 30, []*Record{small, big}, 1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	fd = recordsFile{}
	if err := json.Unmarshal(data, &fd); err != nil {
		t.Fatal(err)
	}
	if len(fd.Records) != 2 || endedDropped != 0 || fd.Records[0].ID != 1 || fd.Records[1].ID != 2 {
		t.Fatalf("预算充足时应全部写入: %+v", fd.Records)
	}
}

// 生效与已结束两类记录各有额度，且整个文件永远不超过硬上限：
// 生效记录再多也不能把已结束的审计历史挤光，已结束的预算再大也不能把文件撑过硬上限。
func TestEncodeRecordsNeverExceedsHard(t *testing.T) {
	mk := func(id int64, ended bool) *Record {
		x := &Record{ID: id, Target: TargetIP, Key: "198.51.100.1", Reason: "manual", Actor: "op", Since: 10, Permanent: true}
		if ended {
			x.EndedAt, x.End, x.EndActor = 20, EndUnban, "op"
		}
		return x
	}
	var recs []*Record
	for id := int64(1); id <= 20; id++ { // 生效 20 条，已结束 20 条，每类单独都远超硬上限
		recs = append(recs, mk(id, false))
	}
	for id := int64(21); id <= 40; id++ {
		recs = append(recs, mk(id, true))
	}
	const hard = 1000
	for _, budget := range []int{400, 700} { // 预算小于、大于硬上限的一半各测一遍
		data, openDropped, endedDropped, err := encodeRecords(41, 30, recs, budget, hard)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > hard {
			t.Fatalf("预算 %d：文件 %d 字节，超过硬上限 %d", budget, len(data), hard)
		}
		var fd recordsFile
		if err := json.Unmarshal(data, &fd); err != nil {
			t.Fatal(err)
		}
		open, ended := 0, 0
		for _, x := range fd.Records {
			if x.End == "" {
				open++
			} else {
				ended++
			}
		}
		if open == 0 || ended == 0 || openDropped+open != 20 || endedDropped+ended != 20 {
			t.Fatalf("预算 %d：两类记录都应保留一部分、其余按丢弃计数：生效 写入 %d 丢 %d，已结束 写入 %d 丢 %d",
				budget, open, openDropped, ended, endedDropped)
		}
	}
}

// checkAgainst 是对账不变量的把关：记录与名单任何一边对不上都要报出来，且只管本名单的对象类型。
func TestRecordsCheckAgainstDetectsMismatch(t *testing.T) {
	const ip = "198.51.100.142"
	clk := newFakeClock()
	newRec := func(since, until int64, permanent bool) *Records {
		r, err := OpenRecords(RecordsConfig{Now: clk.Now})
		if err != nil {
			t.Fatal(err)
		}
		r.start(Record{Target: TargetIP, Key: ip, Reason: "manual", Actor: "op", Since: since, Until: until, Permanent: permanent})
		return r
	}
	ban := func(since, until int64, permanent bool) map[string]*entry {
		return map[string]*entry{ip: {IP: ip, Ban: &banRec{Since: since, Until: until, Permanent: permanent, Reason: "manual"}}}
	}
	for _, tc := range []struct {
		name    string
		entries map[string]*entry
		wantErr bool
	}{
		{"一致", ban(100, 3700, false), false},
		{"开始时间不同", ban(101, 3700, false), true},
		{"截止时间不同", ban(100, 3800, false), true},
		{"永久与否不同", ban(100, 3700, true), true},
		{"名单里没有这个封禁", map[string]*entry{ip: {IP: ip}}, true},
		{"名单里没有这个条目", map[string]*entry{}, true},
	} {
		r := newRec(100, 3700, false)
		err := r.checkAgainst(TargetIP, tc.entries)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s：期望出错=%v，实际 %v", tc.name, tc.wantErr, err)
		}
		_ = r.Close()
	}
	// 名单里有封禁、记录里没有。
	r, err := OpenRecords(RecordsConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.checkAgainst(TargetIP, ban(100, 3700, false)); err == nil {
		t.Error("名单里有封禁而记录里没有，应报错")
	}
	// 其他对象类型的未结束记录不归本名单管。
	const dev = "dev-0123456789abcdef"
	r.start(Record{Target: TargetDevice, Key: dev, Reason: "manual", Actor: "op", Since: 100, Permanent: true})
	if err := r.checkAgainst(TargetIP, map[string]*entry{}); err != nil {
		t.Errorf("设备对象的记录不应按 IP 名单校验: %v", err)
	}
}

// 触发分项的汇总规则（单元）。
func TestBanTrigger(t *testing.T) {
	const at = 1000
	ev := func(ts int64, kind, detail string, pts int) eventRec {
		return eventRec{TS: ts, Kind: kind, Detail: detail, Points: pts}
	}
	window := 600 * time.Second
	events := []eventRec{
		ev(at-700, "old", "", 9), // 窗口外
		ev(at-500, "a", "x", 2),
		ev(at-300, "b", "", 1),
		ev(at-200, "a", "x", 2),
		ev(at-100, "c", "", 1),
		ev(at-50, "d", "", 1),
		ev(at+10, "future", "", 5), // 晚于封禁时刻（时钟回拨）
	}
	for _, tc := range []struct {
		name    string
		events  []eventRec
		points  int
		want    []TriggerItem
		partial bool
	}{
		{"窗口内、不晚于封禁时刻，按分值排序，最多 3 项；凑不够分值就是部分", events, 100,
			[]TriggerItem{{"a", "x", 2, 4}, {"d", "", 1, 1}, {"c", "", 1, 1}}, true},
		{"累计到封禁分值为止", events, 2,
			[]TriggerItem{{"d", "", 1, 1}, {"c", "", 1, 1}}, false},
		{"不计分的事件不算", []eventRec{ev(at-20, "a", "x", 2), ev(at-10, "z", "", 0)}, 100,
			[]TriggerItem{{"a", "x", 1, 2}}, true},
		{"同分时次数多的在前", []eventRec{ev(at-30, "p", "", 1), ev(at-20, "p", "", 1), ev(at-10, "q", "", 2)}, 100,
			[]TriggerItem{{"p", "", 2, 2}, {"q", "", 1, 2}}, true},
		{"恰好凑够分值不算部分", []eventRec{ev(at-20, "a", "x", 3), ev(at-10, "b", "", 2)}, 5,
			[]TriggerItem{{"a", "x", 1, 3}, {"b", "", 1, 2}}, false},
	} {
		got, partial := banTrigger(tc.events, at, window, tc.points)
		if !reflect.DeepEqual(got, tc.want) || partial != tc.partial {
			t.Errorf("%s: got %+v partial=%v，want %+v partial=%v", tc.name, got, partial, tc.want, tc.partial)
		}
	}
}

// 文件里的字符串按运行时的长度上限截断，触发分项与客户端按条数上限截断。
func TestRecordsLoadSanitizes(t *testing.T) {
	env := newRecEnv(t)
	const ip = "198.51.100.119"
	items := make([]string, 6)
	clients := make([]string, 6)
	for i := range items {
		items[i] = fmt.Sprintf(`{"kind":"k%d\u202e","detail":%q,"count":1,"points":1}`, i, strings.Repeat("d", 300))
		clients[i] = fmt.Sprintf(`{"role":"help","version":%q,"host":"h%d"}`, strings.Repeat("v", 300), i)
	}
	content := fmt.Sprintf(`{"schema":1,"next_id":2,"saved_at":0,"records":[{"id":1,"target":"ip","key":%q,"reason":"manual","actor":%q,"note":%q,"trigger":[%s],"clients":[%s],"since":1790000000,"until":1790003600,"permanent":false,"ended_at":1790000100,"end":"unban","end_actor":%q}]}`,
		ip, strings.Repeat("a", 300), strings.Repeat("n", 1000), strings.Join(items, ","), strings.Join(clients, ","), strings.Repeat("e", 300))
	if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	env.open()
	rec := onlyRec(t, env.r, ip)
	if len(rec.Actor) > maxRecordField || len(rec.EndActor) > maxRecordField || len([]rune(rec.Note)) > maxNoteLen {
		t.Fatalf("字符串应按上限截断: actor=%d end_actor=%d note=%d", len(rec.Actor), len(rec.EndActor), len([]rune(rec.Note)))
	}
	if len(rec.Trigger) != maxRecordTrigger || len(rec.Clients) != maxRecordClients {
		t.Fatalf("分项与客户端应按条数截断: %d %d", len(rec.Trigger), len(rec.Clients))
	}
	for _, it := range rec.Trigger {
		if strings.ContainsRune(it.Kind, '\u202e') || len(it.Detail) > maxRecordField {
			t.Fatalf("分项应剔除控制字符并截断: %+v", it)
		}
	}
	for _, c := range rec.Clients {
		if len(c.Version) > maxClientField {
			t.Fatalf("客户端字段应截断: %+v", c)
		}
	}
}

// ---------- 损坏与互斥 ----------

// 记录文件损坏：记录转纯内存、名单照常持久化、原文件不被覆盖。
func TestRecordsCorruptFileNotOverwritten(t *testing.T) {
	for name, content := range map[string]string{
		"语法错误":   `{"schema":1,"records":[`,
		"未知版本":   `{"schema":99,"next_id":1,"saved_at":0,"records":[]}`,
		"尾部多余数据": `{"schema":1,"next_id":1,"saved_at":0,"records":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := newRecEnv(t)
			if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
			if err == nil {
				t.Fatal("损坏的记录文件应报错")
			}
			if r == nil || r.Persistent() {
				t.Fatal("应返回可用的纯内存 Records")
			}
			cfg := testConfig(env.clk)
			cfg.Path, cfg.Records = env.listPath, r
			l, err := Open(cfg)
			if err != nil {
				t.Fatalf("名单应照常打开: %v", err)
			}
			autoBan(t, l, "198.51.100.95")
			if n := len(r.Snapshot()); n != 1 {
				t.Fatalf("纯内存记录应照常工作，实际 %d 条", n)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(env.recPath); string(got) != content {
				t.Fatalf("损坏的记录文件被覆盖了: %q", got)
			}
			if !l.Persistent() {
				t.Fatal("名单应仍是持久化模式")
			}
		})
	}
}

// 单条坏记录只丢该条，不拒绝整份文件。
func TestRecordsLoadDropsOnlyBadRecords(t *testing.T) {
	env := newRecEnv(t)
	good := `{"id":3,"target":"ip","key":"198.51.100.96","reason":"manual","actor":"op","since":1790000000,"until":1790003600,"permanent":false,"ended_at":1790000100,"end":"unban","end_actor":"op"}`
	bad := []string{
		`{"id":4,"target":"ip","key":"not-an-ip","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`,
		`{"id":5,"target":"ip","key":"198.51.100.97","reason":"weird","actor":"op","since":1790000000,"until":0,"permanent":true}`,
		`{"id":6,"target":"router","key":"198.51.100.98","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`,
		`{"id":7,"target":"ip","key":"198.51.100.99","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true,"end":"exploded","ended_at":1790000100}`,
		`{"id":3,"target":"ip","key":"198.51.100.100","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`,
		`null`,
		`{"id":0,"target":"ip","key":"198.51.100.103","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`,
		`{"id":9,"target":"ip","key":"198.51.100.104","reason":"manual","actor":"op","since":0,"until":0,"permanent":true}`,
		`{"id":10,"target":"ip","key":"198.51.100.105","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true,"end":"unban"}`,
		`{"id":11,"target":"ip","key":"198.51.100.106","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true,"ended_at":1790000100}`,
		`{"id":12,"target":"device","key":"dev-0123456789ABCDEF01234567","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`,
		`{"id":13,"target":"device","key":"dev-0123","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`,
		`{"id":14,"target":"device","key":"198.51.100.107","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}`,
	}
	dev := "dev-" + strings.Repeat("0a", 12)
	goodDev := fmt.Sprintf(`{"id":2,"target":"device","key":%q,"reason":"auto","actor":"auto","since":1790000000,"until":1790003600,"permanent":false,"ended_at":1790003600,"end":"expired","end_actor":"auto"}`, dev)
	content := fmt.Sprintf(`{"schema":1,"next_id":15,"saved_at":0,"records":[%s,%s,%s]}`, goodDev, good, strings.Join(bad, ","))
	if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	env.open()
	rs := env.r.Snapshot()
	if len(rs) != 2 || rs[0].ID != 2 || rs[0].Key != dev || rs[1].ID != 3 || rs[1].Key != "198.51.100.96" {
		t.Fatalf("应只保留那两条好记录: %+v", rs)
	}
	if _, err := env.l.Ban("198.51.100.101", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if id := onlyRec(t, env.r, "198.51.100.101").ID; id != 15 {
		t.Fatalf("新记录 id 应接着 next_id=15，got %d", id)
	}
}

// 同一路径只能被一个进程打开；第二个退回纯内存。
func TestRecordsFileLock(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	r2, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
	if err == nil {
		t.Fatal("第二个实例应拿不到锁")
	}
	if r2 == nil || r2.Persistent() {
		t.Fatal("拿不到锁时应返回纯内存 Records")
	}
	_ = r2.Close()
}

// 记录文件与名单文件写在同一目录，各自的临时文件互不误删。
func TestRecordsTempFilesDoNotCollideWithList(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	if _, err := env.l.Ban("198.51.100.102", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	env.close()
	dir := filepath.Dir(env.recPath)
	// 名单的残留清理只认自己的临时文件命名；记录的临时文件不能落在这个命名里。
	stray := filepath.Join(dir, ".blocklist-crash.tmp")
	if err := os.WriteFile(stray, nil, 0600); err != nil {
		t.Fatal(err)
	}
	recTmp := filepath.Join(dir, recordsTmpPrefix+filepath.Base(env.recPath)+"-1.tmp")
	if err := os.WriteFile(recTmp, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Match(tmpPrefix+"*"+tmpSuffix, filepath.Base(recTmp)); m {
		t.Fatalf("记录的临时文件 %s 会被名单的残留清理误删", filepath.Base(recTmp))
	}
	env.open()
	for _, p := range []string{stray, recTmp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("上次崩溃残留的 %s 应在打开时被清理", filepath.Base(p))
		}
	}
}

// 记录写盘时实际使用的临时文件命名不能落在名单的残留清理范围内（观察真实写盘用的 pattern，而不是常量本身）。
func TestRecordsSaveUsesOwnTempPrefix(t *testing.T) {
	env := newRecEnv(t)
	var mu sync.Mutex
	var patterns []string
	env.rc.atomicWrite = func(path string, data []byte, pattern string) error {
		mu.Lock()
		patterns = append(patterns, pattern)
		mu.Unlock()
		return writeFileAtomicPattern(path, data, pattern)
	}
	env.open()
	if _, err := env.l.Ban("198.51.100.147", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	env.close()
	mu.Lock()
	defer mu.Unlock()
	if len(patterns) == 0 {
		t.Fatal("关闭时应至少写盘一次")
	}
	for _, p := range patterns {
		if !strings.HasPrefix(p, recordsTmpPrefix) {
			t.Errorf("写盘用的临时文件命名 %q 应以 %q 开头", p, recordsTmpPrefix)
		}
		if m, _ := filepath.Match(tmpPrefix+"*"+tmpSuffix, strings.ReplaceAll(p, "*", "x")); m {
			t.Errorf("写盘用的临时文件命名 %q 会被名单的残留清理误删", p)
		}
	}
}

// 记录文件加载不拒绝不认识的字段：以后给记录加字段再回滚到本版时应照常加载。
func TestRecordsLoadToleratesUnknownFields(t *testing.T) {
	env := newRecEnv(t)
	content := `{"schema":1,"next_id":4,"saved_at":0,"future_top":{"a":1},"records":[` +
		`{"id":3,"target":"ip","key":"198.51.100.146","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true,` +
		`"future_field":"x","end":"unban","ended_at":1790000100}]}`
	if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
	if err != nil || !r.Persistent() {
		t.Fatalf("带不认识字段的记录文件应照常加载: %v", err)
	}
	defer r.Close()
	if rs := r.Snapshot(); len(rs) != 1 || rs[0].ID != 3 {
		t.Fatalf("记录应被加载: %+v", rs)
	}
}

// 同一秒内自动封禁被手动永久封禁覆盖，崩溃时记录文件已是覆盖后的状态、名单文件还停在覆盖之前；
// 重启时那条旧的自动封禁到期，不能把到期记到新的手动永久记录头上（它按对不上记 lost）。
func TestRecordsStaleExpiryDoesNotEndNewerRecord(t *testing.T) {
	env := newRecEnv(t)
	const ip = "198.51.100.143"
	env.open()
	autoBan(t, env.l, ip)
	env.close()
	stale, err := os.ReadFile(env.listPath)
	if err != nil {
		t.Fatal(err)
	}
	env.open()
	if _, err := env.l.Ban(ip, 0, true, "op", "覆盖"); err != nil {
		t.Fatal(err)
	}
	env.close()
	if err := os.WriteFile(env.listPath, stale, 0600); err != nil { // 名单文件回到覆盖之前
		t.Fatal(err)
	}
	env.clk.Advance(3 * time.Hour) // 旧的自动封禁（首次，1 小时）已到期
	env.open()
	rs := recsFor(env.r, ip)
	if len(rs) != 2 || rs[0].End != EndReplaced || rs[1].End != EndLost || !rs[1].Permanent || rs[1].EndedAt != env.now() {
		t.Fatalf("旧封禁到期不应结束新的手动永久记录，它应按对不上记 lost：%+v", rs)
	}
	if err := env.l.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// 记录 id 的上限是 2^53：id 空间用尽（只可能来自被篡改的文件）后不再开新记录，
// 不发出下次加载会被丢弃的越界 id。
func TestRecordsIDSpaceExhausted(t *testing.T) {
	open := func(t *testing.T, nextID int64, records string) *Records {
		t.Helper()
		env := newRecEnv(t)
		content := fmt.Sprintf(`{"schema":1,"next_id":%d,"saved_at":0,"records":[%s]}`, nextID, records)
		if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	startN := func(r *Records, keys ...string) {
		for _, k := range keys {
			r.start(Record{Target: TargetIP, Key: k, Reason: "manual", Actor: "op", Since: 1790000000, Permanent: true})
		}
	}
	t.Run("最大 id 的记录已存在", func(t *testing.T) {
		r := open(t, maxRecordID+1, fmt.Sprintf(`{"id":%d,"target":"ip","key":"198.51.100.144","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true,"end":"unban","ended_at":1790000100}`, int64(maxRecordID)))
		if rs := r.Snapshot(); len(rs) != 1 || rs[0].ID != maxRecordID {
			t.Fatalf("2^53 本身是合法 id: %+v", rs)
		}
		startN(r, "198.51.100.145")
		if rs := r.Snapshot(); len(rs) != 1 {
			t.Fatalf("id 用尽后不应再开记录: %+v", rs)
		}
	})
	t.Run("用尽时仍结束同键的旧记录", func(t *testing.T) {
		// 名单加载失败退回纯内存时，记录里仍生效的旧封禁不会被对账；之后同键又开新封禁，
		// 即使不再发新 id，旧记录也要按对不上结束，不能一直显示「封禁中」。
		env := newRecEnv(t)
		content := fmt.Sprintf(`{"schema":1,"next_id":%d,"saved_at":0,"records":[{"id":5,"target":"ip","key":"198.51.100.149","reason":"manual","actor":"op","since":1790000000,"until":0,"permanent":true}]}`, int64(maxRecordID)+1)
		if err := os.WriteFile(env.recPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := OpenRecords(RecordsConfig{Path: env.recPath, Now: env.clk.Now})
		if err != nil {
			t.Fatal(err)
		}
		r.start(Record{Target: TargetIP, Key: "198.51.100.149", Reason: "manual", Actor: "op", Since: 1790000500, Until: 1790004100})
		rs := r.Snapshot()
		if len(rs) != 1 || rs[0].ID != 5 || rs[0].End != EndLost {
			t.Fatalf("id 用尽时同键旧记录仍应记 lost，且不开新记录: %+v", rs)
		}
		defer r.Close()
		// 清理的结果要靠后台写盘落下来（不能只指望关闭时那一次），否则崩溃后旧记录又变回「封禁中」。
		deadline := time.Now().Add(5 * time.Second)
		for r.saveCount.Load() < 1 { // saver 的第一次写盘不受间隔限制
			if time.Now().After(deadline) {
				t.Fatal("清理同键旧记录后 5 秒内没有写盘")
			}
			time.Sleep(time.Millisecond)
		}
		fd, err := loadRecordsFile(env.recPath)
		if err != nil || fd == nil || len(fd.Records) != 1 || fd.Records[0].End != EndLost {
			t.Fatalf("写盘后文件里旧记录应是 lost: %+v %v", fd, err)
		}
	})
	t.Run("下一个 id 恰为上限", func(t *testing.T) {
		r := open(t, maxRecordID, "")
		startN(r, "198.51.100.145", "198.51.100.148")
		if rs := r.Snapshot(); len(rs) != 1 || rs[0].ID != maxRecordID {
			t.Fatalf("应只发出一个 id=2^53 的记录，之后不再发: %+v", rs)
		}
	})
}

// ---------- 其他 ----------

func TestSiblingPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "",
		"/var/lib/relay/blocklist.json": "/var/lib/relay/blocklist-records.json",
		"/srv/relay/.remote_assist_relay_default_blocklist.json": "/srv/relay/.remote_assist_relay_default_blocklist-records.json",
		"/x.d/blocklist": "/x.d/blocklist-records",
		"/x/.blocklist":  "/x/.blocklist-records",
	} {
		in, want = filepath.FromSlash(in), filepath.FromSlash(want)
		if got := SiblingPath(in, "records"); got != want {
			t.Errorf("SiblingPath(%q) = %q，want %q", in, got, want)
		}
	}
}

// 记录文件的 JSON 字段名是控制台 API 的契约。
func TestRecordJSONFields(t *testing.T) {
	rec := Record{ID: 1, Target: TargetIP, Key: "198.51.100.1", Reason: "auto", Actor: "auto", Note: "n",
		Trigger: []TriggerItem{{Kind: "join_failed", Detail: "code_invalid", Count: 1, Points: 1}}, TriggerPartial: true,
		Clients: []RecordClient{{Role: "help", Version: "v1", Host: "h"}}, Offense: 1,
		Since: 10, Until: 20, EndedAt: 20, End: EndExpired, EndActor: "auto", Label: "l", Via: "198.51.100.2"}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "target", "key", "label", "via", "reason", "actor", "note", "trigger", "trigger_partial", "clients", "offense", "since", "until", "permanent", "ended_at", "end", "end_actor"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺字段 %q：%s", k, b)
		}
	}
}
