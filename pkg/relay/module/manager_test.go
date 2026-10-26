package module

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func statusOf(t *testing.T, m *Manager, id string) Status {
	t.Helper()
	for _, s := range m.Statuses() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("module %s has no status", id)
	return Status{}
}

func actions(r ApplyResult) map[string]Action {
	out := map[string]Action{}
	for _, m := range r.Modules {
		out[m.ID] = m.Action
	}
	return out
}

func mustApply(t *testing.T, m *Manager, d Desired) ApplyResult {
	t.Helper()
	r, err := m.Apply(context.Background(), d)
	if err != nil {
		t.Fatalf("apply revision %d: %v", d.Revision, err)
	}
	return r
}

func chainRegistry(t *testing.T, env *fakeEnv) *Registry {
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "store"})
	registerFake(t, reg, env, Descriptor{ID: "audit", Requires: []Dependency{{ID: "store"}}})
	registerFake(t, reg, env, Descriptor{ID: "notify", Requires: []Dependency{{ID: "audit"}}})
	registerFake(t, reg, env, Descriptor{ID: "solo"})
	return reg
}

func TestCanTransition(t *testing.T) {
	legal := [][2]State{
		{StateStopped, StateStarting}, {StateFailed, StateStarting}, {StateStarting, StateReady},
		{StateStarting, StateDegraded}, {StateStarting, StateFailed}, {StateReady, StateDegraded},
		{StateDegraded, StateReady}, {StateReady, StateStopping}, {StateDegraded, StateStopping},
		{StateStopping, StateStopped}, {StateStopping, StateFailed},
	}
	for _, p := range legal {
		if !canTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be legal", p[0], p[1])
		}
	}
	illegal := [][2]State{
		{StateStopped, StateReady}, {StateReady, StateStarting}, {StateReady, StateStopped},
		{StateStopping, StateReady}, {StateFailed, StateReady}, {StateStopped, StateFailed},
	}
	for _, p := range illegal {
		if canTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be illegal", p[0], p[1])
		}
	}
}

func TestManagerStartsInDependencyOrderAndStopsInReverse(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	r := mustApply(t, m, desired(1, "notify", "audit", "store"))
	if got := env.take(); !reflect.DeepEqual(got, []string{"start:store:", "start:audit:", "start:notify:"}) {
		t.Fatalf("start events = %v", got)
	}
	if a := actions(r); a["store"] != ActionStarted || a["notify"] != ActionStarted || len(r.Failed()) != 0 {
		t.Fatalf("result = %+v", r)
	}
	for _, id := range []string{"store", "audit", "notify"} {
		st := statusOf(t, m, id)
		if st.State != StateReady || st.ConfigRevision != 1 || st.Version != "1.0.0" {
			t.Fatalf("%s status = %+v", id, st)
		}
	}
	if rev, digest := m.Revision(); rev != 1 || digest != "digest-1" {
		t.Fatalf("revision = %d %s", rev, digest)
	}
	if err := m.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if got := env.take(); !reflect.DeepEqual(got, []string{"stop:notify", "stop:audit", "stop:store"}) {
		t.Fatalf("stop events = %v", got)
	}
	if err := m.Shutdown(); err != nil {
		t.Fatal("second shutdown must be a no-op")
	}
	if _, err := m.Apply(context.Background(), desired(2)); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("apply after shutdown: %v", err)
	}
}

func TestManagerValidationFailureKeepsRunningModules(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, `solo={"value":"v1"}`))
	env.take()
	bad := []Desired{
		desired(2, `solo={"invalid":true}`),
		desired(3, `solo={"unknown_field":1}`),
		desired(4, "notify"),        // 缺必选依赖
		desired(5, "solo", "ghost"), // 未注册模块
	}
	v := desired(6, "solo")
	v.SchemaVersion = CurrentConfigVersion + 1
	bad = append(bad, v)
	for _, d := range bad {
		if _, err := m.Apply(context.Background(), d); err == nil {
			t.Fatalf("revision %d accepted", d.Revision)
		}
	}
	if got := env.take(); len(got) != 0 {
		t.Fatalf("invalid configs touched running modules: %v", got)
	}
	if st := statusOf(t, m, "solo"); st.State != StateReady || st.ConfigRevision != 1 {
		t.Fatalf("solo = %+v", st)
	}
	if rev, _ := m.Revision(); rev != 1 {
		t.Fatalf("revision moved to %d after rejected configs", rev)
	}
}

