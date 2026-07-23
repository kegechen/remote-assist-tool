package ratelimit

import (
	"strconv"
	"testing"
)

func BenchmarkKeyedLimiterFullChurn(b *testing.B) {
	const maxKeys = 1024
	k := NewKeyedLimiter(100, 100, maxKeys, 0)
	for i := 0; i < maxKeys; i++ {
		k.Allow(strconv.Itoa(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k.Allow(strconv.Itoa(maxKeys + i))
	}
}
