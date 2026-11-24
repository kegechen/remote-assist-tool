package blocklist

import (
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// newScaleListCJK 与 newScaleList 同规模，但主机名和备注是中文：中文走 ToLower 路径，会分配内存。
func newScaleListCJK(tb testing.TB) *List {
	tb.Helper()
	cfg := DefaultConfig()
	cfg.MaxTracked = MaxTrackedLimit
	l, err := Open(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < scaleTracked; i++ {
		ip := scaleIP(i)
		l.ObserveConnect(ip)
		for c := 0; c < maxClientsPerIP; c++ {
			l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", Host: fmt.Sprintf("用户%d@办公电脑 麒麟 arm64", c), ClientID: fmt.Sprintf("cid-%d-%d", i, c)})
		}
		l.RecordViolation(ip, KindJoinFailed, "code_invalid")
	}
	for i := 0; i < scaleBanned; i++ {
		if _, err := l.Ban(scaleIP(i), time.Hour, false, "bench", "手动封禁：扫描器来源，持续探测"); err != nil {
			tb.Fatal(err)
		}
	}
	for i := scaleBanned; i < scaleBanned+scaleAllowed; i++ {
		if err := l.Allow(scaleIP(i), 0, true, "bench", "运维白名单：内网巡检"); err != nil {
			tb.Fatal(err)
		}
	}
	return l
}

// B-N3：搜索在读锁内对每个条目做小写转换并分配内存，中文备注、主机名下读锁持有 12–14ms，
// 期间写锁与排在写锁后的 CheckAccept 都被挡住。修复：containsFold 不再分配；扫描分块取读锁，
// 单次持锁时间与表大小无关。
// 这里在持续搜索的同时，测写锁请求方（它被读锁挡住的时间就是读锁的最长持有时间）。
func TestSearchQueryHoldsReadLockBriefly(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试")
	}
	l := newScaleListCJK(t)
	defer l.Close()
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			// 不命中任何条目：全表都要比较，是持锁最长的情形。
			l.Query(Filter{Status: "tracked", Search: "不存在的词条", Limit: DefaultPageLimit})
		}
	}()
	time.Sleep(20 * time.Millisecond)
	waits := make([]time.Duration, 0, 300)
	for i := 0; i < 300; i++ {
		start := time.Now()
		l.mu.Lock()
		waits = append(waits, time.Since(start))
		l.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	stop.Store(true)
	<-done
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	p50, p99 := waits[len(waits)/2], waits[len(waits)*99/100]
	t.Logf("持续搜索时写锁等待：p50=%s p99=%s max=%s", p50, p99, waits[len(waits)-1])
	if p99 > 5*time.Millisecond {
		t.Fatalf("搜索持读锁过久：写锁等待 p99=%s（> 5ms；p50=%s）", p99, p50)
	}
}

// 搜索结果在拆成「锁内取快照、锁外匹配」之后必须与原语义一致：IP、主机名、版本、生效中的备注，
// 不区分大小写、含中文。
func TestSearchMatchesSnapshotSemantics(t *testing.T) {
	l := newScaleListCJK(t)
	defer l.Close()
	count := func(q string) int { return l.Query(Filter{Status: "tracked", Search: q, Limit: MaxPageLimit}).Total }
	all := count("用户3@办公")
	t.Logf("all=%d host=%d version=%d ban=%d allow=%d", count(""), all, count("V1.1.0"), count("扫描器来源"), count("内网巡检"))
	if got := count("用户3@办公"); got != all {
		t.Fatalf("host search: %d", got)
	}
	if got := count("V1.1.0"); got != all {
		t.Fatalf("version search (case-insensitive): %d", got)
	}
	if got := count("扫描器来源"); got != scaleBanned {
		t.Fatalf("ban note search: %d", got)
	}
	if got := count("内网巡检"); got != scaleAllowed {
		t.Fatalf("allow note search: %d", got)
	}
	if got := count("10.0.0.5"); got < 1 {
		t.Fatalf("ip search: %d", got)
	}
	if got := count("不存在"); got != 0 {
		t.Fatalf("miss: %d", got)
	}
}

func TestContainsFold(t *testing.T) {
	cases := []struct {
		s, q string
		want bool
	}{
		{"Alice@PC Windows 11", "windows", true},
		{"用户3@办公电脑 麒麟 ARM64", "arm64", true}, // ASCII 查询词，s 含中文：不分配也要忽略大小写
		{"用户3@办公电脑 麒麟 arm64", "办公电脑", true},  // 中文查询词：逐字节匹配
		{"用户3@办公电脑", "办公室", false},
		{"ÉCOLE", "école", true}, // 含大小写的非 ASCII：退回 ToLower
		{"Привет", "привет", true},
		{"abc", "", true},
		{"ab", "abc", false},
	}
	for _, c := range cases {
		if got := containsFold(c.s, c.q); got != c.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", c.s, c.q, got, c.want)
		}
	}
	if n := testing.AllocsPerRun(100, func() {
		containsFold("用户3@办公电脑 麒麟 ARM64", "arm64")
		containsFold("用户3@办公电脑 麒麟", "办公")
	}); n != 0 {
		t.Errorf("containsFold allocates %v times in the common cases", n)
	}
}