func TestManagerRestartsChangedModuleAndDependents(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, "store", "audit", "notify", "solo"))
	env.take()
	r := mustApply(t, m, desired(2, `store={"value":"v2"}`, "audit", "notify", "solo"))
	want := []string{"stop:notify", "stop:audit", "stop:store", "start:store:v2", "start:audit:", "start:notify:"}
	if got := env.take(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	a := actions(r)
	if a["store"] != ActionRestarted || a["notify"] != ActionRestarted {
		t.Fatalf("actions = %v", a)
	}
	if _, touched := a["solo"]; touched {
		t.Fatal("unrelated module was restarted")
	}
	if st := statusOf(t, m, "solo"); st.ConfigRevision != 1 {
		t.Fatalf("unchanged module revision = %d", st.ConfigRevision)
	}
	// 同一份配置再 Apply 不产生任何动作。
	if r := mustApply(t, m, desired(3, `store={"value":"v2"}`, "audit", "notify", "solo")); len(r.Modules) != 0 || len(env.take()) != 0 {
		t.Fatalf("no-op apply did work: %+v", r)
	}
}

func TestManagerDisablesModules(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, "store", "audit", "solo"))
	env.take()
	r := mustApply(t, m, desired(2, "store"))
	if a := actions(r); a["audit"] != ActionStopped || a["solo"] != ActionStopped {
		t.Fatalf("actions = %v", a)
	}
	if got := env.take(); len(got) != 2 || got[0] == "stop:store" {
		t.Fatalf("events = %v", got)
	}
	if sts := m.Statuses(); len(sts) != 1 || sts[0].ID != "store" {
		t.Fatalf("statuses = %+v", sts)
	}
}

func TestManagerStartFailureIsIsolated(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	r := mustApply(t, m, desired(1, `store={"fail":true}`, "audit", "solo"))
	a := actions(r)
	if a["store"] != ActionFailed || a["audit"] != ActionFailed || a["solo"] != ActionStarted {
		t.Fatalf("actions = %v", a)
	}
	if st := statusOf(t, m, "audit"); st.State != StateFailed || !strings.Contains(st.Error, "required module(s) not running: store") {
		t.Fatalf("audit = %+v", st)
	}
	if st := statusOf(t, m, "solo"); st.State != StateReady {
		t.Fatalf("solo = %+v", st)
	}
	env.take()
	// 配置未变：暂停重试，不再调用 Start。
	r = mustApply(t, m, desired(2, `store={"fail":true}`, "audit", `solo={"value":"x"}`))
	if a := actions(r); a["store"] != ActionSkipped {
		t.Fatalf("actions = %v", a)
	}
	for _, e := range env.take() {
		if strings.HasPrefix(e, "start:store") {
			t.Fatal("failed module retried with unchanged config")
		}
	}
	// 配置变化后重试成功，依赖者随之启动。
	r = mustApply(t, m, desired(3, `store={"value":"fixed"}`, "audit", `solo={"value":"x"}`))
	if a := actions(r); a["store"] != ActionStarted || a["audit"] != ActionStarted {
		t.Fatalf("actions = %v", a)
	}
	if st := statusOf(t, m, "audit"); st.State != StateReady || st.Error != "" {
		t.Fatalf("audit = %+v", st)
	}
}

