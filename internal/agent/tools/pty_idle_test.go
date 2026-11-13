package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// fakeClock 可手动拨动的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// idleHarness 起一个带假时钟的会话，返回 open 结果通道。
func idleHarness(t *testing.T, timeout time.Duration) (*PTYManager, *ptyWriteTool, *fakeClock, *[]*fakePTY, chan ptyOpenResult) {
	t.Helper()
	m, open, write, _, _, made := newFakeTools(t, false)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	m.now = clk.Now
	m.idleTimeout = timeout
	m.idleTick = 5 * time.Millisecond
	done := make(chan ptyOpenResult, 1)
	go func() {
		res, _ := open.Run(context.Background(), json.RawMessage(`{"session":"i1"}`), &ptyCaptureSink{})
		var out ptyOpenResult
		json.Unmarshal(res, &out)
		done <- out
	}()
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")
	return m, write, clk, made, done
}

func expectIdleClosed(t *testing.T, done chan ptyOpenResult) {
	t.Helper()
	select {
	case out := <-done:
		if out.Reason != "idle" {
			t.Fatalf("reason=%q 期望 idle", out.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("空闲超时后会话未被回收")
	}
}

func expectAlive(t *testing.T, m *PTYManager, done chan ptyOpenResult) {
	t.Helper()
	time.Sleep(60 * time.Millisecond) // 超过若干个检查周期
	select {
	case out := <-done:
		t.Fatalf("会话不应被回收，却结束了: %+v", out)
	default:
	}
	if m.Count() != 1 {
		t.Fatal("会话应仍存活")
	}
}

func TestPTYIdleReapedWhenNoInputNoOutput(t *testing.T) {
	_, _, clk, _, done := idleHarness(t, time.Hour)
	clk.Advance(time.Hour + time.Minute)
	expectIdleClosed(t, done)
}

func TestPTYIdleRenewedByInput(t *testing.T) {
	m, write, clk, _, done := idleHarness(t, time.Hour)
	clk.Advance(50 * time.Minute)
	args, _ := json.Marshal(map[string]any{"session": "i1", "data": []byte("x")})
	if _, err := write.Run(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}
	clk.Advance(50 * time.Minute) // 距上次输入仅 50 分钟（回显输出也在此刻刷新）
	expectAlive(t, m, done)
	clk.Advance(61 * time.Minute) // 无输入无输出
	expectIdleClosed(t, done)
}

// 长时间无输入但持续有输出（tail -f / 编译）不得被误杀。
func TestPTYIdleRenewedByOutput(t *testing.T) {
	m, _, clk, made, done := idleHarness(t, time.Hour)
	f := (*made)[0]
	for i := 0; i < 3; i++ {
		clk.Advance(40 * time.Minute)
		go f.outW.Write([]byte("log line\n"))
		time.Sleep(60 * time.Millisecond) // 让读协程消化并刷新活动时间
	}
	expectAlive(t, m, done) // 累计 2 小时，但每 40 分钟都有输出
	clk.Advance(61 * time.Minute)
	expectIdleClosed(t, done)
}

func TestPTYIdleDisabled(t *testing.T) {
	m, _, clk, _, done := idleHarness(t, 0)
	clk.Advance(48 * time.Hour)
	expectAlive(t, m, done)
}

func TestPTYIdleTimeoutFromEnv(t *testing.T) {
	cases := map[string]time.Duration{
		"": defaultPTYIdleTimeout, "30m": 30 * time.Minute, "0": 0, "off": 0, "garbage": defaultPTYIdleTimeout, "-5m": defaultPTYIdleTimeout,
	}
	for in, want := range cases {
		if got := ptyIdleTimeoutFromEnv(func(string) string { return in }); got != want {
			t.Errorf("env=%q 得到 %v 期望 %v", in, got, want)
		}
	}
}
