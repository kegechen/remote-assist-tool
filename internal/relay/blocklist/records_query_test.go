package blocklist

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 封禁记录查询：参数白名单、筛选、搜索、按 id 倒序的游标分页。

func recIDs(p RecordPage) []int64 {
	out := []int64{}
	for _, x := range p.Records {
		out = append(out, x.ID)
	}
	return out
}

const (
	qDevA = "dev-0123456789abcdef01234567"
	qDevB = "dev-fedcba9876543210fedcba98"
)

// seedRecordQuery 直接开 / 结束记录，覆盖两种对象、两种来源、每一种结束方式，外加一条计划已过、
// 名单还没清扫到的生效记录（id 9）。返回的 now 是时钟当前时刻。
func seedRecordQuery(t *testing.T) (*Records, *fakeClock, int64) {
	t.Helper()
	clk := newFakeClock()
	r, err := OpenRecords(RecordsConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now().Unix()
	// 1：IP 自动，生效中；封禁时的客户端主机名 Office-PC
	r.start(Record{Target: TargetIP, Key: "198.51.100.1", Reason: "auto", Actor: autoActor, Offense: 1, Since: now - 60, Until: now + 3600,
		Trigger: []TriggerItem{{Kind: "join_failed", Detail: "code_invalid", Count: 20, Points: 20}},
		Clients: []RecordClient{{Role: "help", Version: "1.2.3", Host: "Office-PC"}}})
	// 2：IP 手动，人工解禁；备注与处置人不参与搜索
	r.start(Record{Target: TargetIP, Key: "198.51.100.2", Reason: "manual", Actor: "ops-alice", Note: "误封测试 office", Since: now - 7200, Until: now + 3600})
	r.end(TargetIP, "198.51.100.2", banRef{since: now - 7200, until: now + 3600}, EndUnban, now-3600, "ops-bob")
	// 3：设备 自动，到期
	r.start(Record{Target: TargetDevice, Key: qDevA, Label: "Lab-Host", Via: "203.0.113.9", Reason: "auto", Actor: autoActor, Offense: 2, Since: now - 9000, Until: now - 5400})
	r.end(TargetDevice, qDevA, banRef{since: now - 9000, until: now - 5400}, EndExpired, now-5400, autoActor)
	// 4：IP 自动，超配额被提前解除
	r.start(Record{Target: TargetIP, Key: "198.51.100.4", Reason: "auto", Actor: autoActor, Offense: 1, Since: now - 5000, Until: now + 5000})
	r.end(TargetIP, "198.51.100.4", banRef{since: now - 5000, until: now + 5000}, EndEvicted, now-4000, autoActor)
	// 5、6：同一 IP 的自动封禁被手动永久封禁覆盖
	r.start(Record{Target: TargetIP, Key: "198.51.100.5", Reason: "auto", Actor: autoActor, Offense: 1, Since: now - 3000, Until: now + 3000})
	r.end(TargetIP, "198.51.100.5", banRef{since: now - 3000, until: now + 3000}, EndReplaced, now-2000, "ops-alice")
	r.start(Record{Target: TargetIP, Key: "198.51.100.5", Reason: "manual", Actor: "ops-alice", Since: now - 2000, Permanent: true})
	// 7：IPv6 /64 自动，重启后对不上
	r.start(Record{Target: TargetIP, Key: "2001:db8:1:2::/64", Reason: "auto", Actor: autoActor, Offense: 1, Since: now - 1000, Until: now + 1000})
	r.end(TargetIP, "2001:db8:1:2::/64", banRef{since: now - 1000, until: now + 1000}, EndLost, now-500, autoActor)
	// 8：设备 手动永久，生效中
	r.start(Record{Target: TargetDevice, Key: qDevB, Label: "Kiosk-7", Via: "198.51.100.200", Reason: "manual", Actor: "ops-alice", Since: now - 100, Permanent: true})
	// 9：IP 自动，计划已过（10 秒前到期）但名单还没清扫到，记录仍未结束
	r.start(Record{Target: TargetIP, Key: "198.51.100.9", Reason: "auto", Actor: autoActor, Offense: 3, Since: now - 7200, Until: now - 10})
	return r, clk, now
}

func TestRecordFilterValidate(t *testing.T) {
	long := strings.Repeat("a", MaxSearchLen+1)
	longCJK := strings.Repeat("汉", MaxSearchLen+1)
	for _, f := range []RecordFilter{
		{Target: "IP"}, {Target: "x"}, {Target: "ALL"},
		{State: "open"}, {State: "ACTIVE"}, {State: "banned"},
		{Reason: "Auto"}, {Reason: "x"},
		{End: "EXPIRED"}, {End: "bogus"}, {End: "active"},
		{Search: long}, {Search: longCJK},
		{Limit: -1}, {Limit: MaxPageLimit + 1},
	} {
		if err := f.Validate(); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("%+v 应报 ErrInvalidFilter，got %v", f, err)
		}
	}
	for _, f := range []RecordFilter{{Cursor: -1}, {Cursor: maxRecordID + 1}} {
		if err := f.Validate(); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%+v 应报 ErrInvalidCursor，got %v", f, err)
		}
	}
	valid := []RecordFilter{
		{}, {Target: "all"}, {Target: TargetIP}, {Target: TargetDevice},
		{State: "all"}, {State: RecordStateActive}, {State: RecordStateEnded},
		{Reason: "all"}, {Reason: "auto"}, {Reason: "manual"},
		{End: "all"}, {Search: strings.Repeat("汉", MaxSearchLen)},
		{Limit: MaxPageLimit}, {Cursor: 1}, {Cursor: maxRecordID},
	}
	for _, end := range []string{EndExpired, EndUnban, EndEvicted, EndReplaced, EndLost} {
		valid = append(valid, RecordFilter{End: end})
	}
	for _, f := range valid {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v 应合法，got %v", f, err)
		}
	}
}