func TestManagerRollsBackToPreviousConfig(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, `store={"value":"good"}`, "audit", "solo"))
	env.take()
	r := mustApply(t, m, desired(2, `store={"value":"bad","fail":true}`, "audit", "solo"))
	if a := actions(r); a["store"] != ActionRolledBack || a["audit"] != ActionRestarted {
		t.Fatalf("actions = %v", a)
	}
	want := []string{"stop:audit", "stop:store", "start:store:bad", "stop:store", "start:store:good", "start:audit:"}
	if got := env.take(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	st := statusOf(t, m, "store")
	// 回滚后运行的是修订 1 的配置，ConfigRevision 如实报告旧修订号。
	if st.State != StateReady || st.ConfigRevision != 1 || !strings.Contains(st.Error, "start failed by request") {
		t.Fatalf("store = %+v", st)
	}
	// 其他模块变化触发 Apply 时，同一份坏配置不再打断正在用旧配置运行的 store。
	r = mustApply(t, m, desired(3, `store={"value":"bad","fail":true}`, "audit", `solo={"value":"y"}`))
	if a := actions(r); a["store"] != ActionSkipped || a["solo"] != ActionRestarted {
		t.Fatalf("actions = %v", a)
	}
	if got := env.take(); !reflect.DeepEqual(got, []string{"stop:solo", "start:solo:y"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestManagerStartTimeoutAndPanic(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	start := time.Now()
	r := mustApply(t, m, desired(1, `store={"hang":true}`, `solo={"panic":true}`))
	if time.Since(start) > 2*time.Second {
		t.Fatal("start timeout not enforced")
	}
	if a := actions(r); a["store"] != ActionFailed || a["solo"] != ActionFailed {
		t.Fatalf("actions = %v", a)
	}
	if st := statusOf(t, m, "store"); !strings.Contains(st.Error, "timed out") {
		t.Fatalf("store = %+v", st)
	}
	if st := statusOf(t, m, "solo"); !strings.Contains(st.Error, "panic") {
		t.Fatalf("solo = %+v", st)
	}
	// 超时的实例也会被清理（Stop 被调用）。
	if got := env.take(); !contains(got, "stop:store") || !contains(got, "stop:solo") {
		t.Fatalf("failed instances not cleaned up: %v", got)
	}
}

func TestManagerStopTimeoutDoesNotBlockApply(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, `solo={"stop_hang":true}`))
	start := time.Now()
	r := mustApply(t, m, desired(2, `solo={"value":"v2"}`))
	if time.Since(start) > 2*time.Second {
		t.Fatal("stop timeout not enforced")
	}
	if a := actions(r); a["solo"] != ActionRestarted {
		t.Fatalf("actions = %v", a)
	}
	if st := statusOf(t, m, "solo"); st.State != StateReady {
		t.Fatalf("solo = %+v", st)
	}
}

func TestManagerTaskPanicDegradesModule(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, `solo={"task":"panic"}`, `store={"task":"loop"}`))
	deadline := time.Now().Add(time.Second)
	for statusOf(t, m, "solo").State != StateDegraded && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	st := statusOf(t, m, "solo")
	if st.State != StateDegraded || len(st.DegradedReasons) != 1 || !strings.Contains(st.DegradedReasons[0], "task boom panicked") {
		t.Fatalf("solo = %+v", st)
	}
	if st := statusOf(t, m, "store"); st.State != StateReady {
		t.Fatalf("store = %+v", st)
	}
	// 托管任务在停止时被取消并等待退出。
	if err := m.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestManagerOptionalDependencyDegradesAndRecovers(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "audit"})
	registerFake(t, reg, env, Descriptor{ID: "notify", Requires: []Dependency{{ID: "audit", Optional: true}}})
	m := newTestManager(reg)
	mustApply(t, m, desired(1, "notify"))
	st := statusOf(t, m, "notify")
	if st.State != StateDegraded || len(st.DegradedReasons) != 1 {
		t.Fatalf("notify = %+v", st)
	}
	env.take()
	// 启用可选依赖后，依赖者按新顺序重启并恢复 ready。
	mustApply(t, m, desired(2, "notify", "audit"))
	if got := env.take(); !reflect.DeepEqual(got, []string{"stop:notify", "start:audit:", "start:notify:"}) {
		t.Fatalf("events = %v", got)
	}
	if st := statusOf(t, m, "notify"); st.State != StateReady || len(st.DegradedReasons) != 0 {
		t.Fatalf("notify = %+v", st)
	}
	// 可选依赖的新配置启动失败：audit 回滚到旧配置继续运行，notify 不降级。
	r := mustApply(t, m, desired(3, "notify", `audit={"fail":true}`))
	if a := actions(r); a["audit"] != ActionRolledBack || a["notify"] != ActionRestarted {
		t.Fatalf("actions = %v", a)
	}
	if st := statusOf(t, m, "notify"); st.State != StateReady {
		t.Fatalf("notify = %+v", st)
	}

	// 可选依赖首次启用即失败（没有可回滚的配置）：依赖者仍运行，但为 degraded。
	m2 := newTestManager(reg)
	r = mustApply(t, m2, desired(1, "notify", `audit={"fail":true}`))
	if a := actions(r); a["audit"] != ActionFailed || a["notify"] != ActionStarted {
		t.Fatalf("actions = %v", a)
	}
	if st := statusOf(t, m2, "notify"); st.State != StateDegraded || !strings.Contains(strings.Join(st.DegradedReasons, ";"), "audit not running") {
		t.Fatalf("notify = %+v", st)
	}
}

