package config

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/pkg/relay/module"
)

func TestReloaderAppliesAndVersionsConfigs(t *testing.T) {
	r, mgr, rec, _ := newTestStack(t)
	ctx := context.Background()
	rep := r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"a1"}}}`), "/cfg.json")
	if rep.Outcome != OutcomeApplied || rep.Revision != 1 || rep.ReloadID != 1 {
		t.Fatalf("first load = %+v", rep)
	}
	if got := rec.take(); !reflect.DeepEqual(got, []string{"start:audit:a1"}) {
		t.Fatalf("events = %v", got)
	}
	// 内容不变：不重复应用。
	if rep := r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"a1"}}}`), "/cfg.json"); rep.Outcome != OutcomeUnchanged || rep.Revision != 1 {
		t.Fatalf("unchanged load = %+v", rep)
	}
	rep = r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"a2"},"notice":{}}}`), "/cfg.json")
	if rep.Outcome != OutcomeApplied || rep.Revision != 2 {
		t.Fatalf("second load = %+v", rep)
	}
	if got := rec.take(); !reflect.DeepEqual(got, []string{"stop:audit", "start:audit:a2", "start:notice:"}) {
		t.Fatalf("events = %v", got)
	}
	st := r.Status()
	if st.Revision != 2 || st.Source != "/cfg.json" || st.LastOutcome != OutcomeApplied || st.LastError != "" || !reflect.DeepEqual(st.History, []uint64{1, 2}) {
		t.Fatalf("status = %+v", st)
	}
	if rev, digest := mgr.Revision(); rev != 2 || digest != r.Current().Digest() {
		t.Fatalf("manager revision = %d %s", rev, digest)
	}
}

func TestReloaderKeepsOldConfigOnErrors(t *testing.T) {
	r, _, rec, logs := newTestStack(t)
	ctx := context.Background()
	r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"good"}}}`), "")
	rec.take()
	bad := []string{
		`{"version":1,"modules":{"audit":`,                   // 格式错误
		`{"version":9,"modules":{}}`,                         // 版本不兼容
		`{"version":1,"modules":{"audit":{"valu":"typo"}}}`,  // 模块参数非法
		`{"version":1,"modules":{"ghost":{}}}`,               // 未注册模块
		`{"version":1,"modules":{"audit":{},"audit":{}}}`,    // 重复键
		`{"version":1,"relay":{"listen":":1"},"modules":{}}`, // 核心段非空
	}
	for _, b := range bad {
		rep := r.Load(ctx, []byte(b), "")
		if rep.Outcome != OutcomeRejected || rep.Revision != 1 || rep.Err == nil {
			t.Fatalf("%s: report = %+v", b, rep)
		}
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("rejected configs touched modules: %v", got)
	}
	st := r.Status()
	if st.Revision != 1 || st.LastOutcome != OutcomeRejected || st.LastError == "" || len(st.History) != 1 {
		t.Fatalf("status = %+v", st)
	}
	// 同一份坏内容再次出现不重复记日志。
	n := len(*logs)
	rep := r.Load(ctx, []byte(bad[len(bad)-1]), "")
	if rep.Outcome != OutcomeRejected || len(*logs) != n {
		t.Fatalf("repeated bad content logged again: %+v", rep)
	}
	// 修好后正常应用。
	if rep := r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"fixed"}}}`), ""); rep.Outcome != OutcomeApplied || rep.Revision != 2 {
		t.Fatalf("fixed load = %+v", rep)
	}
}

func TestReloaderModuleFailureStillCommitsSnapshot(t *testing.T) {
	r, mgr, _, _ := newTestStack(t)
	ctx := context.Background()
	r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"good"}}}`), "")
	rep := r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"bad","fail":true}}}`), "")
	if rep.Outcome != OutcomeApplied || rep.Revision != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if failed := rep.Result.Failed(); len(failed) != 1 || failed[0] != "audit" {
		t.Fatalf("failed = %v", failed)
	}
	// 模块已回滚到旧配置继续运行。
	for _, st := range mgr.Statuses() {
		if st.ID == "audit" && (st.State != module.StateReady || st.ConfigRevision != 1) {
			t.Fatalf("audit = %+v", st)
		}
	}
}

func TestReloaderRollbackAndHistory(t *testing.T) {
	r, _, rec, _ := newTestStack(t)
	r.keep = 3
	ctx := context.Background()
	for i, v := range []string{"v1", "v2", "v3", "v4"} {
		rep := r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{"value":"`+v+`"}}}`), "")
		if rep.Revision != uint64(i+1) {
			t.Fatalf("load %s = %+v", v, rep)
		}
	}
	if st := r.Status(); !reflect.DeepEqual(st.History, []uint64{2, 3, 4}) {
		t.Fatalf("history = %v", st.History)
	}
	rec.take()
	rep := r.Rollback(ctx, 2)
	if rep.Outcome != OutcomeApplied || rep.Revision != 5 {
		t.Fatalf("rollback = %+v", rep)
	}
	if got := rec.take(); !reflect.DeepEqual(got, []string{"stop:audit", "start:audit:v2"}) {
		t.Fatalf("events = %v", got)
	}
	if rep := r.Rollback(ctx, 1); rep.Outcome != OutcomeRejected || !strings.Contains(rep.Err.Error(), "not in the retained history") {
		t.Fatalf("rollback to evicted revision = %+v", rep)
	}
	if rep := r.Rollback(ctx, 5); rep.Outcome != OutcomeUnchanged {
		t.Fatalf("rollback to current = %+v", rep)
	}
}