func TestParseRecordCursor(t *testing.T) {
	for raw, want := range map[string]int64{"1": 1, "42": 42, "9007199254740992": maxRecordID} {
		got, err := ParseRecordCursor(raw)
		if err != nil || got != want {
			t.Errorf("ParseRecordCursor(%q) = %d, %v；want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "0", "-1", "+5", " 5", "5 ", "1.5", "1e3", "abc", "0x10", "٣",
		"9007199254740993", "99999999999999999999", strings.Repeat("1", 40), strings.Repeat("0", 16) + "1"} {
		if _, err := ParseRecordCursor(raw); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("ParseRecordCursor(%q) 应报 ErrInvalidCursor，got %v", raw, err)
		}
	}
}

func TestRecordsQueryFilters(t *testing.T) {
	r, _, _ := seedRecordQuery(t)
	for _, tc := range []struct {
		f    RecordFilter
		want []int64
	}{
		{RecordFilter{}, []int64{9, 8, 7, 6, 5, 4, 3, 2, 1}},
		{RecordFilter{Target: "all", State: "all", Reason: "all", End: "all"}, []int64{9, 8, 7, 6, 5, 4, 3, 2, 1}},
		{RecordFilter{Target: TargetIP}, []int64{9, 7, 6, 5, 4, 2, 1}},
		{RecordFilter{Target: TargetDevice}, []int64{8, 3}},
		{RecordFilter{State: RecordStateActive}, []int64{8, 6, 1}},
		{RecordFilter{State: RecordStateEnded}, []int64{9, 7, 5, 4, 3, 2}},
		{RecordFilter{Reason: "auto"}, []int64{9, 7, 5, 4, 3, 1}},
		{RecordFilter{Reason: "manual"}, []int64{8, 6, 2}},
		{RecordFilter{End: EndExpired}, []int64{9, 3}},
		{RecordFilter{End: EndUnban}, []int64{2}},
		{RecordFilter{End: EndEvicted}, []int64{4}},
		{RecordFilter{End: EndReplaced}, []int64{5}},
		{RecordFilter{End: EndLost}, []int64{7}},
		{RecordFilter{State: RecordStateActive, End: EndExpired}, []int64{}},
		{RecordFilter{Target: TargetDevice, State: RecordStateActive}, []int64{8}},
		{RecordFilter{Target: TargetIP, Reason: "manual", State: RecordStateEnded}, []int64{2}},
	} {
		p := r.Query(tc.f)
		if got := recIDs(p); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v：got %v，want %v", tc.f, got, tc.want)
		}
		if p.Total != len(tc.want) || p.Start != 0 || p.HasNext || p.NextCursor != 0 {
			t.Errorf("%+v：total=%d start=%d has_next=%v next=%d", tc.f, p.Total, p.Start, p.HasNext, p.NextCursor)
		}
	}
}

