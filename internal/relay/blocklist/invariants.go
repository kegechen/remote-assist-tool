package blocklist

import (
	"container/list"
	"fmt"
)

// checkInvariants 校验内部结构的一致性，返回第一处不一致。持写锁或读锁调用均可（只读）。
// 供测试（含随机化属性测试）在每一步之后调用；生产路径不调用。
//
// 校验项：
//   - 两条 LRU 中的条目都在主表里、指针一致、lru/elem 回指正确、未被钉住；idle 上的条目没有命中；
//   - 可淘汰条目数 ≤ MaxTracked，主表 ≤ 硬上限；
//   - 自动封禁堆：每个元素都在主表里（没有孤儿）、heapIdx 与位置一致、满足堆序、是生效或待清扫的自动封禁；
//     每个族的堆不超过各自配额；
//   - 主表里每个条目要么在 LRU 上，要么带着封禁/白名单/宽限状态（可能已到期待清扫），不存在"哪里都不在"的条目；
//   - 主表里每个带 heapIdx 的条目确实在对应堆的该位置；
//   - 记忆 LRU 的 map 与链表一致且不超上限；
//   - 开着封禁记录时：每个带封禁的条目恰有一条计划（since、until、permanent）相同的未结束记录，反之亦然（见 Records.checkAgainst）。
func (l *List) checkInvariants(now int64) error {
	inLRU := 0
	for _, ll := range []*list.List{l.idle, l.scoring} {
		for el := ll.Front(); el != nil; el = el.Next() {
			e := el.Value.(*entry)
			inLRU++
			if l.entries[e.IP] != e {
				return fmt.Errorf("LRU 条目 %s 不在主表或指针不一致（孤儿）", e.IP)
			}
			if e.lru != ll || e.elem != el {
				return fmt.Errorf("LRU 条目 %s 的 lru/elem 回指错误", e.IP)
			}
			if e.pinned(now) {
				return fmt.Errorf("钉住条目 %s 仍挂在 LRU 上", e.IP)
			}
			if e.heapIdx >= 0 {
				return fmt.Errorf("LRU 条目 %s 同时在自动封禁堆里", e.IP)
			}
			if ll == l.idle && len(e.hits) > 0 {
				return fmt.Errorf("idle 条目 %s 有命中", e.IP)
			}
		}
	}
	if inLRU > l.cfg.MaxTracked {
		return fmt.Errorf("可淘汰条目 %d 超过 MaxTracked %d", inLRU, l.cfg.MaxTracked)
	}
	if len(l.entries) > l.hardCap() {
		return fmt.Errorf("主表 %d 超过硬上限 %d", len(l.entries), l.hardCap())
	}
	for fam, h := range l.banHeaps() {
		if h.Len() > l.autoBanCap() {
			return fmt.Errorf("族 %d 自动封禁 %d 超过上限 %d", fam, h.Len(), l.autoBanCap())
		}
		for i, e := range *h {
			if l.entries[e.IP] != e {
				return fmt.Errorf("自动封禁堆里的 %s 不在主表（孤儿）", e.IP)
			}
			if e.heapIdx != i {
				return fmt.Errorf("堆条目 %s heapIdx=%d 实际位置 %d", e.IP, e.heapIdx, i)
			}
			if e.Ban == nil || e.Ban.Reason != "auto" {
				return fmt.Errorf("堆条目 %s 不是自动封禁", e.IP)
			}
			if familyOf(e.IP) != fam {
				return fmt.Errorf("堆条目 %s 放错了族", e.IP)
			}
			if i > 0 {
				p := (i - 1) / 2
				if (*h)[p].Ban.Until > e.Ban.Until {
					return fmt.Errorf("堆序被破坏：%s 在 %s 之下", e.IP, (*h)[p].IP)
				}
			}
		}
	}
	for k, e := range l.entries {
		if e.IP != k {
			return fmt.Errorf("主表键 %s 与条目 IP %s 不一致", k, e.IP)
		}
		if e.elem == nil && e.Ban == nil && e.Allow == nil && e.GraceUntil == 0 {
			return fmt.Errorf("条目 %s 既不在 LRU 也不带钉住状态（泄漏）", k)
		}
		if e.Ban != nil && e.Ban.Reason == "auto" && e.Ban.active(now) && e.heapIdx < 0 {
			return fmt.Errorf("生效的自动封禁 %s 不在堆里", k)
		}
		if e.heapIdx >= 0 {
			h := l.banHeaps()[familyOf(e.IP)]
			if e.heapIdx >= h.Len() || (*h)[e.heapIdx] != e {
				return fmt.Errorf("条目 %s 的 heapIdx 指向错误", k)
			}
		}
	}
	for _, m := range l.mems() {
		if m.ll.Len() != len(m.m) {
			return fmt.Errorf("记忆 LRU 链表 %d 与 map %d 不一致", m.ll.Len(), len(m.m))
		}
		if m.ll.Len() > m.cap {
			return fmt.Errorf("记忆 LRU %d 超过上限 %d", m.ll.Len(), m.cap)
		}
		for el := m.ll.Front(); el != nil; el = el.Next() {
			r := el.Value.(*memRec)
			if m.m[r.Key] != el {
				return fmt.Errorf("记忆 %s 的 map 指向错误", r.Key)
			}
		}
	}
	if r := l.cfg.Records; r != nil {
		return r.checkAgainst(l.recordTarget(), l.entries)
	}
	return nil
}

// CheckInvariants 取读锁校验内部一致性（测试与诊断用）。
func (l *List) CheckInvariants() error {
	now := l.now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.checkInvariants(now)
}