func TestManagerHealthProbe(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	for _, id := range []string{"a", "b", "c", "d"} {
		registerFake(t, reg, env, Descriptor{ID: id})
	}
	m := newTestManager(reg)
	mustApply(t, m, desired(1, `a={"health":"ok"}`, `b={}`, `c={"health":"panic"}`, `d={"health":"block"}`))
	start := time.Now()
	got := map[string]HealthStatus{}
	for _, st := range m.Statuses() {
		got[st.ID] = st.Health.Status
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("blocking Health() not bounded")
	}
	want := map[string]HealthStatus{"a": HealthOK, "b": HealthUnknown, "c": HealthUnhealthy, "d": HealthUnhealthy}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("health = %v, want %v", got, want)
	}
}

func TestManagerPassesSharedInfo(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	var shared SharedInfo
	if err := reg.Register(func() Module {
		return &probeModule{fakeModule: fakeModule{env: env, desc: Descriptor{ID: "probe", Version: "1", Source: SourceCore, APIVersion: APIVersion}}, got: &shared}
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(reg)
	mustApply(t, m, desired(7, "probe"))
	if shared.ConfigRevision != 7 || shared.ConfigDigest != "digest-7" || shared.RelayVersion != "v1.0.0" || shared.Instance != "default" {
		t.Fatalf("shared = %+v", shared)
	}
}

type probeModule struct {
	fakeModule
	got *SharedInfo
}

func (p *probeModule) Start(ctx context.Context, mc ModuleContext) error {
	*p.got = mc.Shared()
	return nil
}

func contains(list []string, v string) bool {
	_, ok := indexOf(list, v)
	return ok
}

func TestManagerTaskPanicDuringStartIsNotLost(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "solo", MaxTasks: 1})
	m := newTestManager(reg)
	for i := 0; i < 20; i++ {
		mustApply(t, m, desired(uint64(i+1), fmt.Sprintf(`solo={"task":"panic_during_start","value":"%d"}`, i)))
		st := statusOf(t, m, "solo")
		if st.State != StateDegraded || len(st.DegradedReasons) != 1 || !strings.Contains(st.DegradedReasons[0], "boom") {
			t.Fatalf("iteration %d: solo = %+v", i, st)
		}
	}
}

func TestManagerRetriesKnownGoodConfigAfterTransientFailure(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, "store", "audit", "solo"))
	env.mu.Lock()
	env.failStarts["audit"] = 1
	env.mu.Unlock()
	// store 变化连带重启 audit；audit 用的是已知可用配置，这次瞬时失败。
	r := mustApply(t, m, desired(2, `store={"value":"v2"}`, "audit", "solo"))
	if a := actions(r); a["audit"] != ActionFailed {
		t.Fatalf("actions = %v", a)
	}
	// 无关模块变化触发下一次 Apply 时 audit 被重试，而不是暂停。
	r = mustApply(t, m, desired(3, `store={"value":"v2"}`, "audit", `solo={"value":"x"}`))
	if a := actions(r); a["audit"] != ActionStarted {
		t.Fatalf("actions = %v", a)
	}
	if st := statusOf(t, m, "audit"); st.State != StateReady || st.Error != "" {
		t.Fatalf("audit = %+v", st)
	}
}

