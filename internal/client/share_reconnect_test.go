package client

import (
	"testing"
	"time"
)

// TestRapidReconnectBackoff 验证防热循环退避决策：
// 正常时长会话清零不退避；建立即断按次数线性退避并封顶。
func TestRapidReconnectBackoff(t *testing.T) {
	// 正常时长会话（≥ 阈值）：重置计数、不退避，保证正常断连快速恢复。
	if rf, d := rapidReconnectBackoff(5*time.Second, 3); rf != 0 || d != 0 {
		t.Fatalf("正常时长会话应清零不退避，得 rf=%d d=%v", rf, d)
	}
	// 恰好等于阈值也算正常。
	if rf, d := rapidReconnectBackoff(rapidReconnectFloor, 7); rf != 0 || d != 0 {
		t.Fatalf("会话存活 == 阈值应视为正常，得 rf=%d d=%v", rf, d)
	}

	// 建立即断：线性递增退避。
	rf, d := rapidReconnectBackoff(100*time.Millisecond, 0)
	if rf != 1 || d != 1*time.Second {
		t.Fatalf("第 1 次快速失败应退避 1s，得 rf=%d d=%v", rf, d)
	}
	rf, d = rapidReconnectBackoff(100*time.Millisecond, rf)
	if rf != 2 || d != 2*time.Second {
		t.Fatalf("第 2 次应退避 2s，得 rf=%d d=%v", rf, d)
	}

	// 退避封顶 reconnectMaxDelay。
	if rf, d := rapidReconnectBackoff(0, 100); d != reconnectMaxDelay || rf != 101 {
		t.Fatalf("应封顶 %v 且计数继续累加，得 rf=%d d=%v", reconnectMaxDelay, rf, d)
	}
}
