package relay

import (
	"fmt"
	"testing"
)

func TestAcquireConnSlotPerIPLimit(t *testing.T) {
	s := &Server{connPerIP: make(map[string]int)}
	ip := "1.2.3.4"

	for i := 0; i < maxConnsPerIP; i++ {
		if !s.acquireConnSlot(ip) {
			t.Fatalf("acquire #%d should succeed (under per-IP limit)", i)
		}
	}
	if s.acquireConnSlot(ip) {
		t.Fatal("acquire beyond per-IP limit should fail")
	}
	// 另一个 IP 不受同一 IP 的限额影响
	if !s.acquireConnSlot("5.6.7.8") {
		t.Fatal("different IP should not be limited by another IP's count")
	}
	// 释放一个名额后该 IP 可再次获取
	s.releaseConnSlot(ip)
	if !s.acquireConnSlot(ip) {
		t.Fatal("acquire after release should succeed")
	}
}

func TestReleaseConnSlotCleansUpMap(t *testing.T) {
	s := &Server{connPerIP: make(map[string]int)}
	ip := "1.2.3.4"

	s.acquireConnSlot(ip)
	s.releaseConnSlot(ip)

	if _, ok := s.connPerIP[ip]; ok {
		t.Fatal("per-IP entry should be deleted when its count reaches 0")
	}
	if s.connTotal != 0 {
		t.Fatalf("connTotal should be 0 after release, got %d", s.connTotal)
	}
}

func TestAcquireConnSlotTotalLimit(t *testing.T) {
	s := &Server{connPerIP: make(map[string]int)}

	// 用各不相同的 IP 撑满全局上限（每个 IP 只占 1，不触发 per-IP 限额）
	for i := 0; i < maxConnsTotal; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
		if !s.acquireConnSlot(ip) {
			t.Fatalf("acquire #%d should succeed (under total limit)", i)
		}
	}
	if s.acquireConnSlot("200.200.200.200") {
		t.Fatal("acquire beyond global total limit should fail")
	}
}
