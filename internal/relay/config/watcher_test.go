package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

var baseMTime = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// writeConfig 写入内容并显式设置修改时间，避免依赖文件系统时间精度。
func writeConfig(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func cfgWith(value string) string {
	return `{"version":1,"modules":{"audit":{"value":"` + value + `"}}}`
}

func newTestWatcher(t *testing.T) (*Watcher, *Reloader, *recorder, *fakeClock, string) {
	t.Helper()
	r, _, rec, _ := newTestStack(t)
	path := filepath.Join(t.TempDir(), "modules.json")
	w, err := NewWatcher(WatchOptions{Path: path, Debounce: 500 * time.Millisecond, ResyncInterval: -1}, r)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	w.now = clock.now
	w.sleep = func(time.Duration) {}
	w.logf = func(string, ...any) {}
	return w, r, rec, clock, path
}

// pollUntil 推进时钟并轮询，返回第一次真正交给 Reloader 的结果。
func pollUntil(t *testing.T, w *Watcher, clock *fakeClock, step time.Duration, max int) (ReloadReport, int) {
	t.Helper()
	for i := 1; i <= max; i++ {
		if rep, delivered := w.poll(context.Background()); delivered {
			return rep, i
		}
		clock.advance(step)
	}
	t.Fatalf("no delivery after %d polls", max)
	return ReloadReport{}, 0
}

func TestNewWatcherRequiresAbsolutePath(t *testing.T) {
	r, _, _, _ := newTestStack(t)
	for _, p := range []string{"", "modules.json", "./x/modules.json"} {
		if _, err := NewWatcher(WatchOptions{Path: p}, r); err == nil {
			t.Errorf("relative path %q accepted", p)
		}
	}
}

func TestWatcherInitialLoad(t *testing.T) {
	w, r, rec, _, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("init"), baseMTime)
	if rep := w.LoadInitial(context.Background()); rep.Outcome != OutcomeApplied || rep.Revision != 1 {
		t.Fatalf("initial = %+v", rep)
	}
	if got := rec.take(); len(got) != 1 || got[0] != "start:audit:init" {
		t.Fatalf("events = %v", got)
	}
	if r.Current().Source() != path {
		t.Fatalf("source = %s", r.Current().Source())
	}
}

func TestWatcherInitialMissingFile(t *testing.T) {
	w, r, _, _, _ := newTestWatcher(t)
	rep := w.LoadInitial(context.Background())
	if rep.Outcome != OutcomeRejected || !strings.Contains(rep.Err.Error(), "does not exist") || r.Current() != nil {
		t.Fatalf("initial = %+v", rep)
	}
}

func TestWatcherDebouncesChanges(t *testing.T) {
	w, _, rec, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	rec.take()

	writeConfig(t, path, cfgWith("v2"), baseMTime.Add(time.Second))
	if _, delivered := w.poll(context.Background()); delivered {
		t.Fatal("change delivered without debounce")
	}
	clock.advance(300 * time.Millisecond)
	// 防抖窗口内再次写入：重新计时。
	writeConfig(t, path, cfgWith("v3"), baseMTime.Add(2*time.Second))
	if _, delivered := w.poll(context.Background()); delivered {
		t.Fatal("second write delivered immediately")
	}
	clock.advance(300 * time.Millisecond)
	if _, delivered := w.poll(context.Background()); delivered {
		t.Fatal("delivered before the quiet period after the last write")
	}
	clock.advance(300 * time.Millisecond)
	rep, delivered := w.poll(context.Background())
	if !delivered || rep.Outcome != OutcomeApplied || rep.Revision != 2 {
		t.Fatalf("debounced delivery = %+v %v", rep, delivered)
	}
	// 中间版本 v2 从未被应用。
	if got := rec.take(); len(got) != 2 || got[1] != "start:audit:v3" {
		t.Fatalf("events = %v", got)
	}
}

func TestWatcherHandlesAtomicRenameReplace(t *testing.T) {
	w, _, rec, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	rec.take()
	tmp := filepath.Join(filepath.Dir(path), ".modules.json.tmp")
	writeConfig(t, tmp, cfgWith("renamed"), baseMTime.Add(time.Minute))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	rep, _ := pollUntil(t, w, clock, 200*time.Millisecond, 10)
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("rename replace = %+v", rep)
	}
	if got := rec.take(); len(got) != 2 || got[1] != "start:audit:renamed" {
		t.Fatalf("events = %v", got)
	}
}

