package blocklist

import (
	"fmt"
	"testing"
)

// 最终轮 B1：向前翻页越过开头时返回从头开始的满页（不足 limit 时补满），API 自洽：
// start=0、has_prev=false、next_cursor 非空、has_next 与实际一致。
func TestCursorPrevAtStartFillsPage(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	for i := 0; i < 25; i++ {
		l.ObserveConnect(fmt.Sprintf("10.70.0.%d", i+1))
		l.RecordViolation(fmt.Sprintf("10.70.0.%d", i+1), KindJoinFailed, "x")
	}
	first := l.Query(Filter{Status: "all", Limit: 10})
	mid := l.Query(Filter{Status: "all", Limit: 3, Cursor: first.NextCursor, Dir: "next"}) // 第 11–13 条
	p := l.Query(Filter{Status: "all", Limit: 10, Cursor: mid.NextCursor, Dir: "prev"})    // 游标（第 13 条）之前 12 条中的最后 10 条：第 3–12 条
	if p.Start != 2 || len(p.Entries) != 10 {
		t.Fatalf("普通向前翻页: start=%d n=%d", p.Start, len(p.Entries))
	}
	p = l.Query(Filter{Status: "all", Limit: 10, Cursor: p.PrevCursor, Dir: "prev"}) // 之前只有 2 条
	if p.Start != 0 || len(p.Entries) != 10 || p.HasPrev || !p.HasNext || p.NextCursor == "" {
		t.Fatalf("越过开头应返回从头补满的第一页: start=%d n=%d has_prev=%v has_next=%v next=%q", p.Start, len(p.Entries), p.HasPrev, p.HasNext, p.NextCursor)
	}
	p = l.Query(Filter{Status: "all", Limit: 10, Cursor: first.PrevCursor, Dir: "prev"}) // 第一页之前没有条目
	if p.Start != 0 || len(p.Entries) != 10 || p.HasPrev || !p.HasNext || p.NextCursor == "" {
		t.Fatalf("第一页再向前应返回第一页: start=%d n=%d has_prev=%v has_next=%v next=%q", p.Start, len(p.Entries), p.HasPrev, p.HasNext, p.NextCursor)
	}
}
