package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 提供可控时钟，避免测试依赖真实时间。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBucket(rate, burst float64, clk *fakeClock) *Bucket {
	b := NewBucket(rate, burst)
	b.nowFn = clk.now
	b.last = clk.now()
	b.tokens = burst
	return b
}

func TestBucketBurstThenExhaust(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	b := newTestBucket(1, 3, clk) // 容量 3，每秒补 1

	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("burst token #%d 应放行", i)
		}
	}
	if b.Allow() {
		t.Fatal("耗尽 burst 后应拒绝")
	}
}

func TestBucketRefill(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	b := newTestBucket(2, 2, clk) // 每秒补 2

	if !b.Allow() || !b.Allow() {
		t.Fatal("初始 2 个令牌应放行")
	}
	if b.Allow() {
		t.Fatal("耗尽后应拒绝")
	}
	clk.advance(1 * time.Second) // 补 2 个
	if !b.Allow() || !b.Allow() {
		t.Fatal("补充后应再放行 2 个")
	}
	if b.Allow() {
		t.Fatal("再次耗尽后应拒绝")
	}
}

func TestBucketRefillCappedAtBurst(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	b := newTestBucket(100, 2, clk) // 高补充率但容量仅 2

	clk.advance(10 * time.Second) // 理论补 1000，但封顶 2
	if !b.Allow() || !b.Allow() {
		t.Fatal("封顶后仍应有 2 个令牌")
	}
	if b.Allow() {
		t.Fatal("令牌数不得超过 burst 容量")
	}
}

func TestKeyedLimiterPerKeyIsolation(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	k := NewKeyedLimiter(1, 2, 100, time.Minute)
	k.nowFn = clk.now

	// key A 耗尽不影响 key B
	if !k.Allow("A") || !k.Allow("A") {
		t.Fatal("A 的前 2 个应放行")
	}
	if k.Allow("A") {
		t.Fatal("A 耗尽后应拒绝")
	}
	if !k.Allow("B") || !k.Allow("B") {
		t.Fatal("B 有独立桶，应放行")
	}
}

func TestKeyedLimiterMaxKeysRecyclesActiveLRU(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	k := NewKeyedLimiter(1, 5, 2, time.Hour) // 最多 2 个 key，空闲 1 小时才淘汰
	k.nowFn = clk.now

	if !k.Allow("k1") {
		t.Fatal("k1 应放行")
	}
	if !k.Allow("k2") {
		t.Fatal("k2 应放行")
	}
	// 表已满且无空闲桶时，复用 LRU 桶，避免攻击者把表填满后锁死新来源。
	if !k.Allow("k3") {
		t.Fatal("表满时新 key 应通过复用 LRU 桶获得准入")
	}
	if k.Len() != 2 {
		t.Fatalf("key 数应封顶为 2，实际 %d", k.Len())
	}
	if _, ok := k.buckets["k1"]; ok {
		t.Fatal("最久未使用的 k1 应被 k3 换绑")
	}
	if _, ok := k.buckets["k3"]; !ok {
		t.Fatal("新 key k3 应进入有界表")
	}
}

func TestKeyedLimiterActiveRecycleDoesNotRefreshBurst(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	k := NewKeyedLimiter(0, 1, 1, time.Hour)
	k.nowFn = clk.now

	if !k.Allow("A") {
		t.Fatal("A 应消耗唯一初始令牌")
	}
	if k.Allow("B") {
		t.Fatal("活跃桶换绑到 B 时不得刷新 burst")
	}
	if _, ok := k.buckets["A"]; ok {
		t.Fatal("A 应已从有界表移除")
	}
	if _, ok := k.buckets["B"]; !ok {
		t.Fatal("即使本次额度不足，桶也应换绑到 B")
	}
}

func TestKeyedLimiterEvictsIdleWhenFull(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	k := NewKeyedLimiter(1, 5, 2, 30*time.Second)
	k.nowFn = clk.now

	k.Allow("old1")
	k.Allow("old2")
	clk.advance(31 * time.Second) // old1/old2 变空闲

	// 表满，但触发空闲淘汰后可容纳新 key
	if !k.Allow("fresh") {
		t.Fatal("空闲 key 应被淘汰以腾位给新 key")
	}
	if k.Len() > 2 {
		t.Fatalf("淘汰后 key 数不应超上限，实际 %d", k.Len())
	}
}

func TestKeyedLimiterEvictsDeterministicLRU(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	k := NewKeyedLimiter(1, 5, 2, 30*time.Second)
	k.nowFn = clk.now

	k.Allow("A")
	k.Allow("B")
	clk.advance(20 * time.Second)
	k.Allow("A") // A 变为最近访问，B 仍是 LRU。
	clk.advance(11 * time.Second)
	if !k.Allow("C") {
		t.Fatal("B 空闲超时后应为 C 腾位")
	}
	if _, ok := k.buckets["B"]; ok {
		t.Fatal("满表时应确定性淘汰 LRU B")
	}
	if _, ok := k.buckets["A"]; !ok {
		t.Fatal("最近访问的 A 不应被淘汰")
	}
}

func TestKeyedLimiterConcurrentAllowIsAtomic(t *testing.T) {
	k := NewKeyedLimiter(0, 1, 10, time.Minute)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var allowed int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if k.Allow("same-key") {
				atomic.AddInt32(&allowed, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := atomic.LoadInt32(&allowed); got != 1 {
		t.Fatalf("burst=1 的同 key 并发准入数=%d，期望 1", got)
	}
}