func TestReloaderRejectMissingFileKeepsConfig(t *testing.T) {
	r, _, rec, logs := newTestStack(t)
	r.Load(context.Background(), []byte(`{"version":1,"modules":{"audit":{}}}`), "")
	rec.take()
	err := errors.New("module config file does not exist: /x")
	if rep := r.Reject(err); rep.Outcome != OutcomeRejected || rep.Revision != 1 {
		t.Fatalf("reject = %+v", rep)
	}
	n := len(*logs)
	r.Reject(err)
	if len(*logs) != n {
		t.Fatal("identical read error logged repeatedly")
	}
	if len(rec.take()) != 0 {
		t.Fatal("missing file stopped modules")
	}
}

func TestReloaderRejectsWhenManagerClosed(t *testing.T) {
	r, mgr, _, _ := newTestStack(t)
	if err := mgr.Shutdown(); err != nil {
		t.Fatal(err)
	}
	rep := r.Load(context.Background(), []byte(`{"version":1,"modules":{"audit":{}}}`), "")
	if rep.Outcome != OutcomeRejected || !errors.Is(rep.Err, module.ErrManagerClosed) || r.Current() != nil {
		t.Fatalf("report = %+v", rep)
	}
}

func TestReloaderRetriesSameContentAfterValidationFailure(t *testing.T) {
	r, _, rec, logs := newTestStack(t)
	ctx := context.Background()
	content := []byte(`{"version":1,"modules":{"audit":{"value":"x"}}}`)
	rec.mu.Lock()
	rec.blockValidate = true
	rec.mu.Unlock()
	if rep := r.Load(ctx, content, ""); rep.Outcome != OutcomeRejected {
		t.Fatalf("blocked = %+v", rep)
	}
	n := len(*logs)
	if rep := r.Load(ctx, content, ""); rep.Outcome != OutcomeRejected || len(*logs) != n {
		t.Fatalf("repeated identical failure must not log again: %+v", rep)
	}
	// 运维修好外部环境后，同一份文件内容被重新尝试并生效。
	rec.mu.Lock()
	rec.blockValidate = false
	rec.mu.Unlock()
	if rep := r.Load(ctx, content, ""); rep.Outcome != OutcomeApplied || rep.Revision != 1 {
		t.Fatalf("after environment fix = %+v", rep)
	}
}