// 搜索匹配键、via、label 与封禁时客户端的主机名（不区分大小写、去首尾空白）；备注与处置人不参与。
func TestRecordsQuerySearch(t *testing.T) {
	r, _, _ := seedRecordQuery(t)
	for _, tc := range []struct {
		q    string
		want []int64
	}{
		{"office", []int64{1}},          // 客户端主机名 Office-PC；2 号的备注里有 office 但不参与
		{"OFFICE-pc", []int64{1}},       // 不区分大小写
		{"203.0.113", []int64{3}},       // via
		{"kiosk", []int64{8}},           // label
		{"  lab-host  ", []int64{3}},    // 去首尾空白
		{"198.51.100.5", []int64{6, 5}}, // 键
		{"198.51.100.2", []int64{8, 2}}, // 键（2）与 via（8 的 198.51.100.200）
		{"2001:DB8", []int64{7}},
		{"dev-0123", []int64{3}},
		{"ops-", []int64{}}, // 处置人不参与
		{"误封测试", []int64{}}, // 备注不参与
		{"", []int64{9, 8, 7, 6, 5, 4, 3, 2, 1}},
	} {
		if got := recIDs(r.Query(RecordFilter{Search: tc.q})); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("q=%q：got %v，want %v", tc.q, got, tc.want)
		}
	}
	if got := recIDs(r.Query(RecordFilter{Search: "198.51.100.2", Target: TargetDevice})); !reflect.DeepEqual(got, []int64{8}) {
		t.Errorf("搜索与对象类型应取交集：got %v", got)
	}
}

// 计划已过、名单还没清扫到的生效记录按到期展示与筛选（名单清扫时写入的正是 expired / until / auto），
// 内部记录本身不改。
func TestRecordsQueryPlanPassedShownExpired(t *testing.T) {
	r, clk, now := seedRecordQuery(t)
	p := r.Query(RecordFilter{End: EndExpired})
	if len(p.Records) != 2 || p.Records[0].ID != 9 {
		t.Fatalf("got %+v", p.Records)
	}
	if x := p.Records[0]; x.End != EndExpired || x.EndedAt != now-10 || x.EndActor != autoActor {
		t.Fatalf("计划已过的生效记录应按到期展示：%+v", x)
	}
	for _, x := range r.Snapshot() {
		if x.ID == 9 && (x.End != "" || x.EndedAt != 0) {
			t.Fatalf("查询不得改动内部记录：%+v", x)
		}
	}
	// 时间再往前走，1 号的计划（now+3600）也过了；永久封禁永远不会（筛选与展示都是）。
	clk.Advance(2 * time.Hour)
	p = r.Query(RecordFilter{State: RecordStateActive})
	if got := recIDs(p); !reflect.DeepEqual(got, []int64{8, 6}) {
		t.Fatalf("got %v", got)
	}
	for _, x := range p.Records {
		if !x.Permanent || x.End != "" || x.EndedAt != 0 || x.EndActor != "" {
			t.Fatalf("永久封禁不得按到期展示：%+v", x)
		}
	}
	p = r.Query(RecordFilter{Search: "198.51.100.1"})
	if len(p.Records) != 1 || p.Records[0].End != EndExpired || p.Records[0].EndedAt != now+3600 {
		t.Fatalf("到期时间取计划到期时间：%+v", p.Records)
	}
	if p.Now != clk.Now().Unix() {
		t.Fatalf("now=%d", p.Now)
	}
}