func TestWatcherWaitsForStableContent(t *testing.T) {
	w, _, rec, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	rec.take()
	writeConfig(t, path, `{"version":1,"modules":{"audit":{"value":"par`, baseMTime.Add(time.Second))
	writes := 0
	w.sleep = func(time.Duration) {
		// 两次读取之间文件仍在被写：第一次稳定读取必然失败。
		if writes == 0 {
			writes++
			writeConfig(t, path, cfgWith("complete"), baseMTime.Add(2*time.Second))
		}
	}
	rep, polls := pollUntil(t, w, clock, 200*time.Millisecond, 20)
	if rep.Outcome != OutcomeApplied || polls < 4 {
		t.Fatalf("delivery = %+v after %d polls", rep, polls)
	}
	if got := rec.take(); len(got) != 2 || got[1] != "start:audit:complete" {
		t.Fatalf("partial content applied: %v", got)
	}
}

func TestWatcherMissingFileKeepsConfigUntilRecreated(t *testing.T) {
	w, r, rec, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	rec.take()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rep, _ := pollUntil(t, w, clock, 200*time.Millisecond, 10)
	if rep.Outcome != OutcomeRejected || r.Current().Revision() != 1 || len(rec.take()) != 0 {
		t.Fatalf("missing file = %+v", rep)
	}
	writeConfig(t, path, cfgWith("v2"), baseMTime.Add(time.Hour))
	rep, _ = pollUntil(t, w, clock, 200*time.Millisecond, 10)
	if rep.Outcome != OutcomeApplied || rep.Revision != 2 {
		t.Fatalf("recreated = %+v", rep)
	}
}

