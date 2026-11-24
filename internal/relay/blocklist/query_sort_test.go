package blocklist

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

// L2：列表搜索 / 筛选 / 排序 / 游标。
//
// 预言机（oracle）完全基于对外可见的视图（SummaryView / EntryView）独立实现筛选与排序，
// 与 Query 内部的实现互不依赖；随机游走翻页测试拿它当"全量排序后切片"的标准答案。

var (
	l2Hosts    = []string{"pc-alpha", "pc-beta", "LAB-7", "mac-mini"}
	l2Versions = []string{"1.2.0", "1.3.1", "2.0.0-rc1"}
	l2Notes    = []string{"扫描器", "测试机误封", "重复骚扰", "scan-bot"}
	l2Kinds    = []Kind{KindTLSGarbage, KindProtoDecode, KindProtoState, KindUnknownMessage, KindJoinFailed, KindJoinFailuresExceeded}
)

// seedL2 生成 n 个键：时钟大多数时候不动、偶尔推进 1 秒，让 last_seen / 封禁时间 / 到期时间大量相同，考验 tie-breaker。
func seedL2(t *testing.T, l *List, clk *fakeClock, r *rand.Rand, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("10.9.%d.%d", i/200, i%200+1)
		if i%7 == 3 {
			ip = fmt.Sprintf("2001:db8:%x:1::5", i)
		}
		if r.Intn(2) == 0 {
			l.ObserveClient(ip, ClientInfo{Role: "share", Version: l2Versions[r.Intn(len(l2Versions))], Host: l2Hosts[r.Intn(len(l2Hosts))], ClientID: fmt.Sprintf("c%d", i)})
		}
		switch r.Intn(6) {
		case 0:
			l.ObserveConnect(ip)
		case 1:
			hit(l, ip, KindProtoDecode, 1+r.Intn(8)) // 观察中，分值 2..16
		case 2:
			hit(l, ip, l2Kinds[r.Intn(len(l2Kinds))], 20) // 自动封禁
		case 3:
			d := time.Duration(1+r.Intn(2)) * time.Hour
			perm := r.Intn(4) == 0
			if _, err := l.Ban(ip, d, perm, "op", l2Notes[r.Intn(len(l2Notes))]); err != nil {
				t.Fatalf("Ban %s: %v", ip, err)
			}
		case 4:
			if err := l.Allow(ip, 24*time.Hour, r.Intn(3) == 0, "op", l2Notes[r.Intn(len(l2Notes))]); err != nil {
				t.Fatalf("Allow %s: %v", ip, err)
			}
		default:
			l.ObserveConnect(ip)
			hit(l, ip, KindProtoState, 1+r.Intn(3))
		}
		if r.Intn(5) == 0 {
			clk.Advance(time.Second)
		}
	}
}

type oracleRow struct {
	s SummaryView
	d EntryView
}

func allRows(t *testing.T, l *List) []oracleRow {
	t.Helper()
	var rows []oracleRow
	for off := 0; ; off += MaxPageLimit {
		p := l.Query(Filter{Status: "tracked", Offset: off, Limit: MaxPageLimit})
		for _, s := range p.Entries {
			d, ok := l.Detail(s.IP)
			if !ok {
				t.Fatalf("Detail(%s) 不存在", s.IP)
			}
			rows = append(rows, oracleRow{s: s, d: d})
		}
		if off+MaxPageLimit >= p.Total {
			return rows
		}
	}
}