func TestManagerApplyIgnoresCancellationOnceStarted(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	cancelled, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	if _, err := m.Apply(cancelled, desired(1, "solo")); !errors.Is(err, context.Canceled) {
		t.Fatalf("apply with cancelled ctx: %v", err)
	}
	if len(env.take()) != 0 || len(m.Statuses()) != 0 {
		t.Fatal("cancelled apply touched modules")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.mu.Lock()
	env.onStart = func(id string) {
		if id == "store" {
			cancel() // 调用方在 Apply 途中取消
		}
	}
	env.mu.Unlock()
	r, err := m.Apply(ctx, desired(2, "store", "audit", "solo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"store", "audit", "solo"} {
		if a := actions(r); a[id] != ActionStarted {
			t.Fatalf("actions = %v", a)
		}
	}
}

func TestManagerOptionalDependencyRecoversFromFailure(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "audit"})
	registerFake(t, reg, env, Descriptor{ID: "notify", Requires: []Dependency{{ID: "audit", Optional: true}}})
	m := newTestManager(reg)
	mustApply(t, m, desired(1, "notify", `audit={"fail":true}`))
	if st := statusOf(t, m, "notify"); st.State != StateDegraded {
		t.Fatalf("notify = %+v", st)
	}
	env.take()
	r := mustApply(t, m, desired(2, "notify", `audit={"value":"fixed"}`))
	if a := actions(r); a["audit"] != ActionStarted || a["notify"] != ActionRestarted {
		t.Fatalf("actions = %v", a)
	}
	// audit 实际启动成功之后，notify 才被重启以按顺序接上它。
	if got := env.take(); !reflect.DeepEqual(got, []string{"start:audit:fixed", "stop:notify", "start:notify:"}) {
		t.Fatalf("events = %v", got)
	}
	if st := statusOf(t, m, "notify"); st.State != StateReady || len(st.DegradedReasons) != 0 {
		t.Fatalf("notify = %+v", st)
	}
}

func TestManagerKnownBadConfigRestartsOnGoodWhenDependencyRestarts(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, "store", `audit={"value":"good"}`))
	mustApply(t, m, desired(2, "store", `audit={"value":"bad","fail":true}`)) // rolled back
	env.take()
	// store 变化连带停掉 audit；audit 的期望配置仍是已知坏配置，应直接用旧配置启动。
	r := mustApply(t, m, desired(3, `store={"value":"v3"}`, `audit={"value":"bad","fail":true}`))
	if a := actions(r); a["audit"] != ActionSkipped {
		t.Fatalf("actions = %v", a)
	}
	if got := env.take(); !reflect.DeepEqual(got, []string{"stop:audit", "stop:store", "start:store:v3", "start:audit:good"}) {
		t.Fatalf("events = %v", got)
	}
	// 坏 → 好 → 坏：回到好配置时清掉拒绝记录，再次出现坏配置会重新尝试。
	mustApply(t, m, desired(4, `store={"value":"v3"}`, `audit={"value":"good"}`))
	if st := statusOf(t, m, "audit"); st.Error != "" {
		t.Fatalf("stale rejection kept: %+v", st)
	}
	env.take()
	r = mustApply(t, m, desired(5, `store={"value":"v3"}`, `audit={"value":"bad","fail":true}`))
	if a := actions(r); a["audit"] != ActionRolledBack {
		t.Fatalf("actions = %v", a)
	}
}

func TestManagerStartTimeoutWaitsForStartBeforeStop(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	mustApply(t, m, desired(1, `store={"hang":true}`, `solo={"hang_hard":true}`))
	got := env.take()
	ret, found := indexOf(got, "start-returned:store")
	stop, ok := indexOf(got, "stop:store")
	if !ok || !found || ret > stop {
		t.Fatalf("Stop called before timed-out Start returned: %v", got)
	}
	if contains(got, "stop:solo") {
		t.Fatalf("Stop called while Start still running: %v", got)
	}
	if st := statusOf(t, m, "solo"); st.State != StateFailed {
		t.Fatalf("solo = %+v", st)
	}
}

func TestManagerBlockedHealthDoesNotAccumulateProbes(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "a"})
	registerFake(t, reg, env, Descriptor{ID: "b"})
	m := newTestManager(reg)
	mustApply(t, m, desired(1, `a={"health":"block"}`, `b={"health":"block"}`))
	start := time.Now()
	m.Statuses() // 两个模块并行探测：总等待约 1s 而不是 2s
	if time.Since(start) > 1800*time.Millisecond {
		t.Fatalf("probes not parallel: %v", time.Since(start))
	}
	start = time.Now()
	for _, st := range m.Statuses() {
		if !strings.Contains(st.Health.Message, "still running") {
			t.Fatalf("status = %+v", st)
		}
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("second Statuses waited on stuck probes")
	}
	m.mu.RLock()
	n := len(m.probing)
	m.mu.RUnlock()
	if n != 2 {
		t.Fatalf("probing = %d, want 2 (one per stuck module)", n)
	}
}