func TestWatcherTouchWithoutContentChangeIsUnchanged(t *testing.T) {
	w, _, rec, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	rec.take()
	if err := os.Chtimes(path, baseMTime.Add(time.Hour), baseMTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rep, _ := pollUntil(t, w, clock, 200*time.Millisecond, 10)
	if rep.Outcome != OutcomeUnchanged || len(rec.take()) != 0 {
		t.Fatalf("touch = %+v", rep)
	}
}

func TestWatcherResyncCatchesSameMetadataChange(t *testing.T) {
	w, _, rec, clock, path := newTestWatcher(t)
	w.opts.ResyncInterval = time.Minute
	writeConfig(t, path, cfgWith("aa"), baseMTime)
	w.LoadInitial(context.Background())
	rec.take()
	// 同大小、同修改时间的改写：元数据看不出变化。
	writeConfig(t, path, cfgWith("bb"), baseMTime)
	clock.advance(30 * time.Second)
	if _, delivered := w.poll(context.Background()); delivered {
		t.Fatal("resync ran before its interval")
	}
	clock.advance(31 * time.Second)
	rep, delivered := w.poll(context.Background())
	if !delivered || rep.Outcome != OutcomeApplied {
		t.Fatalf("resync = %+v %v", rep, delivered)
	}
}

func TestWatcherRejectsOversizedFile(t *testing.T) {
	w, r, _, _, path := newTestWatcher(t)
	writeConfig(t, path, `{"version":1,"modules":{"audit":{"value":"`+strings.Repeat("x", MaxFileBytes)+`"}}}`, baseMTime)
	rep := w.LoadInitial(context.Background())
	if rep.Outcome != OutcomeRejected || !strings.Contains(rep.Err.Error(), "exceeds") || r.Current() != nil {
		t.Fatalf("oversized = %+v", rep)
	}
}

func TestWatcherRunLoop(t *testing.T) {
	r, _, rec, _ := newTestStack(t)
	path := filepath.Join(t.TempDir(), "modules.json")
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w, err := NewWatcher(WatchOptions{Path: path, PollInterval: 10 * time.Millisecond, Debounce: 30 * time.Millisecond, StableDelay: time.Millisecond}, r)
	if err != nil {
		t.Fatal(err)
	}
	w.logf = func(string, ...any) {}
	w.LoadInitial(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	writeConfig(t, path, cfgWith("v2"), baseMTime.Add(time.Second))
	deadline := time.Now().Add(3 * time.Second)
	for r.Status().Revision != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if r.Status().Revision != 2 {
		t.Fatalf("run loop did not apply the change: %+v", r.Status())
	}
	if got := rec.take(); len(got) != 3 || got[2] != "start:audit:v2" {
		t.Fatalf("events = %v", got)
	}
}

func TestWatcherRollbackIsNotUndoneByResync(t *testing.T) {
	w, r, rec, clock, path := newTestWatcher(t)
	w.opts.ResyncInterval = time.Minute
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	writeConfig(t, path, cfgWith("v2"), baseMTime.Add(time.Second))
	if rep, _ := pollUntil(t, w, clock, 200*time.Millisecond, 10); rep.Revision != 2 {
		t.Fatalf("v2 = %+v", rep)
	}
	if rep := r.Rollback(context.Background(), 1); rep.Outcome != OutcomeApplied || rep.Revision != 3 {
		t.Fatalf("rollback = %+v", rep)
	}
	rec.take()
	clock.advance(2 * time.Minute)
	rep, delivered := w.poll(context.Background())
	if !delivered || rep.Outcome != OutcomeUnchanged || rep.Revision != 3 || len(rec.take()) != 0 {
		t.Fatalf("resync undid the rollback: %+v %v", rep, delivered)
	}
	if !r.Status().RollbackPinned {
		t.Fatal("status must report the pinned rollback")
	}
	// 文件真正变化后恢复自动应用。
	writeConfig(t, path, cfgWith("v3"), baseMTime.Add(2*time.Second))
	rep, _ = pollUntil(t, w, clock, 200*time.Millisecond, 10)
	if rep.Outcome != OutcomeApplied || rep.Revision != 4 || r.Status().RollbackPinned {
		t.Fatalf("after file change = %+v", rep)
	}
}

func TestWatcherInitialUnstableIsPendingNotRejected(t *testing.T) {
	w, r, _, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	n := 0
	w.sleep = func(time.Duration) {
		n++
		writeConfig(t, path, cfgWith(strings.Repeat("x", n)), baseMTime.Add(time.Duration(n)*time.Second))
	}
	rep := w.LoadInitial(context.Background())
	if rep.Outcome != OutcomePending || r.Status().LastOutcome != "" {
		t.Fatalf("initial = %+v status %+v", rep, r.Status())
	}
	w.sleep = func(time.Duration) {}
	rep, _ = pollUntil(t, w, clock, 200*time.Millisecond, 10)
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("later = %+v", rep)
	}
}

func TestWatcherReportsLongUnstablePeriodOnce(t *testing.T) {
	w, r, _, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	n := 0
	w.sleep = func(time.Duration) {
		n++
		writeConfig(t, path, cfgWith(strings.Repeat("y", n%3+1)), baseMTime.Add(time.Duration(n)*time.Second))
	}
	writeConfig(t, path, cfgWith("start"), baseMTime.Add(time.Hour))
	reports := 0
	for i := 0; i < 150; i++ {
		if rep, delivered := w.poll(context.Background()); delivered {
			reports++
			if rep.Outcome != OutcomeRejected || !strings.Contains(rep.Err.Error(), "changing or locked") {
				t.Fatalf("report = %+v", rep)
			}
		}
		clock.advance(time.Second)
	}
	if reports != 1 || r.Current().Revision() != 1 {
		t.Fatalf("reports = %d, revision = %d", reports, r.Current().Revision())
	}
}

func TestWatcherReportsContinuousRewritesOnce(t *testing.T) {
	w, r, _, clock, path := newTestWatcher(t)
	writeConfig(t, path, cfgWith("v1"), baseMTime)
	w.LoadInitial(context.Background())
	reports := 0
	for i := 0; i < 150; i++ {
		// 每 300ms 改写一次，始终达不到 500ms 的静默期。
		writeConfig(t, path, cfgWith(strings.Repeat("z", i%3+1)), baseMTime.Add(time.Duration(i+1)*time.Second))
		if rep, delivered := w.poll(context.Background()); delivered {
			reports++
			if rep.Outcome != OutcomeRejected {
				t.Fatalf("report = %+v", rep)
			}
		}
		clock.advance(300 * time.Millisecond)
		w.poll(context.Background())
		clock.advance(300 * time.Millisecond)
	}
	if reports != 1 || r.Current().Revision() != 1 {
		t.Fatalf("reports = %d, revision = %d", reports, r.Current().Revision())
	}
}