func TestRecordsQueryPaging(t *testing.T) {
	clk := newFakeClock()
	r, err := OpenRecords(RecordsConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now().Unix()
	ip := func(i int) string { return fmt.Sprintf("198.51.100.%d", i) }
	for i := 1; i <= 120; i++ { // id 与 i 相同；i 是 3 的倍数的为手动
		reason := "auto"
		if i%3 == 0 {
			reason = "manual"
		}
		r.start(Record{Target: TargetIP, Key: ip(i), Reason: reason, Actor: "op", Since: now - int64(1000-i), Until: now + 3600})
	}
	desc := func(from, to int64) []int64 {
		out := []int64{}
		for id := from; id >= to; id-- {
			out = append(out, id)
		}
		return out
	}

	p1 := r.Query(RecordFilter{Limit: 50})
	if got := recIDs(p1); !reflect.DeepEqual(got, desc(120, 71)) || p1.Total != 120 || p1.Start != 0 || !p1.HasNext || p1.NextCursor != 71 || p1.Limit != 50 {
		t.Fatalf("第 1 页：ids=%v total=%d start=%d has_next=%v next=%d", got, p1.Total, p1.Start, p1.HasNext, p1.NextCursor)
	}
	// 翻页之间：第 1 页上的一条结束、又来了一条新封禁。游标按 id，下一页不受影响。
	r.end(TargetIP, ip(100), banRef{since: now - 900, until: now + 3600}, EndUnban, now, "op")
	r.start(Record{Target: TargetIP, Key: "203.0.113.1", Reason: "auto", Actor: autoActor, Since: now, Until: now + 3600})
	p2 := r.Query(RecordFilter{Limit: 50, Cursor: p1.NextCursor})
	if got := recIDs(p2); !reflect.DeepEqual(got, desc(70, 21)) || p2.Total != 121 || p2.Start != 51 || !p2.HasNext || p2.NextCursor != 21 {
		t.Fatalf("第 2 页：ids=%v total=%d start=%d has_next=%v next=%d", got, p2.Total, p2.Start, p2.HasNext, p2.NextCursor)
	}
	p3 := r.Query(RecordFilter{Limit: 50, Cursor: p2.NextCursor})
	if got := recIDs(p3); !reflect.DeepEqual(got, desc(20, 1)) || p3.Start != 101 || p3.HasNext || p3.NextCursor != 0 {
		t.Fatalf("第 3 页：ids=%v start=%d has_next=%v next=%d", got, p3.Start, p3.HasNext, p3.NextCursor)
	}
	if p := r.Query(RecordFilter{Limit: 50, Cursor: 1}); len(p.Records) != 0 || p.Total != 121 || p.Start != 121 || p.HasNext {
		t.Fatalf("游标之后没有记录：%+v", p)
	}
	// 恰好取完：最后一页满页时不报还有下一页。
	if p := r.Query(RecordFilter{Limit: 20, Cursor: 21}); len(p.Records) != 20 || p.HasNext || p.NextCursor != 0 {
		t.Fatalf("恰好取完：len=%d has_next=%v next=%d", len(p.Records), p.HasNext, p.NextCursor)
	}

	// 带筛选翻页：手动的 40 条（3 的倍数），每页 25。
	m1 := r.Query(RecordFilter{Reason: "manual", Limit: 25})
	if len(m1.Records) != 25 || m1.Records[0].ID != 120 || m1.Records[24].ID != 48 || m1.Total != 40 || !m1.HasNext || m1.NextCursor != 48 {
		t.Fatalf("筛选第 1 页：ids=%v total=%d next=%d", recIDs(m1), m1.Total, m1.NextCursor)
	}
	m2 := r.Query(RecordFilter{Reason: "manual", Limit: 25, Cursor: m1.NextCursor})
	if len(m2.Records) != 15 || m2.Records[0].ID != 45 || m2.Records[14].ID != 3 || m2.Start != 25 || m2.HasNext {
		t.Fatalf("筛选第 2 页：ids=%v start=%d has_next=%v", recIDs(m2), m2.Start, m2.HasNext)
	}
}

func TestRecordsQueryLimitDefaults(t *testing.T) {
	clk := newFakeClock()
	r, err := OpenRecords(RecordsConfig{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now().Unix()
	for i := 0; i < 250; i++ {
		key := fmt.Sprintf("198.51.100.%d", i)
		if i >= 200 {
			key = fmt.Sprintf("203.0.113.%d", i-200)
		}
		r.start(Record{Target: TargetIP, Key: key, Reason: "manual", Actor: "op", Since: now, Permanent: true})
	}
	if p := r.Query(RecordFilter{}); len(p.Records) != DefaultPageLimit || p.Limit != DefaultPageLimit || !p.HasNext {
		t.Fatalf("默认每页 %d：len=%d limit=%d", DefaultPageLimit, len(p.Records), p.Limit)
	}
	if p := r.Query(RecordFilter{Limit: 1000}); len(p.Records) != MaxPageLimit || p.Limit != MaxPageLimit || !p.HasNext || p.Total != 250 {
		t.Fatalf("上限 %d：len=%d limit=%d total=%d", MaxPageLimit, len(p.Records), p.Limit, p.Total)
	}
}

// 返回的是副本：改返回值里的切片不影响内部记录；空结果的 records 是空数组而不是 null。
func TestRecordsQueryReturnsCopies(t *testing.T) {
	r, _, _ := seedRecordQuery(t)
	p := r.Query(RecordFilter{Search: "office"})
	if len(p.Records) != 1 {
		t.Fatalf("got %v", recIDs(p))
	}
	p.Records[0].Trigger[0].Count = 999
	p.Records[0].Clients[0].Host = "tampered"
	again := r.Query(RecordFilter{Search: "office"})
	if len(again.Records) != 1 || again.Records[0].Trigger[0].Count != 20 || again.Records[0].Clients[0].Host != "Office-PC" {
		t.Fatalf("查询结果与内部记录共享了内存：%+v", again.Records)
	}
	if p := r.Query(RecordFilter{Search: "no-such-thing"}); p.Records == nil || p.Total != 0 {
		t.Fatalf("空结果：%+v", p)
	}
	var nilRecs *Records
	if p := nilRecs.Query(RecordFilter{}); p.Records == nil || len(p.Records) != 0 || p.Total != 0 || p.Limit != DefaultPageLimit {
		t.Fatalf("nil Records：%+v", p)
	}
}

// 页面带上保留规则与是否持久化，控制台据此提示。
func TestRecordsQueryPageMeta(t *testing.T) {
	clk := newFakeClock()
	r, err := OpenRecords(RecordsConfig{Now: clk.Now, MaxRecords: 123, Retention: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Query(RecordFilter{})
	if p.Persistent || p.MaxRecords != 123 || p.RetentionDays != 30 || p.Now != clk.Now().Unix() {
		t.Fatalf("got %+v", p)
	}
	env := newRecEnv(t)
	env.open()
	if p := env.r.Query(RecordFilter{}); !p.Persistent || p.MaxRecords != DefaultMaxRecords || p.RetentionDays != 90 {
		t.Fatalf("持久化：%+v", p)
	}
}

// List.Records 返回名单共用的封禁记录，控制台经由它查询；没配置时为 nil。
func TestListRecordsAccessor(t *testing.T) {
	clk := newFakeClock()
	l, r := openRecMem(t, clk, nil, RecordsConfig{})
	if l.Records() != r {
		t.Fatal("List.Records 应返回配置里的 Records")
	}
	if openMem(t, clk).Records() != nil {
		t.Fatal("没配置记录时应为 nil")
	}
	var nilList *List
	if nilList.Records() != nil {
		t.Fatal("nil List 应返回 nil")
	}
	// 经由名单产生的记录能查到：自动封禁、手动封禁后解禁。
	autoBan(t, l, "198.51.100.31")
	if _, err := l.Ban("198.51.100.32", time.Hour, false, "op", "手动"); err != nil {
		t.Fatal(err)
	}
	if err := l.Unban("198.51.100.32", "op", "解禁", nil); err != nil {
		t.Fatal(err)
	}
	p := l.Records().Query(RecordFilter{})
	if len(p.Records) != 2 || p.Records[0].Key != "198.51.100.32" || p.Records[0].End != EndUnban || p.Records[1].Key != "198.51.100.31" || p.Records[1].End != "" {
		t.Fatalf("got %+v", p.Records)
	}
}