func (r oracleRow) matches(f Filter) bool {
	s := r.s
	switch f.Status {
	case "banned":
		if s.Ban == nil {
			return false
		}
	case "allowlisted":
		if s.Allow == nil {
			return false
		}
	case "grace":
		if s.GraceUntil == 0 {
			return false
		}
	case "suspect":
		if s.Status != "suspect" {
			return false
		}
	case "tracked":
	default:
		if s.Status == "seen" && len(r.d.Events) == 0 && len(r.d.History) == 0 {
			return false
		}
	}
	if f.Reason != "" && (s.Ban == nil || s.Ban.Reason != f.Reason) {
		return false
	}
	if f.Kind != "" && (s.Ban == nil || s.Ban.Kind != f.Kind) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Search)); q != "" {
		hay := []string{s.IP}
		for _, c := range r.d.Clients {
			hay = append(hay, c.Host, c.Version)
		}
		if s.Ban != nil {
			hay = append(hay, s.Ban.Note)
		}
		if s.Allow != nil {
			hay = append(hay, s.Allow.Note)
		}
		found := false
		for _, h := range hay {
			if strings.Contains(strings.ToLower(h), q) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// key 返回排序键：null=排序值缺失（永远排最后）。
func (r oracleRow) key(sortName string) (null bool, v1, v2 int64) {
	s := r.s
	switch sortName {
	case SortLastSeen:
		return false, s.LastSeen, 0
	case SortBannedAt:
		if s.Ban == nil {
			return true, 0, 0
		}
		return false, s.Ban.Since, 0
	case SortExpires:
		if s.Ban == nil {
			return true, 0, 0
		}
		if s.Ban.Permanent {
			return false, math.MaxInt64, 0
		}
		return false, s.Ban.Until, 0
	case SortPoints:
		return false, int64(s.Points), s.LastSeen
	default:
		return false, int64(4 - statusRank[s.Status]), s.LastSeen
	}
}

func oracleList(rows []oracleRow, f Filter) []string {
	sortName, asc := f.Sort, f.Order == OrderAsc
	if sortName == "" {
		sortName = SortStatus
	}
	var out []oracleRow
	for _, r := range rows {
		if r.matches(f) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		an, a1, a2 := out[i].key(sortName)
		bn, b1, b2 := out[j].key(sortName)
		if an != bn {
			return !an
		}
		if !an {
			if a1 != b1 {
				return (a1 < b1) == asc
			}
			if a2 != b2 {
				return (a2 < b2) == asc
			}
		}
		return (out[i].s.IP < out[j].s.IP) != asc
	})
	ips := make([]string, len(out))
	for i, r := range out {
		ips[i] = r.s.IP
	}
	return ips
}

func entryIPs(p Page) []string {
	out := make([]string, len(p.Entries))
	for i, e := range p.Entries {
		out[i] = e.IP
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func l2Filters() []Filter {
	return []Filter{
		{Status: "all"},
		{Status: "tracked"},
		{Status: "banned"},
		{Status: "banned", Kind: string(KindProtoDecode)},
		{Status: "all", Kind: string(KindJoinFailuresExceeded)},
		{Status: "all", Reason: "manual"},
		{Status: "banned", Reason: "auto", Kind: string(KindUnknownMessage)},
		{Status: "all", Search: "pc-alpha"},
		{Status: "all", Search: "  LAB-7 "}, // 前后空白与大小写不影响
		{Status: "tracked", Search: "2.0.0"},
		{Status: "all", Search: "2001:db8"},
		{Status: "all", Search: "扫描器"},
		{Status: "banned", Reason: "manual", Search: "10.9.0.1"},
		{Status: "all", Search: "没有任何条目会匹配这个"},
	}
}

// 随机游走翻页：每种排序 × 正倒序 × 若干筛选，随机前后翻页，页内容必须等于"全量排序后切片"，
// 相邻两页 0 跳过 0 重复；再各做一次完整的正向/反向遍历。
func TestQueryRandomWalkPaging(t *testing.T) {
	seed := time.Now().UnixNano()
	r := rand.New(rand.NewSource(seed))
	t.Logf("seed=%d", seed)
	clk := newFakeClock()
	l := openMem(t, clk)
	seedL2(t, l, clk, r, 420)
	rows := allRows(t, l)

	ties := map[int64]int{}
	for _, row := range rows {
		ties[row.s.LastSeen]++
	}
	if len(ties) >= len(rows)/2 {
		t.Fatalf("测试数据 last_seen 相同值太少（%d 个不同值 / %d 条），起不到考验 tie-breaker 的作用", len(ties), len(rows))
	}

	for _, sortName := range []string{"", SortStatus, SortLastSeen, SortBannedAt, SortExpires, SortPoints} {
		for _, order := range []string{"", OrderDesc, OrderAsc} {
			for fi, base := range l2Filters() {
				f := base
				f.Sort, f.Order = sortName, order
				name := fmt.Sprintf("sort=%q/order=%q/f%d", sortName, order, fi)
				truth := oracleList(rows, f)
				f.Limit = 1 + r.Intn(37)

				first := l.Query(f)
				if first.Total != len(truth) {
					t.Fatalf("%s: total=%d 期望 %d", name, first.Total, len(truth))
				}
				if want := truth[:min(f.Limit, len(truth))]; !sameStrings(entryIPs(first), want) {
					t.Fatalf("%s: 第一页 %v 期望 %v", name, entryIPs(first), want)
				}
				if first.HasPrev || first.Start != 0 {
					t.Fatalf("%s: 第一页 has_prev=%v start=%d", name, first.HasPrev, first.Start)
				}

				// 随机游走
				cur := first
				for step := 0; step < 60; step++ {
					forward := r.Intn(10) < 7
					if forward && !cur.HasNext || !forward && !cur.HasPrev {
						forward = !forward
					}
					g := f
					g.Limit = 1 + r.Intn(37)
					switch {
					case forward && cur.HasNext:
						g.Cursor, g.Dir = cur.NextCursor, "next"
					case !forward && cur.HasPrev:
						g.Cursor, g.Dir = cur.PrevCursor, "prev"
					default:
						continue
					}
					nxt := l.Query(g)
					if nxt.CursorReset {
						t.Fatalf("%s: 同排序同筛选的游标不应被重置", name)
					}
					if nxt.Total != len(truth) {
						t.Fatalf("%s: total 漂移", name)
					}
					got := entryIPs(nxt)
					if nxt.Start < 0 || nxt.Start+len(got) > len(truth) || !sameStrings(got, truth[nxt.Start:nxt.Start+len(got)]) {
						t.Fatalf("%s: 第 %d 步 start=%d 页内容 %v 与全量排序切片不一致", name, step, nxt.Start, got)
					}
					if g.Dir == "next" {
						if nxt.Start != cur.Start+len(cur.Entries) {
							t.Fatalf("%s: 向后翻页衔接错误：上页 start=%d n=%d，本页 start=%d（跳过或重复）", name, cur.Start, len(cur.Entries), nxt.Start)
						}
					} else {
						switch {
						case nxt.Start == 0:
							// 越过开头：补满的第一页
							if len(got) != min(g.Limit, len(truth)) {
								t.Fatalf("%s: 越过开头应补满，got %d", name, len(got))
							}
						case nxt.Start+len(got) != cur.Start:
							t.Fatalf("%s: 向前翻页衔接错误：上页 start=%d，本页 start=%d n=%d", name, cur.Start, nxt.Start, len(got))
						}
					}
					if nxt.HasPrev != (nxt.Start > 0) || nxt.HasNext != (nxt.Start+len(got) < len(truth)) {
						t.Fatalf("%s: has_prev/has_next 与位置不符", name)
					}
					cur = nxt
				}

				// 完整正向遍历
				var seq []string
				p := l.Query(f)
				seq = append(seq, entryIPs(p)...)
				for p.HasNext {
					g := f
					g.Cursor, g.Dir = p.NextCursor, "next"
					p = l.Query(g)
					seq = append(seq, entryIPs(p)...)
				}
				if !sameStrings(seq, truth) {
					t.Fatalf("%s: 正向遍历 %d 条，期望 %d 条且顺序一致（0 跳过 0 重复）", name, len(seq), len(truth))
				}
				// 从末页之后的空页完整反向遍历（页与页之间不重叠；最后一步的补满页只取新增部分）
				if len(truth) > 0 {
					g := f
					g.Cursor, g.Dir = p.NextCursor, "next"
					q := l.Query(g)
					if len(q.Entries) != 0 {
						t.Fatalf("%s: 末页之后应为空页", name)
					}
					var rev []string
					covered := len(truth)
					for q.HasPrev {
						g := f
						g.Cursor, g.Dir = q.PrevCursor, "prev"
						q = l.Query(g)
						got := entryIPs(q)
						if q.Start+len(got) > covered {
							got = got[:covered-q.Start]
						}
						rev = append(append([]string(nil), got...), rev...)
						covered = q.Start
					}
					if !sameStrings(rev, truth) {
						t.Fatalf("%s: 反向遍历 %d 条，期望 %d 条且顺序一致", name, len(rev), len(truth))
					}
				}
			}
		}
	}
}

// 换排序 / 筛选后，旧游标作废：服务端忽略它，从第一页返回并标记 CursorReset。
func TestQueryCursorMismatchRestartsFromFirstPage(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	seedL2(t, l, clk, rand.New(rand.NewSource(7)), 150)

	base := Filter{Status: "all", Sort: SortLastSeen, Order: OrderDesc, Limit: 10}
	p1 := l.Query(base)
	if !p1.HasNext || p1.Sort != SortLastSeen || p1.Order != OrderDesc {
		t.Fatalf("第一页应回显排序并有下一页: %+v", p1)
	}
	use := func(g Filter) Page {
		g.Cursor, g.Dir = p1.NextCursor, "next"
		return l.Query(g)
	}
	// 同排序同筛选：正常翻页
	if p := use(base); p.CursorReset || p.Start != 10 {
		t.Fatalf("同条件应正常翻页: reset=%v start=%d", p.CursorReset, p.Start)
	}
	// 搜索词大小写 / 空白不同但归一化后相同：仍有效
	s1 := l.Query(Filter{Status: "all", Search: "PC-alpha", Limit: 3})
	if !s1.HasNext {
		t.Fatal("测试数据里 pc-alpha 应多于 3 条")
	}
	if p := l.Query(Filter{Status: "all", Search: " pc-ALPHA ", Limit: 3, Cursor: s1.NextCursor, Dir: "next"}); p.CursorReset {
		t.Fatal("搜索词仅大小写/空白不同，游标不应失效")
	}
	mismatches := map[string]Filter{
		"换排序方式": {Status: "all", Sort: SortPoints, Order: OrderDesc, Limit: 10},
		"换方向":   {Status: "all", Sort: SortLastSeen, Order: OrderAsc, Limit: 10},
		"换状态":   {Status: "banned", Sort: SortLastSeen, Order: OrderDesc, Limit: 10},
		"换来源":   {Status: "all", Reason: "manual", Sort: SortLastSeen, Order: OrderDesc, Limit: 10},
		"换触发原因": {Status: "all", Kind: string(KindProtoDecode), Sort: SortLastSeen, Order: OrderDesc, Limit: 10},
		"换搜索词":  {Status: "all", Search: "pc", Sort: SortLastSeen, Order: OrderDesc, Limit: 10},
	}
	for name, g := range mismatches {
		p := use(g)
		if !p.CursorReset || p.Start != 0 || p.HasPrev {
			t.Errorf("%s：应忽略旧游标并从第一页开始: reset=%v start=%d has_prev=%v", name, p.CursorReset, p.Start, p.HasPrev)
		}
		g.Cursor, g.Dir = "", ""
		want := l.Query(g)
		if !sameStrings(entryIPs(p), entryIPs(want)) {
			t.Errorf("%s：重置后的页应等于该条件的第一页", name)
		}
		// 带着旧游标的 prev 同样处理
		g.Cursor, g.Dir = p1.PrevCursor, "prev"
		if q := l.Query(g); !q.CursorReset || q.Start != 0 {
			t.Errorf("%s(prev)：应从第一页开始", name)
		}
		// offset 与不匹配的游标同时出现时，以游标为准（重置），不使用 offset
		g.Offset = 20
		if q := l.Query(g); q.Start != 0 {
			t.Errorf("%s：游标不匹配时不应再使用 offset，start=%d", name, q.Start)
		}
	}
	// 重置后的页自己的游标是有效的
	g := mismatches["换排序方式"]
	reset := use(g)
	g.Cursor, g.Dir = reset.NextCursor, "next"
	if p := l.Query(g); p.CursorReset || p.Start != 10 {
		t.Fatalf("重置页的游标应可继续翻页: reset=%v start=%d", p.CursorReset, p.Start)
	}
}

func TestParseCursorRejectsGarbage(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	seedL2(t, l, clk, rand.New(rand.NewSource(3)), 40)
	good := l.Query(Filter{Status: "all", Limit: 5}).NextCursor
	if err := ParseCursor(good); err != nil {
		t.Fatalf("合法游标被拒绝: %v", err)
	}
	// 旧版三段式游标（0|1|1.2.3.4）不再接受。
	for _, bad := range []string{"", "!!!", "eA", "MHwxfDEuMi4zLjQ", strings.Repeat("A", 400)} {
		if err := ParseCursor(bad); err == nil {
			t.Errorf("ParseCursor(%q) 应报错", bad)
		}
	}
}

func TestFilterValidate(t *testing.T) {
	ok := []Filter{{}, {Sort: SortExpires, Order: OrderAsc, Kind: string(KindJoinFailed), Search: "x"}, {Sort: SortStatus}}
	for _, f := range ok {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v 应合法: %v", f, err)
		}
	}
	bad := []Filter{
		{Sort: "ip"}, {Sort: "LAST_SEEN"}, {Order: "up"}, {Kind: "nope"}, {Kind: "manual"},
		{Search: strings.Repeat("a", MaxSearchLen+1)}, {Search: strings.Repeat("汉", MaxSearchLen+1)},
	}
	for _, f := range bad {
		if err := f.Validate(); err == nil {
			t.Errorf("%+v 应被拒绝", f)
		}
	}
	if err := (Filter{Search: strings.Repeat("汉", MaxSearchLen)}).Validate(); err != nil {
		t.Errorf("恰好 MaxSearchLen 个字符应合法: %v", err)
	}
}

// 搜索覆盖键（含 IPv6 网段）、主机名、版本、封禁备注、白名单备注；不区分大小写；子串匹配。
func TestQuerySearchFields(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	l.ObserveClient("10.5.0.1", ClientInfo{Role: "share", Version: "9.8.7-beta", Host: "Build-Server-01", ClientID: "a"})
	l.ObserveConnect("10.5.0.2")
	if _, err := l.Ban("10.5.0.2", time.Hour, false, "op", "疑似撞库扫描"); err != nil {
		t.Fatal(err)
	}
	l.ObserveConnect("10.5.0.3")
	if err := l.Allow("10.5.0.3", time.Hour, false, "op", "Office-NAT"); err != nil {
		t.Fatal(err)
	}
	l.ObserveClient("2001:db8:abcd:1::9", ClientInfo{Role: "help", Version: "1.0", Host: "h", ClientID: "b"})
	cases := []struct {
		q    string
		want []string
	}{
		{"build-server", []string{"10.5.0.1"}},
		{"BUILD-SERVER-01", []string{"10.5.0.1"}},
		{"9.8.7-BETA", []string{"10.5.0.1"}},
		{"撞库", []string{"10.5.0.2"}},
		{"office-nat", []string{"10.5.0.3"}},
		{"10.5.0.2", []string{"10.5.0.2"}},
		{"2001:db8:abcd", []string{"2001:db8:abcd:1::/64"}},
		{"/64", []string{"2001:db8:abcd:1::/64"}},
		{"zzz-none", nil},
	}
	for _, c := range cases {
		p := l.Query(Filter{Status: "tracked", Search: c.q, Sort: SortLastSeen, Order: OrderAsc, Limit: 50})
		got := entryIPs(p)
		sort.Strings(got)
		if !sameStrings(got, c.want) || p.Total != len(c.want) {
			t.Errorf("搜索 %q: got %v 期望 %v", c.q, got, c.want)
		}
	}
}

// 触发原因只匹配有封禁、且封禁时最后一类事件相同的条目（手动封禁没有触发原因）。
func TestQueryKindFilter(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	hit(l, "10.6.0.1", KindProtoDecode, 20)
	hit(l, "10.6.0.2", KindJoinFailed, 20)
	hit(l, "10.6.0.3", KindProtoDecode, 3) // 观察中，未封禁
	if _, err := l.Ban("10.6.0.4", time.Hour, false, "op", "x"); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string][]string{
		string(KindProtoDecode): {"10.6.0.1"},
		string(KindJoinFailed):  {"10.6.0.2"},
		string(KindTLSGarbage):  nil,
	} {
		p := l.Query(Filter{Status: "tracked", Kind: kind, Limit: 50})
		got := entryIPs(p)
		sort.Strings(got)
		if !sameStrings(got, want) {
			t.Errorf("kind=%s: got %v 期望 %v", kind, got, want)
		}
	}
}

// 排序值缺失（未封禁的条目没有封禁时间 / 到期时间）永远排在最后，无论正倒序；永久封禁的到期时间视为最晚。
func TestQuerySortNullsLastAndPermanent(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	l.ObserveConnect("10.7.0.1") // 未封禁
	if _, err := l.Ban("10.7.0.2", time.Hour, false, "op", "a"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if _, err := l.Ban("10.7.0.3", 0, true, "op", "b"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if _, err := l.Ban("10.7.0.4", 2*time.Hour, false, "op", "c"); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		SortExpires + "/" + OrderDesc:  {"10.7.0.3", "10.7.0.4", "10.7.0.2", "10.7.0.1"}, // 永久最晚
		SortExpires + "/" + OrderAsc:   {"10.7.0.2", "10.7.0.4", "10.7.0.3", "10.7.0.1"},
		SortBannedAt + "/" + OrderDesc: {"10.7.0.4", "10.7.0.3", "10.7.0.2", "10.7.0.1"},
		SortBannedAt + "/" + OrderAsc:  {"10.7.0.2", "10.7.0.3", "10.7.0.4", "10.7.0.1"},
	}
	for k, w := range want {
		parts := strings.SplitN(k, "/", 2)
		got := entryIPs(l.Query(Filter{Status: "tracked", Sort: parts[0], Order: parts[1], Limit: 50}))
		if !sameStrings(got, w) {
			t.Errorf("%s: got %v 期望 %v", k, got, w)
		}
	}
}

// 默认排序（不带 sort / order）保持原来的「状态优先、再按最近出现」，键升序兜底。
func TestQueryDefaultSortUnchanged(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	seedL2(t, l, clk, rand.New(rand.NewSource(11)), 120)
	rows := allRows(t, l)
	got := entryIPs(l.Query(Filter{Status: "tracked", Limit: MaxPageLimit}))
	want := oracleList(rows, Filter{Status: "tracked"})
	if !sameStrings(got, want) {
		t.Fatalf("默认排序与「状态优先再按最近出现」不一致")
	}
	p := l.Query(Filter{Status: "tracked", Limit: 5})
	if p.Sort != SortStatus || p.Order != OrderDesc {
		t.Fatalf("默认应回显 sort=status order=desc: %q %q", p.Sort, p.Order)
	}
}
