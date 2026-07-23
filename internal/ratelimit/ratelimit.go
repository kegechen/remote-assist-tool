// Package ratelimit 提供无外部依赖的令牌桶限流原语，供 relay TCP（join 尝试）与
// p2p UDP（每 IP / 全局 PPS、日志采样）等公网入口做资源耗尽防护。
//
// 设计取舍：
//   - 惰性补充（无后台 goroutine），作为库使用时没有生命周期需要管理，也不会泄漏 goroutine。
//   - KeyedLimiter 自带 key 数上限、LRU 淘汰与空闲淘汰，防止「限流器本身」被海量伪造 key
//     （如伪造源 IP）撑爆内存——这类防护件若自身可被无界扩张就失去意义。
package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// Bucket 经典令牌桶：容量 burst，每秒补充 rate 个令牌，惰性补充。并发安全。
// 零值不可用，必须经 NewBucket 创建。
type Bucket struct {
	mu     sync.Mutex
	rate   float64 // 每秒补充令牌数
	burst  float64 // 桶容量上限
	tokens float64 // 当前令牌数
	last   time.Time
	nowFn  func() time.Time // 可注入时钟，便于测试；默认 time.Now
}

// NewBucket 创建一个初始装满（tokens = burst）的令牌桶。
// ratePerSec <= 0 表示永不补充（一次性 burst 配额）；burst <= 0 表示永远拒绝。
func NewBucket(ratePerSec, burst float64) *Bucket {
	return &Bucket{
		rate:   ratePerSec,
		burst:  burst,
		tokens: burst,
		last:   time.Now(),
		nowFn:  time.Now,
	}
}

// Allow 消耗一个令牌，成功返回 true，令牌不足返回 false。
func (b *Bucket) Allow() bool { return b.AllowN(1) }

// AllowN 消耗 n 个令牌，成功返回 true，令牌不足返回 false（不扣减）。
func (b *Bucket) AllowN(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.nowFn()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens >= n {
		b.tokens -= n
		return true
	}
	return false
}

// keyedBucket 是 KeyedLimiter 内的单 key 桶，附带最近活跃时间用于空闲淘汰。
type keyedBucket struct {
	key        string
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
	element    *list.Element
}

// KeyedLimiter 为每个 key（通常是源 IP）维护独立令牌桶，实现「每 key」限流。
// 自带 key 数上限（maxKeys）与空闲淘汰（idleExpiry），防止限流器自身内存膨胀。
type KeyedLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*keyedBucket
	lru        *list.List // front=最近访问，back=最久未访问
	rate       float64
	burst      float64
	maxKeys    int
	idleExpiry time.Duration
	nowFn      func() time.Time
}

// NewKeyedLimiter 创建按 key 限流器。每个 key 各自 ratePerSec/burst；
// buckets 总数不超过 maxKeys；表满时优先淘汰空闲 key，否则复用 LRU 桶并保留其
// 令牌状态，避免攻击者填满 key 表后永久拒绝所有新来源，也避免轮换 key 刷新 burst。
func NewKeyedLimiter(ratePerSec, burst float64, maxKeys int, idleExpiry time.Duration) *KeyedLimiter {
	return &KeyedLimiter{
		buckets:    make(map[string]*keyedBucket),
		lru:        list.New(),
		rate:       ratePerSec,
		burst:      burst,
		maxKeys:    maxKeys,
		idleExpiry: idleExpiry,
		nowFn:      time.Now,
	}
}

// Allow 对给定 key 消耗一个令牌。
// 若 key 不存在且表已满，则回收 LRU 桶：空闲过期的桶按新来源初始化，仍活跃的桶
// 保留令牌余额与补充时间后换绑到新 key。这样内存有界，且满表不会成为新来源的
// 永久拒绝开关。
func (k *KeyedLimiter) Allow(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()

	now := k.nowFn()
	kb, ok := k.buckets[key]
	if !ok {
		if len(k.buckets) >= k.maxKeys {
			oldest := k.lru.Back()
			if oldest == nil {
				return false
			}
			candidate := oldest.Value.(*keyedBucket)
			delete(k.buckets, candidate.key)
			if now.Sub(candidate.lastSeen) < k.idleExpiry {
				// 活跃桶换绑时继承额度，防止攻击者靠 key churn 获得新的 burst。
				candidate.key = key
				kb = candidate
				k.lru.MoveToFront(oldest)
				k.buckets[key] = candidate
			} else {
				k.lru.Remove(oldest)
			}
		}
		if kb == nil {
			kb = &keyedBucket{
				key:        key,
				tokens:     k.burst,
				lastRefill: now,
				lastSeen:   now,
			}
			kb.element = k.lru.PushFront(kb)
			k.buckets[key] = kb
		}
	} else {
		k.lru.MoveToFront(kb.element)
	}
	kb.lastSeen = now

	if elapsed := now.Sub(kb.lastRefill).Seconds(); elapsed > 0 {
		kb.tokens += elapsed * k.rate
		if kb.tokens > k.burst {
			kb.tokens = k.burst
		}
		kb.lastRefill = now
	}
	if kb.tokens < 1 {
		return false
	}
	kb.tokens--
	return true
}

// Len 返回当前活跃 key 数（主要用于测试与监控）。
func (k *KeyedLimiter) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.buckets)
}
