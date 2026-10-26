package config

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/remote-assist/tool/pkg/relay/module"
)

// DefaultHistory 保留的最近有效配置版本数，用于手动回滚。
const DefaultHistory = 5

// Applier 是 Reloader 驱动的模块管理器（module.Manager 实现了它）。
// Apply 必须先完整校验期望状态，校验失败时返回 error 且不改变任何模块。
type Applier interface {
	Apply(ctx context.Context, d module.Desired) (module.ApplyResult, error)
}

// ReloadOutcome 是一次加载尝试的结果。
type ReloadOutcome string

const (
	OutcomeApplied   ReloadOutcome = "applied"   // 新配置已校验并应用（模块级失败见 Result）
	OutcomeUnchanged ReloadOutcome = "unchanged" // 内容摘要与当前配置相同，或与回滚时固定的文件内容相同
	OutcomeRejected  ReloadOutcome = "rejected"  // 读取/解析/校验失败，继续使用旧配置
	OutcomePending   ReloadOutcome = "pending"   // 文件仍在写入，稍后由监听器重试；不计入状态
)

// ReloadReport 描述一次加载尝试。
type ReloadReport struct {
	// ReloadID 每次加载尝试唯一的流水号，相关日志都带上它。
	ReloadID uint64
	Outcome  ReloadOutcome
	Revision uint64 // 当前生效的配置修订号（rejected/unchanged 时为原修订号）
	Digest   string
	Result   module.ApplyResult
	Err      error
}

// Status 是热加载的可观测状态。
type Status struct {
	Revision      uint64
	Digest        string
	Source        string
	LoadedAt      time.Time
	LastAttemptAt time.Time
	LastReloadID  uint64
	LastOutcome   ReloadOutcome
	LastError     string
	// RollbackPinned 手动回滚后，磁盘上的文件内容暂不自动应用，直到文件内容再次变化。
	RollbackPinned bool
	// History 保留的有效配置修订号（旧 → 新），可用于 Rollback。
	History []uint64
}

// Reloader 把配置内容变成已应用的模块状态：解析 → 校验（含模块依赖与模块自身校验）→ 应用 →
// 提交为当前快照。任何一步失败都保留最近一次有效配置。
//
// 加载尝试由 opMu 串行化（Apply 可能耗时数十秒）；状态字段由 mu 保护且只短暂持有，
// 因此 Status/Current 不会被正在进行的加载阻塞。
type Reloader struct {
	applier Applier
	logf    func(format string, args ...any)
	now     func() time.Time
	keep    int

	opMu sync.Mutex

	mu           sync.Mutex
	current      *Snapshot
	history      []*Snapshot
	nextRevision uint64
	nextReloadID uint64

	lastAttemptAt time.Time
	lastOutcome   ReloadOutcome
	lastErr       error
	// lastErrDigest/lastErrParse：最近一次失败的内容摘要，以及失败是否发生在解析阶段。
	// 只有解析失败（内容本身坏）才跳过重复处理；校验/应用失败可能取决于外部环境或瞬时状态，
	// 同内容再来时仍会重试，只是不重复记日志。
	lastErrDigest string
	lastErrParse  bool
	// pinned 手动回滚时磁盘文件内容的摘要：该内容不再被自动应用，直到文件变成别的内容。
	pinned string
}

// NewReloader 创建热加载编排器。
func NewReloader(applier Applier) *Reloader {
	return &Reloader{applier: applier, logf: log.Printf, now: time.Now, keep: DefaultHistory, nextRevision: 1}
}

// Load 尝试加载一份配置内容。
func (r *Reloader) Load(ctx context.Context, data []byte, source string) ReloadReport {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	id := r.begin()
	digest := Digest(data)

	r.mu.Lock()
	if r.pinned != "" && r.pinned != digest {
		r.pinned = ""
	}
	switch {
	case r.current != nil && r.current.digest == digest, r.pinned == digest:
		r.lastOutcome, r.lastErr, r.lastErrDigest, r.lastErrParse = OutcomeUnchanged, nil, "", false
		rev, cur := r.current.revision, r.current.digest
		r.mu.Unlock()
		return ReloadReport{ReloadID: id, Outcome: OutcomeUnchanged, Revision: rev, Digest: cur}
	case r.lastErr != nil && r.lastErrParse && r.lastErrDigest == digest:
		// 同一份格式错误的内容：不重复解析、不重复报错，等待内容变化。
		err := r.lastErr
		r.mu.Unlock()
		return r.reject(id, digest, err, true)
	}
	r.mu.Unlock()

	snap, err := Parse(data, source)
	if err != nil {
		return r.reject(id, digest, err, true)
	}
	return r.apply(ctx, id, snap, digest)
}

