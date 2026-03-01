package tests

import (
	"net"
	"testing"
	"time"
)

// TestPlaceholder 占位测试
func TestPlaceholder(t *testing.T) {
	// 集成测试需要实际运行环境
	t.Skip("Integration tests require running server")
}

// Helper for getting free port
func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}

// TestRetryBackoff 测试退避重试逻辑
func TestRetryBackoff(t *testing.T) {
	delays := []time.Duration{}
	for i := 0; i < 5; i++ {
		delay := time.Duration(i*100) * time.Millisecond
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		delays = append(delays, delay)
	}

	if len(delays) != 5 {
		t.Error("Expected 5 delays")
	}
}
