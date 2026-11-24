package blocklist

import (
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// S10：控制台查询（Query）与写盘快照不得长时间持锁。洪泛期间写锁排在 Query 的读锁后面，
// 随后 accept 的读锁又排在写锁后面，Query 在锁内做 O(n log n) 排序会让 accept 停顿数毫秒。
// 这里在持续查询的同时测量写路径（RecordViolation）的等待时间。
func TestQueryDoesNotStallWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试")
	}
	l := newScaleList(t)
	defer l.Close()
	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				l.Query(Filter{Status: "tracked", Limit: DefaultPageLimit})
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	lat := make([]time.Duration, 0, 4000)
	begin := time.Now()
	for i := 0; i < 4000; i++ {
		start := time.Now()
		l.RecordViolation(scaleIP(i%scaleTracked), KindTLSAborted, "eof")
		lat = append(lat, time.Since(start))
	}
	mean := time.Since(begin) / 4000
	stop.Store(true)
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p99, max := lat[len(lat)*99/100], lat[len(lat)-1]
	// Windows 上单次计时分辨率约 0.5ms，均值用总耗时/次数计算更可信。
	t.Logf("持续 Query(tracked) 时 RecordViolation：均值 %s，p50=%s p99=%s max=%s", mean, lat[len(lat)/2], p99, max)
	if p99 > 2*time.Millisecond {
		t.Fatalf("Query 持锁过久：写路径 p99 等待 %s（> 2ms）", p99)
	}
}