// Rollback 把历史中的某个有效配置重新应用为新修订（手动回滚）。回滚后磁盘上当前的文件内容
// 不会被监听器的定期重读再次应用，直到文件内容真正变化。
func (r *Reloader) Rollback(ctx context.Context, revision uint64) ReloadReport {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	id := r.begin()
	r.mu.Lock()
	var target *Snapshot
	for _, h := range r.history {
		if h.revision == revision {
			target = h
		}
	}
	if target != nil && r.current != nil && r.current.digest == target.digest {
		rev := r.current.revision
		r.lastOutcome = OutcomeUnchanged
		r.mu.Unlock()
		return ReloadReport{ReloadID: id, Outcome: OutcomeUnchanged, Revision: rev, Digest: target.digest}
	}
	var fileDigest string
	if r.current != nil {
		fileDigest = r.current.digest
	}
	if r.pinned != "" {
		fileDigest = r.pinned // 连续回滚：文件内容仍是第一次回滚时固定的那份
	}
	if r.lastErrDigest != "" {
		// 当前文件内容是一份被拒绝的配置：同样固定它，避免回滚后被自动重试覆盖。
		fileDigest = r.lastErrDigest
	}
	r.mu.Unlock()
	if target == nil {
		return r.reject(id, "", fmt.Errorf("revision %d is not in the retained history", revision), false)
	}
	r.logf("module config reload #%d: manual rollback to revision %d", id, revision)
	rep := r.apply(ctx, id, target, "")
	if rep.Outcome == OutcomeApplied {
		r.mu.Lock()
		r.pinned = fileDigest
		r.mu.Unlock()
	}
	return rep
}

// Reject 记录一次无法读取配置内容的失败（例如文件被删除或不可读），继续使用旧配置。
func (r *Reloader) Reject(err error) ReloadReport {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.reject(r.begin(), "", err, false)
}

// Pending 记录一次因文件仍在写入而推迟的尝试；不改变状态，只返回报告。
func (r *Reloader) Pending(err error) ReloadReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	rev, cur := uint64(0), ""
	if r.current != nil {
		rev, cur = r.current.revision, r.current.digest
	}
	return ReloadReport{Outcome: OutcomePending, Revision: rev, Digest: cur, Err: err}
}

func (r *Reloader) begin() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextReloadID++
	r.lastAttemptAt = r.now()
	return r.nextReloadID
}

// apply 应用快照。contentDigest 为本次文件内容摘要（回滚时为空），用于失败去重。
func (r *Reloader) apply(ctx context.Context, id uint64, snap *Snapshot, contentDigest string) ReloadReport {
	r.mu.Lock()
	candidate := snap.withRevision(r.nextRevision, r.now())
	r.mu.Unlock()
	desired := candidate.Desired()
	result, err := r.applier.Apply(ctx, desired)
	if err != nil {
		// Apply 只在校验失败（未知模块、依赖、模块配置）、ctx 已取消或管理器关闭时报错，此时模块状态未被修改。
		return r.reject(id, contentDigest, err, false)
	}
	r.mu.Lock()
	r.nextRevision++
	r.current = candidate
	r.history = append(r.history, candidate)
	if len(r.history) > r.keep {
		r.history = append([]*Snapshot(nil), r.history[len(r.history)-r.keep:]...)
	}
	r.lastOutcome, r.lastErr, r.lastErrDigest, r.lastErrParse = OutcomeApplied, nil, "", false
	r.mu.Unlock()
	if failed := result.Failed(); len(failed) > 0 {
		r.logf("module config reload #%d: revision %d applied (digest %.12s); modules not running the new config: %v", id, candidate.revision, candidate.digest, failed)
	} else {
		r.logf("module config reload #%d: revision %d applied (digest %.12s, %d enabled module(s))", id, candidate.revision, candidate.digest, len(desired.Modules))
	}
	return ReloadReport{ReloadID: id, Outcome: OutcomeApplied, Revision: candidate.revision, Digest: candidate.digest, Result: result}
}

// reject 记录失败并保留当前配置。与上一次失败的摘要和错误完全相同时不重复记日志。
func (r *Reloader) reject(id uint64, digest string, err error, parseErr bool) ReloadReport {
	r.mu.Lock()
	repeated := r.lastErr != nil && r.lastErrDigest == digest && r.lastErr.Error() == err.Error()
	r.lastOutcome, r.lastErr, r.lastErrDigest, r.lastErrParse = OutcomeRejected, err, digest, parseErr
	rev, cur := uint64(0), ""
	if r.current != nil {
		rev, cur = r.current.revision, r.current.digest
	}
	hasCurrent := r.current != nil
	r.mu.Unlock()
	// relay 正在退出导致的取消不是配置问题，不记日志（状态里仍保留原因）。
	if !repeated && !errors.Is(err, context.Canceled) {
		if hasCurrent {
			r.logf("module config reload #%d rejected: %v (keeping revision %d)", id, err, rev)
		} else {
			r.logf("module config reload #%d rejected: %v (no module configuration active)", id, err)
		}
	}
	return ReloadReport{ReloadID: id, Outcome: OutcomeRejected, Revision: rev, Digest: cur, Err: err}
}

// Current 返回当前生效的配置快照；尚未成功加载时为 nil。
func (r *Reloader) Current() *Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// Status 返回热加载状态快照。
func (r *Reloader) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{LastAttemptAt: r.lastAttemptAt, LastReloadID: r.nextReloadID, LastOutcome: r.lastOutcome, RollbackPinned: r.pinned != ""}
	if r.lastErr != nil {
		st.LastError = r.lastErr.Error()
	}
	if r.current != nil {
		st.Revision, st.Digest, st.Source, st.LoadedAt = r.current.revision, r.current.digest, r.current.source, r.current.loadedAt
	}
	for _, h := range r.history {
		st.History = append(st.History, h.revision)
	}
	return st
}

// ErrNoConfig 表示配置文件不存在。
var ErrNoConfig = errors.New("module config file does not exist")
