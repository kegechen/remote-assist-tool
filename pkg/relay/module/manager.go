package module

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// State 是模块生命周期状态：
//
//	stopped/failed → starting → ready ⇄ degraded → stopping → stopped
//	                     └──────────→ failed ←──────────┘
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateDegraded State = "degraded"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

var allowedTransitions = map[State][]State{
	StateStopped:  {StateStarting},
	StateFailed:   {StateStarting},
	StateStarting: {StateReady, StateDegraded, StateFailed},
	StateReady:    {StateDegraded, StateStopping},
	StateDegraded: {StateReady, StateStopping},
	StateStopping: {StateStopped, StateFailed},
}

// canTransition 报告 from → to 是否是合法的生命周期迁移。
func canTransition(from, to State) bool {
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// healthCheckTimeout 一次 Statuses() 等待各模块 Health() 的总时长；超时按 unhealthy 报告。
const healthCheckTimeout = time.Second

var (
	// ErrManagerClosed Shutdown 之后再调用 Apply。
	ErrManagerClosed = errors.New("module manager closed")
	errTimeout       = errors.New("timed out")
)

// Desired 是一次配置快照对模块的期望状态，由 config 包从统一配置生成。
type Desired struct {
	// Revision / Digest 标识来源配置快照，透传给模块的 SharedInfo 与状态。
	Revision uint64
	Digest   string
	// SchemaVersion 统一配置根节点的 version。
	SchemaVersion int
	// Modules 已启用模块的配置；未出现的模块即停用。
	Modules map[string]Config
	// Disabled 配置中出现但 enabled=false 的模块 ID。它们不会启动，但同样必须是已注册的
	// 内置模块，避免拼写错误的停用条目被静默接受。
	Disabled []string
	// Requirements 配置对模块来源/最低版本的期望（modules.<id>.require.{source,min_version}），可为空。
	Requirements map[string]Requirement
}

func (d Desired) enabledIDs() []string {
	ids := make([]string, 0, len(d.Modules))
	for id := range d.Modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (d Desired) ref(id string) cfgRef {
	return cfgRef{cfg: d.Modules[id], revision: d.Revision, digest: d.Digest}
}

// Action 是一次 Apply 对单个模块做了什么。
type Action string

const (
	ActionStarted    Action = "started"
	ActionRestarted  Action = "restarted"
	ActionStopped    Action = "stopped"
	ActionRolledBack Action = "rolled_back" // 新配置启动失败，已用上一份有效配置重新启动
	ActionFailed     Action = "failed"
	ActionSkipped    Action = "skipped" // 该配置此前已启动失败：保持现状，等待下一次配置变化
)

// ModuleResult 是 Apply 对单个模块的处理结果。
type ModuleResult struct {
	ID     string
	Action Action
	Error  string
}

// ApplyResult 汇总一次 Apply。模块级失败只体现在这里，不会让 Apply 返回 error。
type ApplyResult struct {
	Revision uint64
	Modules  []ModuleResult
}

// Failed 返回本次处理后未能以期望配置运行的模块 ID（含回滚到旧配置、暂停重试的）。
func (r ApplyResult) Failed() []string {
	var out []string
	for _, m := range r.Modules {
		if m.Action == ActionFailed || m.Action == ActionRolledBack || m.Action == ActionSkipped || m.Error != "" {
			out = append(out, m.ID)
		}
	}
	return out
}

// Status 是单个模块的可观测状态快照。
type Status struct {
	ID              string
	Version         string
	Source          string
	APIVersion      int
	State           State
	Health          Health
	Error           string
	DegradedReasons []string
	// ConfigRevision 运行实例所用配置的修订号；未运行时为最近一次尝试启动的修订号。
	// 回滚后它是旧配置的修订号，可与 Manager.Revision() 比较判断是否落后。
	ConfigRevision uint64
	Since          time.Time
}

// cfgRef 是一份模块配置及其来源快照。
type cfgRef struct {
	cfg      Config
	revision uint64
	digest   string
}

// instance 是一个已成功启动的模块实例。
type instance struct {
	mod  Module
	mc   *moduleContext
	desc Descriptor
	ref  cfgRef
}

type slot struct {
	id   string
	desc Descriptor
	inst *instance // nil = 未运行

	attempted cfgRef // 最近一次尝试启动的配置
	good      cfgRef // 最近一次成功启动的配置，用于回滚
	hasGood   bool
	// rejected 是启动失败并已回滚的新配置：同一份配置再出现时直接沿用 good，不再重试。
	rejected    Config
	hasRejected bool
	// retryable 最近一次失败可在下次 Apply 重试（依赖未运行、已知可用配置的瞬时失败、回滚失败）；
	// 否则同一份配置暂停重试，等待配置变化。
	retryable bool

	state        State
	err          string
	baseDegraded []string // 依赖相关的降级原因；托管任务 panic 原因另由 inst.mc 记录
	// waitingOptional 启动时已启用但未运行的可选依赖；它们恢复后本模块需重启以按顺序接上。
	waitingOptional []string
	since           time.Time
}

// Manager 负责模块启用/停用、依赖顺序、启停超时和健康汇总。
//
// 所有模块代码都在 callSafely 中执行：模块 panic、超时或报错只影响该模块（及依赖它的模块），
// 不会传播到调用方，也就不会让 relay 核心退出。持有 mu 时从不调用模块代码。
type Manager struct {
	reg  *Registry
	base SharedInfo
	logf func(format string, args ...any)
	now  func() time.Time

	applyMu sync.Mutex // 串行化 Apply / Shutdown，并保护 plan、closed
	plan    Plan
	closed  bool

	mu       sync.RWMutex // 保护 slots、slot 字段、probing
	slots    map[string]*slot
	probing  map[*moduleContext]bool // 仍未返回的 Health() 探测，避免阻塞的模块累积 goroutine
	revision uint64
	digest   string
}

// NewManager 创建模块管理器。base 中的 ConfigRevision/ConfigDigest 会被每次 Apply 覆盖。
func NewManager(reg *Registry, base SharedInfo) *Manager {
	return &Manager{
		reg:     reg,
		base:    base,
		logf:    log.Printf,
		now:     time.Now,
		slots:   make(map[string]*slot),
		probing: make(map[*moduleContext]bool),
	}
}

// Validate 校验期望状态：模块均已注册、支持该配置版本、依赖/冲突/循环检查通过、
// 每个模块的 Validate 通过。无副作用，失败时返回汇总错误。
func (m *Manager) Validate(d Desired) (Plan, error) {
	for _, id := range d.Disabled {
		if _, ok := m.reg.Descriptor(id); !ok {
			return Plan{}, fmt.Errorf("unknown module %q (not a built-in module)", id)
		}
	}
	// 来源/版本期望对启用与停用的模块都校验：配置写给另一份构建时尽早暴露。
	reqIDs := make([]string, 0, len(d.Requirements))
	for id := range d.Requirements {
		reqIDs = append(reqIDs, id)
	}
	sort.Strings(reqIDs)
	for _, id := range reqIDs {
		req := d.Requirements[id]
		desc, ok := m.reg.Descriptor(id)
		if !ok {
			return Plan{}, fmt.Errorf("unknown module %q (not a built-in module)", id)
		}
		if err := CheckRequirement(desc, req); err != nil {
			return Plan{}, err
		}
	}
	plan, err := BuildPlan(m.reg, d.enabledIDs())
	if err != nil {
		return Plan{}, err
	}
	var errs []string
	for _, id := range plan.Order {
		cfg := d.Modules[id]
		if cfg.ModuleID != id || cfg.SchemaVersion != d.SchemaVersion {
			errs = append(errs, fmt.Sprintf("module %s: config belongs to %q version %d", id, cfg.ModuleID, cfg.SchemaVersion))
			continue
		}
		mod, desc, err := m.reg.newInstance(id)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if !desc.SupportsConfigVersion(d.SchemaVersion) {
			errs = append(errs, fmt.Sprintf("module %s (%s %s) does not support config version %d", id, desc.Source, desc.Version, d.SchemaVersion))
			continue
		}
		note, err := CheckCoreCompatibility(desc, m.base.CoreVersion)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if note != "" {
			m.logf("module manager: WARNING: %s", note)
		}
		if err := callSafely(func() error { return mod.Validate(cfg) }); err != nil {
			errs = append(errs, fmt.Sprintf("module %s: invalid config: %v", id, err))
		}
	}
	if len(errs) > 0 {
		return Plan{}, errors.New(strings.Join(errs, "; "))
	}
	return plan, nil
}

// Apply 让运行中的模块与 d 一致。校验失败（或调用前 ctx 已取消）时返回 error 且不触碰任何
// 运行中的模块；一旦开始停启，就不再响应 ctx 取消，整个过程由各模块的启停超时约束，
// 避免半途取消把模块留在停止状态。
//
// 第一阶段语义：停用的模块、配置变化的模块、可选依赖可用性将发生变化的模块及它们的（传递）
// 依赖者按依赖逆序停止，然后按依赖顺序启动所有未运行的启用模块；未变化的模块不受影响。
// 新配置启动失败时，有上一份有效配置就用它重新启动（rolled_back），否则标记为 failed；
// 必选依赖未运行的模块也标记为 failed。失败只影响该模块及依赖它的模块。
func (m *Manager) Apply(ctx context.Context, d Desired) (ApplyResult, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if m.closed {
		return ApplyResult{}, ErrManagerClosed
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	newPlan, err := m.Validate(d)
	if err != nil {
		return ApplyResult{}, err
	}
	ctx = context.WithoutCancel(ctx)
	oldPlan := m.plan
	result := ApplyResult{Revision: d.Revision}

	// 1. 计算需要停掉的运行实例。
	roots, keptOnGood := m.stopRoots(d, oldPlan, newPlan)
	stopSet := oldPlan.closureOfDependents(roots)
	for id := range newPlan.closureOfDependents(roots) {
		stopSet[id] = true
	}
	restarted := map[string]bool{}
	for i := len(oldPlan.Order) - 1; i >= 0; i-- {
		id := oldPlan.Order[i]
		s := m.slot(id)
		if !stopSet[id] || s == nil || !m.running(s) {
			continue
		}
		if err := m.stopModule(s); err != nil {
			m.logf("module manager: stop %s: %v", id, err)
		}
		if _, keep := d.Modules[id]; keep {
			restarted[id] = true
		} else {
			result.Modules = append(result.Modules, ModuleResult{ID: id, Action: ActionStopped, Error: m.slotErr(s)})
		}
	}
	m.mu.Lock()
	for id := range m.slots {
		if _, keep := d.Modules[id]; !keep {
			delete(m.slots, id)
		}
	}
	m.revision, m.digest = d.Revision, d.Digest
	m.mu.Unlock()

	// 2. 按依赖顺序启动未运行的启用模块。
	for _, id := range newPlan.Order {
		s := m.ensureSlot(id)
		if m.running(s) {
			if keptOnGood[id] {
				result.Modules = append(result.Modules, ModuleResult{ID: id, Action: ActionSkipped, Error: m.slotErr(s)})
			}
			continue
		}
		var res ModuleResult
		switch {
		case m.knownRejected(s, d.Modules[id]):
			// 这份配置此前已启动失败并回滚：因依赖重启被停掉时直接沿用旧配置。
			res = m.startKnownGood(ctx, s, newPlan)
		case !restarted[id] && m.pausedRetry(s, d.Modules[id]):
			res = ModuleResult{ID: id, Action: ActionSkipped, Error: m.slotErr(s)}
		default:
			res = m.startWithRollback(ctx, s, d.ref(id), newPlan, true)
			if res.Action == ActionStarted && restarted[id] {
				res.Action = ActionRestarted
			}
		}
		result.Modules = append(result.Modules, res)
	}
	result.Modules = m.restartForRecoveredOptional(ctx, newPlan, result.Modules)
	m.plan = newPlan
	sort.Slice(result.Modules, func(i, j int) bool { return result.Modules[i].ID < result.Modules[j].ID })
	for _, r := range result.Modules {
		if r.Error != "" {
			m.logf("module manager: revision %d: module %s %s: %s", d.Revision, r.ID, r.Action, r.Error)
		} else {
			m.logf("module manager: revision %d: module %s %s", d.Revision, r.ID, r.Action)
		}
	}
	return result, nil
}

// stopRoots 返回需要停掉的运行中模块（不含依赖者闭包），以及因配置已被拒绝而继续用旧配置运行的模块。
// 可选依赖恢复导致的重启不在这里预判，见 restartForRecoveredOptional。
func (m *Manager) stopRoots(d Desired, oldPlan, newPlan Plan) (roots []string, keptOnGood map[string]bool) {
	keptOnGood = map[string]bool{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.slots {
		if s.inst == nil {
			continue
		}
		cfg, keep := d.Modules[id]
		if !keep || !sameStrings(oldPlan.MissingOptional[id], newPlan.MissingOptional[id]) {
			roots = append(roots, id)
			continue
		}
		running := s.inst.ref.cfg
		switch {
		case cfg.Equal(running):
			// 配置回到当前运行的版本（如 坏 → 好 → 坏 中的“好”）：清掉过时的拒绝记录。
			if s.hasRejected {
				s.hasRejected, s.err = false, ""
			}
		case s.hasRejected && cfg.Equal(s.rejected):
			keptOnGood[id] = true
		default:
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)
	return roots, keptOnGood
}

// restartForRecoveredOptional 处理本次 Apply 未触碰、但因可选依赖未运行而降级的模块：只有当该
// 依赖在本次启动阶段确实运行起来后，才把它们（连同依赖者）按逆序停止、按顺序用原配置重启，
// 从而按依赖顺序接上恢复的模块。依赖仍未运行时不做任何事，避免无谓重启。
func (m *Manager) restartForRecoveredOptional(ctx context.Context, plan Plan, results []ModuleResult) []ModuleResult {
	touched := map[string]int{}
	for i, r := range results {
		touched[r.ID] = i
	}
	var recovered []string
	m.mu.RLock()
	for _, id := range plan.Order {
		s := m.slots[id]
		if s == nil || s.inst == nil {
			continue
		}
		if _, ok := touched[id]; ok {
			continue
		}
		for _, dep := range s.waitingOptional {
			if ds := m.slots[dep]; ds != nil && ds.inst != nil {
				recovered = append(recovered, id)
				break
			}
		}
	}
	m.mu.RUnlock()
	if len(recovered) == 0 {
		return results
	}
	set := plan.closureOfDependents(recovered)
	refs := map[string]cfgRef{}
	rejectErr := map[string]string{} // 仍在沿用旧配置的模块：重启会经 starting 清掉 err，需恢复
	for i := len(plan.Order) - 1; i >= 0; i-- {
		id := plan.Order[i]
		s := m.slot(id)
		if !set[id] || s == nil || !m.running(s) {
			continue
		}
		m.mu.RLock()
		refs[id] = s.inst.ref
		if s.hasRejected {
			rejectErr[id] = s.err
		}
		m.mu.RUnlock()
		if err := m.stopModule(s); err != nil {
			m.logf("module manager: stop %s: %v", id, err)
		}
	}
	for _, id := range plan.Order {
		ref, ok := refs[id]
		if !ok {
			continue
		}
		s := m.slot(id)
		res := m.startWithRollback(ctx, s, ref, plan, false)
		if res.Action == ActionStarted {
			res.Action = ActionRestarted
			if msg, ok := rejectErr[id]; ok {
				m.mu.Lock()
				s.err = msg
				m.mu.Unlock()
			}
		}
		i, wasTouched := touched[id]
		switch {
		case !wasTouched:
			results = append(results, res)
		case res.Action == ActionRestarted && results[i].Action != ActionStarted && results[i].Action != ActionRestarted:
			// 本次已回滚/沿用旧配置的模块只是多重启了一次：保留原结果，调用方仍能看到新配置未生效。
		default:
			results[i] = res
		}
	}
	return results
}

func (m *Manager) knownRejected(s *slot, cfg Config) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return s.hasRejected && s.hasGood && cfg.Equal(s.rejected)
}

func (m *Manager) pausedRetry(s *slot, cfg Config) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pausedRetryLocked(s, cfg)
}

func (m *Manager) pausedRetryLocked(s *slot, cfg Config) bool {
	return s.inst == nil && s.state == StateFailed && !s.retryable && cfg.Equal(s.attempted.cfg)
}

func (m *Manager) startKnownGood(ctx context.Context, s *slot, plan Plan) ModuleResult {
	m.mu.RLock()
	good, rejectErr := s.good, s.err
	m.mu.RUnlock()
	res := m.startWithRollback(ctx, s, good, plan, false)
	if res.Action == ActionStarted {
		m.mu.Lock()
		s.err = rejectErr
		m.mu.Unlock()
		return ModuleResult{ID: s.id, Action: ActionSkipped, Error: rejectErr}
	}
	return res
}

// startWithRollback 用 ref 启动模块，失败时回滚到上一份有效配置。clearRejected 为 true 表示
// ref 来自新的期望配置：启动成功即忘掉此前被拒绝的配置。
func (m *Manager) startWithRollback(ctx context.Context, s *slot, ref cfgRef, plan Plan, clearRejected bool) ModuleResult {
	if missing := m.missingRequired(s.desc); len(missing) > 0 {
		err := fmt.Sprintf("required module(s) not running: %s", strings.Join(missing, ", "))
		m.markFailed(s, ref, err, true)
		return ModuleResult{ID: s.id, Action: ActionFailed, Error: err}
	}
	degraded, waiting := m.degradedReasons(s.desc, plan)
	err := m.startModule(ctx, s, ref, degraded, waiting)
	if err == nil {
		if clearRejected {
			m.mu.Lock()
			s.hasRejected = false
			m.mu.Unlock()
		}
		return ModuleResult{ID: s.id, Action: ActionStarted}
	}
	m.mu.RLock()
	good, hasGood := s.good, s.hasGood
	m.mu.RUnlock()
	if !hasGood || good.cfg.Equal(ref.cfg) || good.cfg.SchemaVersion != ref.cfg.SchemaVersion {
		// 已知可用的配置启动失败多半是瞬时问题（端口未释放等）：下次 Apply 重试，而不是暂停。
		m.markFailed(s, ref, m.slotErr(s), hasGood && good.cfg.Equal(ref.cfg))
		return ModuleResult{ID: s.id, Action: ActionFailed, Error: m.slotErr(s)}
	}
	if rbErr := m.startModule(ctx, s, good, degraded, waiting); rbErr != nil {
		m.markFailed(s, ref, fmt.Sprintf("%v; rollback to previous config failed: %v", err, rbErr), true)
		return ModuleResult{ID: s.id, Action: ActionFailed, Error: m.slotErr(s)}
	}
	m.mu.Lock()
	s.err = err.Error()
	s.rejected, s.hasRejected = ref.cfg, true
	s.attempted = ref
	m.mu.Unlock()
	return ModuleResult{ID: s.id, Action: ActionRolledBack, Error: err.Error()}
}

// startModule 创建新实例并在启动超时内调用 Start；失败时清理该实例并把 slot 置为 failed。
func (m *Manager) startModule(ctx context.Context, s *slot, ref cfgRef, degraded, waiting []string) error {
	mod, desc, err := m.reg.newInstance(s.id)
	if err != nil {
		m.markFailed(s, ref, err.Error(), false)
		return err
	}
	if !m.transition(s, StateStarting, "") {
		return fmt.Errorf("module %s: cannot start from state %s", s.id, m.slotState(s))
	}
	shared := m.base
	shared.ConfigRevision, shared.ConfigDigest = ref.revision, ref.digest
	var mc *moduleContext
	mc = newModuleContext(s.id, ref.cfg, shared, desc.maxTasks(), func(string, any) {
		m.onTaskPanic(s, mc)
	})
	mc.logf = m.logf

	startCtx, cancel := context.WithTimeout(ctx, desc.startTimeout())
	startDone, err := runWithTimeout(startCtx, func() error { return mod.Start(startCtx, mc) })
	cancel()
	if err != nil {
		if cleanupErr := m.cleanupFailedStart(mod, mc, desc, startDone); cleanupErr != nil {
			m.logf("module manager: cleanup after failed start of %s: %v", s.id, cleanupErr)
		}
		err = fmt.Errorf("start: %w", err)
		m.markFailed(s, ref, err.Error(), false)
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.inst = &instance{mod: mod, mc: mc, desc: desc, ref: ref}
	s.desc = desc
	s.attempted, s.good, s.hasGood, s.retryable = ref, ref, true, false
	s.baseDegraded, s.waitingOptional = degraded, waiting
	// 与 onTaskPanic 在同一把锁下决定最终状态：启动期间已发生的任务 panic 也会体现为 degraded。
	next := StateReady
	if len(s.degradedReasonsLocked()) > 0 {
		next = StateDegraded
	}
	m.transitionLocked(s, next, "")
	return nil
}

// cleanupFailedStart 清理启动失败的实例：先取消托管任务，等 Start 真正返回后才调用 Stop
// （保证同一实例的生命周期调用串行），最后限时等待托管任务退出。
func (m *Manager) cleanupFailedStart(mod Module, mc *moduleContext, desc Descriptor, startDone <-chan struct{}) error {
	mc.cancelTasks()
	stopCtx, cancel := context.WithTimeout(context.Background(), desc.stopTimeout())
	defer cancel()
	select {
	case <-startDone:
	case <-stopCtx.Done():
		return fmt.Errorf("Start still running after timeout; instance abandoned without Stop")
	}
	_, stopErr := runWithTimeout(stopCtx, func() error { return mod.Stop(stopCtx) })
	return errors.Join(stopErr, mc.wait(stopCtx))
}

// stopModule 停止 slot 的运行实例：cancel → Stop → wait，均受停止超时约束。
func (m *Manager) stopModule(s *slot) error {
	m.mu.Lock()
	inst := s.inst
	if inst == nil {
		m.mu.Unlock()
		return nil
	}
	m.transitionLocked(s, StateStopping, "")
	m.mu.Unlock()

	inst.mc.cancelTasks()
	stopCtx, cancel := context.WithTimeout(context.Background(), inst.desc.stopTimeout())
	defer cancel()
	_, stopErr := runWithTimeout(stopCtx, func() error { return inst.mod.Stop(stopCtx) })
	err := errors.Join(stopErr, inst.mc.wait(stopCtx))

	m.mu.Lock()
	defer m.mu.Unlock()
	s.inst = nil
	if err != nil {
		m.transitionLocked(s, StateFailed, fmt.Sprintf("stop: %v", err))
		// 停止失败不代表配置有问题：之后应允许再次启动。
		s.retryable = true
		return err
	}
	m.transitionLocked(s, StateStopped, "")
	return nil
}

// Shutdown 按依赖逆序停止所有模块；之后 Apply 返回 ErrManagerClosed。可重复调用。
func (m *Manager) Shutdown() error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	var errs []error
	for i := len(m.plan.Order) - 1; i >= 0; i-- {
		s := m.slot(m.plan.Order[i])
		if s == nil || !m.running(s) {
			continue
		}
		if err := m.stopModule(s); err != nil {
			errs = append(errs, fmt.Errorf("module %s: %w", s.id, err))
		}
	}
	return errors.Join(errs...)
}

// Statuses 返回所有已启用模块的状态快照（按 ID 升序）。各模块 Health() 并行探测、总等待
// healthCheckTimeout；上一次探测仍未返回的模块直接报告 unhealthy，不再新起 goroutine。
func (m *Manager) Statuses() []Status {
	type probeResult struct {
		idx int
		h   Health
	}
	m.mu.Lock()
	out := make([]Status, 0, len(m.slots))
	results := make(chan probeResult, len(m.slots))
	pending := map[int]bool{}
	for _, s := range m.slots {
		st := Status{
			ID: s.id, Version: s.desc.Version, Source: s.desc.Source, APIVersion: s.desc.APIVersion, State: s.state, Error: s.err,
			DegradedReasons: s.degradedReasonsLocked(), ConfigRevision: s.attempted.revision, Since: s.since,
			Health: Health{Status: HealthUnknown},
		}
		idx := len(out)
		if inst := s.inst; inst != nil {
			st.ConfigRevision = inst.ref.revision
			if m.probing[inst.mc] {
				st.Health = Health{Status: HealthUnhealthy, Message: "previous health check still running"}
			} else {
				m.probing[inst.mc] = true
				pending[idx] = true
				go func() {
					var h Health
					if err := callSafely(func() error { h = inst.mod.Health(); return nil }); err != nil {
						h = Health{Status: HealthUnhealthy, Message: "health check failed: " + err.Error()}
					} else if h.Status == "" {
						h.Status = HealthUnknown
					}
					results <- probeResult{idx: idx, h: h}
					m.mu.Lock()
					delete(m.probing, inst.mc)
					m.mu.Unlock()
				}()
			}
		}
		out = append(out, st)
	}
	m.mu.Unlock()

	timer := time.NewTimer(healthCheckTimeout)
	defer timer.Stop()
	for len(pending) > 0 {
		select {
		case r := <-results:
			out[r.idx].Health = r.h
			delete(pending, r.idx)
		case <-timer.C:
			for idx := range pending {
				out[idx].Health = Health{Status: HealthUnhealthy, Message: "health check timed out"}
			}
			pending = nil
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Revision 返回最近一次成功校验并应用的配置修订号与摘要。
func (m *Manager) Revision() (uint64, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revision, m.digest
}

// onTaskPanic 在托管任务 panic 后把运行中的模块标为 degraded。启动尚未完成时什么都不做：
// panic 原因已记录在 mc 上，startModule 在同一把锁下汇总。
func (m *Manager) onTaskPanic(s *slot, mc *moduleContext) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.inst == nil || s.inst.mc != mc || s.state != StateReady {
		return
	}
	m.transitionLocked(s, StateDegraded, "")
}

func (s *slot) degradedReasonsLocked() []string {
	out := append([]string(nil), s.baseDegraded...)
	if s.inst != nil {
		out = append(out, s.inst.mc.panicReasons()...)
	}
	return out
}

func (m *Manager) transition(s *slot, to State, errMsg string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transitionLocked(s, to, errMsg)
}

// transitionLocked 执行合法的状态迁移并记录 err；非法迁移只记日志（属于框架缺陷），返回 false。
func (m *Manager) transitionLocked(s *slot, to State, errMsg string) bool {
	if !canTransition(s.state, to) {
		m.logf("module manager: BUG: invalid transition of %s from %s to %s", s.id, s.state, to)
		return false
	}
	s.state, s.since = to, m.now()
	if to == StateStarting {
		s.err = ""
	}
	if errMsg != "" {
		s.err = errMsg
	}
	return true
}

// markFailed 记录一次失败的启动尝试。只用于没有运行实例的 slot。
func (m *Manager) markFailed(s *slot, ref cfgRef, errMsg string, retryable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.inst != nil {
		m.logf("module manager: BUG: markFailed on running module %s", s.id)
		return
	}
	s.attempted, s.err, s.retryable = ref, errMsg, retryable
	s.state, s.since = StateFailed, m.now()
}

func (m *Manager) missingRequired(desc Descriptor) []string {
	var missing []string
	for _, dep := range desc.Requires {
		if dep.Optional {
			continue
		}
		if s := m.slot(dep.ID); s == nil || !m.running(s) {
			missing = append(missing, dep.ID)
		}
	}
	return missing
}

// degradedReasons 返回依赖相关的降级原因，以及已启用但未运行的可选依赖。
func (m *Manager) degradedReasons(desc Descriptor, plan Plan) (reasons, waiting []string) {
	var out []string
	for _, id := range plan.MissingOptional[desc.ID] {
		out = append(out, fmt.Sprintf("optional module %s not enabled", id))
	}
	for _, dep := range desc.Requires {
		if !dep.Optional {
			continue
		}
		if _, enabled := indexOf(plan.Order, dep.ID); !enabled {
			continue
		}
		if s := m.slot(dep.ID); s == nil || !m.running(s) {
			out = append(out, fmt.Sprintf("optional module %s not running", dep.ID))
			waiting = append(waiting, dep.ID)
		}
	}
	return out, waiting
}

func (m *Manager) slot(id string) *slot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.slots[id]
}

func (m *Manager) ensureSlot(id string) *slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.slots[id]; ok {
		return s
	}
	desc, _ := m.reg.Descriptor(id)
	s := &slot{id: id, desc: desc, state: StateStopped, since: m.now()}
	m.slots[id] = s
	return s
}

func (m *Manager) running(s *slot) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return s.inst != nil
}

func (m *Manager) slotState(s *slot) State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return s.state
}

func (m *Manager) slotErr(s *slot) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return s.err
}

// runWithTimeout 在独立 goroutine 中安全执行 fn，ctx 截止时返回 errTimeout（fn 被放弃，
// 但其返回时 done 仍会关闭，调用方可据此等待它真正结束）。
func runWithTimeout(ctx context.Context, fn func() error) (<-chan struct{}, error) {
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- callSafely(fn)
	}()
	select {
	case err := <-result:
		return done, err
	case <-ctx.Done():
		return done, errTimeout
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func indexOf(list []string, v string) (int, bool) {
	for i, s := range list {
		if s == v {
			return i, true
		}
	}
	return -1, false
}
