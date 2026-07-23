package p2p

import "testing"

func TestSTUNCustomLimitsApplied(t *testing.T) {
	limits := DefaultLimits()
	limits.WorkerCount = 1
	limits.TaskQueueDepth = 7
	limits.MaxRelaySessionsTotal = 3
	limits.InvalidLogSampleEvery = 1
	s, err := NewSTUNServerWithValidatorAndLimits("127.0.0.1:0", func(string) bool { return true }, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if cap(s.taskQueue) != 7 || s.limits.WorkerCount != 1 || s.limits.MaxRelaySessionsTotal != 3 {
		t.Fatalf("自定义 UDP limits 未生效: cap=%d limits=%+v", cap(s.taskQueue), s.limits)
	}
}

func TestSTUNRejectsInvalidLimitsBeforeListen(t *testing.T) {
	limits := DefaultLimits()
	limits.WorkerCount = -1
	if _, err := NewSTUNServerWithValidatorAndLimits("127.0.0.1:0", func(string) bool { return true }, limits); err == nil {
		t.Fatal("非法 worker_count 应被拒绝")
	}
}
