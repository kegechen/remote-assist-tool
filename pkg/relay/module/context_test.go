package module

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestContext(maxTasks int, onPanic func(string, any)) *moduleContext {
	mc := newModuleContext("audit", NewConfig("audit", 1, []byte(`{"k":"v"}`)), SharedInfo{RelayVersion: "v1.0.0"}, maxTasks, onPanic)
	mc.logf = func(string, ...any) {}
	return mc
}

func TestModuleContextReadOnlyAccessors(t *testing.T) {
	mc := newTestContext(0, nil)
	if mc.ModuleID() != "audit" || mc.Shared().RelayVersion != "v1.0.0" {
		t.Fatalf("unexpected accessors: %s %+v", mc.ModuleID(), mc.Shared())
	}
	b := mc.Config().Bytes()
	b[0] = 'X'
	if string(mc.Config().Bytes()) != `{"k":"v"}` {
		t.Fatal("Config() leaked a mutable alias")
	}
	var lines []string
	mc.logf = func(format string, args ...any) { lines = append(lines, format) }
	mc.Logf("hello %d", 1)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "module %s") {
		t.Fatalf("log not prefixed: %v", lines)
	}
}

func TestModuleContextTaskLimit(t *testing.T) {
	mc := newTestContext(2, nil)
	release := make(chan struct{})
	for i := 0; i < 2; i++ {
		if err := mc.Go("t", func(ctx context.Context) { <-release }); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
	}
	if err := mc.Go("t", func(ctx context.Context) {}); !errors.Is(err, ErrTooManyTasks) {
		t.Fatalf("expected ErrTooManyTasks, got %v", err)
	}
	close(release)
	if err := mc.close(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := mc.Go("t", func(ctx context.Context) {}); !errors.Is(err, ErrContextClosed) {
		t.Fatalf("expected ErrContextClosed, got %v", err)
	}
	if err := mc.Go("", func(ctx context.Context) {}); err == nil {
		t.Fatal("unnamed task must be rejected")
	}
}

func TestModuleContextCloseCancelsTasks(t *testing.T) {
	mc := newTestContext(0, nil)
	var exited atomic.Bool
	if err := mc.Go("loop", func(ctx context.Context) {
		<-ctx.Done()
		exited.Store(true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := mc.close(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !exited.Load() || mc.runningTasks() != 0 {
		t.Fatal("task not cancelled and awaited")
	}
	select {
	case <-mc.Done():
	default:
		t.Fatal("Done not closed after close")
	}
	// 重复 close 安全。
	if err := mc.close(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestModuleContextCloseTimeout(t *testing.T) {
	mc := newTestContext(0, nil)
	release := make(chan struct{})
	defer close(release)
	if err := mc.Go("stuck", func(ctx context.Context) { <-release }); err != nil {
		t.Fatal(err)
	}
	if err := mc.close(contextWithTimeout(t, 20*time.Millisecond)); !errors.Is(err, ErrTasksNotStopped) {
		t.Fatalf("expected ErrTasksNotStopped, got %v", err)
	}
}

func TestModuleContextTaskPanicIsolated(t *testing.T) {
	var mu sync.Mutex
	var gotTask string
	mc := newTestContext(1, func(task string, r any) {
		mu.Lock()
		gotTask = task
		mu.Unlock()
	})
	if err := mc.Go("boom", func(ctx context.Context) { panic("bad") }); err != nil {
		t.Fatal(err)
	}
	if err := mc.close(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotTask != "boom" || mc.runningTasks() != 0 {
		t.Fatalf("panic not reported or slot leaked: %q %d", gotTask, mc.runningTasks())
	}
}

func contextWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestModuleContextPanicInCallbackStillReleasesSlot(t *testing.T) {
	mc := newTestContext(1, func(string, any) { panic("callback broken") })
	mc.logf = func(string, ...any) { panic("logger broken") }
	if err := mc.Go("boom", func(ctx context.Context) { panic("bad") }); err != nil {
		t.Fatal(err)
	}
	// 槽位被释放后同一上限下可以再提交任务。
	deadline := time.Now().Add(time.Second)
	for mc.runningTasks() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := mc.Go("again", func(ctx context.Context) {}); err != nil {
		t.Fatalf("slot not released after panic: %v", err)
	}
	if err := mc.close(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestModuleContextGoRacingClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		mc := newTestContext(MaxTasksLimit, nil)
		var started, accepted atomic.Int32
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := mc.Go("t", func(ctx context.Context) {
					started.Add(1)
					<-ctx.Done()
				})
				if err == nil {
					accepted.Add(1)
				} else if !errors.Is(err, ErrContextClosed) {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		if err := mc.close(contextWithTimeout(t, time.Second)); err != nil {
			t.Fatalf("close: %v", err)
		}
		wg.Wait()
		// 被接受的任务都已被等待退出；之后再提交一律被拒。
		if mc.runningTasks() != 0 || started.Load() != accepted.Load() {
			t.Fatalf("running=%d started=%d accepted=%d", mc.runningTasks(), started.Load(), accepted.Load())
		}
	}
}

func TestNewTestContext(t *testing.T) {
	mc, closeFn := NewTestContext(NewConfig("audit", 1, nil), SharedInfo{Instance: "default"}, 1)
	if mc.ModuleID() != "audit" || mc.Shared().Instance != "default" {
		t.Fatalf("unexpected test context: %s %+v", mc.ModuleID(), mc.Shared())
	}
	if err := mc.Go("t", func(ctx context.Context) { <-ctx.Done() }); err != nil {
		t.Fatal(err)
	}
	if err := closeFn(contextWithTimeout(t, time.Second)); err != nil {
		t.Fatalf("close: %v", err)
	}
}