func TestReloaderRetriesAfterCancelledContext(t *testing.T) {
	r, _, _, _ := newTestStack(t)
	content := []byte(`{"version":1,"modules":{"audit":{}}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rep := r.Load(ctx, content, ""); rep.Outcome != OutcomeRejected || !errors.Is(rep.Err, context.Canceled) {
		t.Fatalf("cancelled = %+v", rep)
	}
	if rep := r.Load(context.Background(), content, ""); rep.Outcome != OutcomeApplied {
		t.Fatalf("retry = %+v", rep)
	}
}

func TestReloaderUnchangedClearsStaleError(t *testing.T) {
	r, _, _, _ := newTestStack(t)
	ctx := context.Background()
	good := []byte(`{"version":1,"modules":{"audit":{}}}`)
	r.Load(ctx, good, "")
	r.Load(ctx, []byte(`{"version":1,"modules":{"ghost":{}}}`), "")
	if st := r.Status(); st.LastError == "" {
		t.Fatal("expected an error")
	}
	if rep := r.Load(ctx, good, ""); rep.Outcome != OutcomeUnchanged {
		t.Fatalf("back to good = %+v", rep)
	}
	if st := r.Status(); st.LastError != "" || st.LastOutcome != OutcomeUnchanged {
		t.Fatalf("stale error kept: %+v", st)
	}
}

func TestReloaderStatusNotBlockedByApply(t *testing.T) {
	r, _, rec, _ := newTestStack(t)
	gate := make(chan struct{})
	rec.mu.Lock()
	rec.startGate = gate
	rec.mu.Unlock()
	done := make(chan ReloadReport)
	go func() { done <- r.Load(context.Background(), []byte(`{"version":1,"modules":{"audit":{}}}`), "") }()
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.take()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond) // 等 Start 开始（阻塞在 gate 上）
	}
	start := time.Now()
	r.Status()
	r.Current()
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("Status blocked by an in-flight Apply")
	}
	close(gate)
	if rep := <-done; rep.Outcome != OutcomeApplied {
		t.Fatalf("load = %+v", rep)
	}
}

func TestReloaderDoesNotLogCancellation(t *testing.T) {
	r, _, _, logs := newTestStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Load(ctx, []byte(`{"version":1,"modules":{"audit":{}}}`), "")
	if len(*logs) != 0 {
		t.Fatalf("cancellation logged as a config failure: %v", *logs)
	}
	if st := r.Status(); !strings.Contains(st.LastError, "canceled") {
		t.Fatalf("status = %+v", st)
	}
}

func TestReloaderRejectsUnknownDisabledModule(t *testing.T) {
	r, _, _, _ := newTestStack(t)
	rep := r.Load(context.Background(), []byte(`{"version":1,"modules":{"audti":{"enabled":false}}}`), "")
	if rep.Outcome != OutcomeRejected || !strings.Contains(rep.Err.Error(), `unknown module "audti"`) {
		t.Fatalf("typo in disabled module accepted: %+v", rep)
	}
	if rep := r.Load(context.Background(), []byte(`{"version":1,"modules":{"audit":{"enabled":false}}}`), ""); rep.Outcome != OutcomeApplied {
		t.Fatalf("known disabled module rejected: %+v", rep)
	}
}

func TestReloaderRejectsRequirementMismatch(t *testing.T) {
	r, _, rec, _ := newTestStack(t)
	rep := r.Load(context.Background(), []byte(`{"version":1,"modules":{"audit":{"require":{"source":"example.com/team/mods"}}}}`), "")
	if rep.Outcome != OutcomeRejected || !strings.Contains(rep.Err.Error(), "config expects source example.com/team/mods") || len(rec.take()) != 0 {
		t.Fatalf("source mismatch accepted: %+v", rep)
	}
	rep = r.Load(context.Background(), []byte(`{"version":1,"modules":{"audit":{"require":{"source":"github.com/remote-assist/tool","min_version":"1.0"}}}}`), "")
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("matching requirement rejected: %+v", rep)
	}
}