func TestManagerStatusesConcurrentWithApply(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				m.Statuses()
				m.Revision()
			}
		}
	}()
	for i := 1; i <= 30; i++ {
		mustApply(t, m, desired(uint64(i), fmt.Sprintf(`store={"value":"%d"}`, i), "audit", `solo={"task":"loop"}`))
	}
	close(stop)
	<-done
	if err := m.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPersistentlyFailingOptionalDependencyDoesNotRestartDependent(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "store"})
	registerFake(t, reg, env, Descriptor{ID: "audit", Requires: []Dependency{{ID: "store"}}})
	registerFake(t, reg, env, Descriptor{ID: "notify", Requires: []Dependency{{ID: "audit", Optional: true}}})
	registerFake(t, reg, env, Descriptor{ID: "solo"})
	m := newTestManager(reg)
	// store 自身失败（暂停重试）→ audit 因必选依赖缺失失败（可重试）→ notify 降级运行。
	mustApply(t, m, desired(1, `store={"fail":true}`, "audit", "notify", "solo"))
	if st := statusOf(t, m, "notify"); st.State != StateDegraded {
		t.Fatalf("notify = %+v", st)
	}
	for i := 2; i <= 4; i++ {
		env.take()
		mustApply(t, m, desired(uint64(i), `store={"fail":true}`, "audit", "notify", fmt.Sprintf(`solo={"value":"%d"}`, i)))
		for _, e := range env.take() {
			if strings.Contains(e, "notify") {
				t.Fatalf("revision %d restarted notify although audit still cannot run: %s", i, e)
			}
		}
	}
}

func TestModuleContextPanicReasonsAreBounded(t *testing.T) {
	mc := newTestContext(1, nil)
	for i := 0; i < maxPanicReasons+5; i++ {
		mc.reportTaskPanic("t", i)
	}
	got := mc.panicReasons()
	if len(got) != maxPanicReasons+1 || !strings.Contains(got[0], "21 task panics in total (5 older omitted)") {
		t.Fatalf("panic reasons = %d %q", len(got), got[0])
	}
}

func TestManagerOptionalRecoveryKeepsRollbackResultOfDependents(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "a"})
	registerFake(t, reg, env, Descriptor{ID: "x", Requires: []Dependency{{ID: "a", Optional: true}}})
	registerFake(t, reg, env, Descriptor{ID: "y", Requires: []Dependency{{ID: "x"}}})
	m := newTestManager(reg)
	mustApply(t, m, desired(1, `a={"fail":true}`, "x", `y={"value":"good"}`))
	if st := statusOf(t, m, "x"); st.State != StateDegraded {
		t.Fatalf("x = %+v", st)
	}
	// 同一次 Apply：a 修好、y 换成坏配置。
	r := mustApply(t, m, desired(2, `a={"value":"fixed"}`, "x", `y={"value":"bad","fail":true}`))
	a := actions(r)
	if a["a"] != ActionStarted || a["x"] != ActionRestarted || a["y"] != ActionRolledBack {
		t.Fatalf("actions = %v", a)
	}
	if failed := r.Failed(); len(failed) != 1 || failed[0] != "y" {
		t.Fatalf("failed = %v", failed)
	}
	st := statusOf(t, m, "y")
	if st.State != StateReady || !strings.Contains(st.Error, "start failed by request") || st.ConfigRevision != 1 {
		t.Fatalf("y = %+v", st)
	}
	if st := statusOf(t, m, "x"); st.State != StateReady {
		t.Fatalf("x = %+v", st)
	}
}

func TestManagerValidateRejectsUnknownDisabledModule(t *testing.T) {
	env := newFakeEnv(t)
	m := newTestManager(chainRegistry(t, env))
	d := desired(1, "solo")
	d.Disabled = []string{"ghost"}
	if _, err := m.Apply(context.Background(), d); err == nil || !strings.Contains(err.Error(), `unknown module "ghost"`) {
		t.Fatalf("unknown disabled module accepted: %v", err)
	}
	d.Disabled = []string{"audit"}
	if _, err := m.Apply(context.Background(), d); err != nil {
		t.Fatalf("known disabled module rejected: %v", err)
	}
}
