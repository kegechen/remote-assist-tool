package blocklist

import (
	"fmt"
	"testing"
	"time"
)

// 规模：1 万个跟踪 IP，其中 1000 个封禁、100 个白名单，每条带满客户端与事件。
const (
	scaleTracked = 9000 // 7900 条可淘汰 + 1100 条钉住，可淘汰部分不超过 MaxTrackedLimit
	scaleBanned  = 1000
	scaleAllowed = 100
)

func scaleIP(i int) string { return fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff) }

func newScaleList(tb testing.TB) *List {
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
			l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", Host: fmt.Sprintf("u%d@pc kylin arm64", c), ClientID: fmt.Sprintf("cid-%d-%d", i, c)})
		}
		for v := 0; v < 5; v++ {
			l.RecordViolation(ip, KindJoinFailed, "code_invalid")
		}
	}
	for i := 0; i < scaleBanned; i++ {
		if _, err := l.Ban(scaleIP(i), time.Hour, false, "bench", ""); err != nil {
			tb.Fatal(err)
		}
	}
	for i := scaleBanned; i < scaleBanned+scaleAllowed; i++ {
		if err := l.Allow(scaleIP(i), 0, true, "bench", ""); err != nil {
			tb.Fatal(err)
		}
	}
	return l
}

// TestScaleTiming 带宽松计时断言的规模测试：accept 查表必须是微秒级，列表接口单页
// 必须远低于人眼可感知的延迟。阈值留了 10 倍以上余量，避免慢机器上误报。
func TestScaleTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试")
	}
	l := newScaleList(t)
	defer l.Close()

	const n = 200000
	start := time.Now()
	for i := 0; i < n; i++ {
		l.CheckAccept(scaleIP(i % scaleTracked))
	}
	perAccept := time.Since(start) / n
	if perAccept > 20*time.Microsecond {
		t.Fatalf("CheckAccept 平均 %s，超过 20µs", perAccept)
	}

	start = time.Now()
	p := l.Query(Filter{Status: "all", Limit: DefaultPageLimit})
	queryAll := time.Since(start)
	if p.Total != scaleTracked || len(p.Entries) != DefaultPageLimit {
		t.Fatalf("Query all total=%d entries=%d", p.Total, len(p.Entries))
	}
	start = time.Now()
	p = l.Query(Filter{Status: "banned", Limit: MaxPageLimit})
	queryBanned := time.Since(start)
	if p.Total != scaleBanned || len(p.Entries) != MaxPageLimit {
		t.Fatalf("Query banned total=%d entries=%d", p.Total, len(p.Entries))
	}
	start = time.Now()
	if _, ok := l.Detail(scaleIP(42)); !ok {
		t.Fatal("Detail 失败")
	}
	detail := time.Since(start)
	t.Logf("规模 %d 跟踪 / %d 封禁：CheckAccept 平均 %s；Query(all, 50) %s；Query(banned, 200) %s；Detail %s",
		scaleTracked, scaleBanned, perAccept, queryAll, queryBanned, detail)
	if queryAll > 200*time.Millisecond || queryBanned > 200*time.Millisecond {
		t.Fatalf("列表查询过慢: all=%s banned=%s", queryAll, queryBanned)
	}
}

func BenchmarkCheckAcceptMiss(b *testing.B) {
	l := newScaleList(b)
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.CheckAccept("192.0.2.77")
	}
}

func BenchmarkCheckAcceptBanned(b *testing.B) {
	l := newScaleList(b)
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.CheckAccept(scaleIP(i % scaleBanned))
	}
}

func BenchmarkCheckAcceptParallel(b *testing.B) {
	l := newScaleList(b)
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.CheckAccept(scaleIP(i % scaleTracked))
			i++
		}
	})
}

func BenchmarkRecordViolation(b *testing.B) {
	l := newScaleList(b)
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.RecordViolation(scaleIP(scaleBanned+scaleAllowed+i%5000), KindTLSGarbage, "eof")
	}
}

func BenchmarkQueryAllPage(b *testing.B) {
	l := newScaleList(b)
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Query(Filter{Status: "all", Limit: DefaultPageLimit})
	}
}

func BenchmarkQueryBannedPage(b *testing.B) {
	l := newScaleList(b)
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Query(Filter{Status: "banned", Limit: DefaultPageLimit})
	}
}

func BenchmarkDetail(b *testing.B) {
	l := newScaleList(b)
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Detail(scaleIP(i % scaleTracked))
	}
}
