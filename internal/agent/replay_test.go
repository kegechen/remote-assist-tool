package agent

import (
	"sync"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

func TestReplayGuardFirstUseAndInOrder(t *testing.T) {
	var g replayGuard
	// Bridge 的调用 ID 从 1 起自增，但热切换后也可能从任意值开始，不能假定从 0。
	for _, id := range []uint64{5, 6, 7, 100} {
		if !g.accept(id) {
			t.Fatalf("id=%d 首次出现应被接受", id)
		}
	}
}

func TestReplayGuardRejectsDuplicate(t *testing.T) {
	var g replayGuard
	g.accept(10)
	if g.accept(10) {
		t.Fatal("重复 ID 应被拒绝")
	}
	g.accept(11)
	if g.accept(10) {
		t.Fatal("窗口内的旧 ID 重放应被拒绝")
	}
}

// TestReplayGuardAcceptsOutOfOrderWithinWindow relay⇄P2P 热切换那一瞬会有少量乱序，
// 窗口内迟到的合法请求必须放行，否则用户会看到无缘无故的 replayed。
func TestReplayGuardAcceptsOutOfOrderWithinWindow(t *testing.T) {
	var g replayGuard
	g.accept(5000)
	for _, id := range []uint64{4999, 4998, 4990, 5000 - replayWindowBits + 1} {
		if !g.accept(id) {
			t.Errorf("id=%d 在窗口内且未见过，应被接受", id)
		}
	}
}

func TestReplayGuardRejectsTooOld(t *testing.T) {
	var g replayGuard
	g.accept(replayWindowBits + 500)
	if g.accept(499) {
		t.Fatal("滑出窗口的 ID 应被拒绝（无从判断是否重放，一律拒绝）")
	}
}

// TestReplayGuardClearsSkippedSlots 窗口前移时必须清掉跨过的槽位。位是按 id%1024 存的，
// 不清的话上一轮同余的 ID 会被误判成"见过"，合法请求被当成重放打掉。
func TestReplayGuardClearsSkippedSlots(t *testing.T) {
	var g replayGuard
	g.accept(1)
	g.accept(2)
	// 跨过一整圈但不到一整窗：2 -> 1024+2，此时 id%1024 == 2，槽位与前面的 2 相同。
	if !g.accept(replayWindowBits + 2) {
		t.Fatal("同余但更新的 ID 应被接受")
	}
	// 大跳跃：整窗清空。
	if !g.accept(replayWindowBits*10 + 2) {
		t.Fatal("大跳跃后同余的 ID 应被接受")
	}
}

func TestReplayGuardReset(t *testing.T) {
	var g replayGuard
	g.accept(42)
	g.reset()
	if !g.accept(42) {
		t.Fatal("reset 后应视作全新窗口")
	}
}

func TestReplayGuardConcurrent(t *testing.T) {
	var g replayGuard
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 8 个 goroutine 抢同一个 ID，只能有一个成功。
			if g.accept(7) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("同一 ID 被接受了 %d 次", accepted)
	}
}

// TestDaemonReplayWindowResetOnRekey 重新握手会换 key，此时窗口必须清空——否则新会话
// 从小 ID 重新计数，会撞上旧会话残留的位，第一批请求全被误判成重放。
func TestDaemonReplayWindowResetOnRekey(t *testing.T) {
	f := newSealedFixture(t)
	f.inject(&proto.ToolReq{ID: 1, Tool: "probe", ArgsJSON: f.seal(1, "probe", `{}`)})
	if resp := f.resp(); !resp.OK {
		t.Fatalf("第一次应成功，得到 %+v", resp)
	}

	newKey := proto.DeriveSessionKey("ABCD-2345", "nonceC", "nonceD")
	f.d.RotateKey(newKey)
	f.key = newKey

	// 新会话的 ID 又从 1 开始。
	f.inject(&proto.ToolReq{ID: 1, Tool: "probe", ArgsJSON: f.seal(1, "probe", `{}`)})
	if resp := f.resp(); !resp.OK {
		t.Fatalf("换 key 后同一个 ID 应被接受，得到 %+v", resp)
	}
}

// TestDaemonReplayWindowKeptOnHotUpgrade key 不变的 P2P 热升级不能 reset，否则升级
// 瞬间在途的请求所在的窗口被清空，重放保护出现一个空档。
func TestDaemonReplayWindowKeptOnHotUpgrade(t *testing.T) {
	f := newSealedFixture(t)
	req := &proto.ToolReq{ID: 1, Tool: "probe", ArgsJSON: f.seal(1, "probe", `{}`)}
	f.inject(req)
	if resp := f.resp(); !resp.OK {
		t.Fatalf("第一次应成功，得到 %+v", resp)
	}

	in := make(chan *proto.Message, 4)
	f.d.SwapConn(&fakeConn{in: in, out: f.out}, f.key) // 同一把 key，仅换通道

	f.inject(req)
	resp := f.resp()
	if resp.OK || resp.ErrorCode != "replayed" {
		t.Fatalf("热升级不应清空重放窗口，期望 replayed，得到 %+v", resp)
	}
}
