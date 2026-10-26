package module

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
)

var (
	// ErrContextClosed 模块已停止（或正在停止）后再提交后台任务。
	ErrContextClosed = errors.New("module context closed")
	// ErrTooManyTasks 后台任务数达到 Descriptor.MaxTasks 上限。
	ErrTooManyTasks = errors.New("too many module background tasks")
	// ErrTasksNotStopped 停止超时，仍有后台任务未退出。
	ErrTasksNotStopped = errors.New("module background tasks did not stop in time")
)

// SharedInfo 是所有模块都可读的公共只读信息，不含任何核心可变状态。
type SharedInfo struct {
	// RelayVersion 面向展示的版本串（可能是 "dev (commit: …)"）。
	RelayVersion string
	// CoreVersion 用于兼容校验的核心版本：正式构建为 tag（如 v1.4.0）；私有仓库组装时取
	// 所依赖公共核心模块的版本；dev 构建为非正式版本串。
	CoreVersion string
	// Instance relay 实例名（--analytics-instance），用于区分同机多实例。
	Instance string
	// ConfigRevision / ConfigDigest 标识模块本次启动所用的配置快照。
	ConfigRevision uint64
	ConfigDigest   string
}

// ModuleContext 是模块访问核心能力的唯一入口。
//
// 第一阶段只开放：自身只读配置、公共只读信息、结构化日志和受控后台任务。路由注册、
// 事件订阅、指标与受控服务调用会在后续阶段以新增方法的方式加入（只增不改），模块
// 不能通过它拿到连接、会话或 *relay.Server。
type ModuleContext interface {
	ModuleID() string
	// Config 返回本模块配置的只读副本。
	Config() Config
	Shared() SharedInfo
	// Logf 写一条带模块前缀的日志。不要把凭据等敏感配置写进日志。
	Logf(format string, args ...any)
	// Go 启动受控后台任务：受并发上限约束，panic 被捕获并记录，模块停止时 ctx 被取消，
	// 框架在 Stop 超时内等待其退出。
	Go(name string, fn func(ctx context.Context)) error
	// Done 在框架开始停止模块时（调用 Module.Stop 之前）关闭。
	Done() <-chan struct{}

	// sealed 把接口限定为只能由本包实现，后续阶段新增方法不会破坏任何外部实现。
	// 模块单测请用 NewTestContext。
	sealed()
}

// moduleContext 是 ModuleContext 的实现；每次 Start 新建一个，Stop 后作废。
type moduleContext struct {
	id     string
	cfg    Config
	shared SharedInfo
	logf   func(format string, args ...any)

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	running  int
	maxTasks int
	wg       sync.WaitGroup
	panics   []string // 最近 maxPanicReasons 条托管任务 panic 原因，供管理器汇总 degraded
	panicN   int      // 累计 panic 次数

	// onTaskPanic 由管理器注入，用于把模块标记为 degraded；可为 nil。
	onTaskPanic func(task string, recovered any)
}

func newModuleContext(id string, cfg Config, shared SharedInfo, maxTasks int, onTaskPanic func(string, any)) *moduleContext {
	ctx, cancel := context.WithCancel(context.Background())
	if maxTasks <= 0 {
		maxTasks = DefaultMaxTasks
	}
	return &moduleContext{
		id:          id,
		cfg:         cfg,
		shared:      shared,
		logf:        log.Printf,
		ctx:         ctx,
		cancel:      cancel,
		maxTasks:    maxTasks,
		onTaskPanic: onTaskPanic,
	}
}

func (c *moduleContext) ModuleID() string { return c.id }
func (c *moduleContext) Config() Config {
	return NewConfig(c.cfg.ModuleID, c.cfg.SchemaVersion, c.cfg.Bytes())
}
func (c *moduleContext) Shared() SharedInfo    { return c.shared }
func (c *moduleContext) Done() <-chan struct{} { return c.ctx.Done() }
func (c *moduleContext) sealed()               {}

func (c *moduleContext) Logf(format string, args ...any) {
	c.logf("module %s: %s", c.id, fmt.Sprintf(format, args...))
}

func (c *moduleContext) Go(name string, fn func(ctx context.Context)) error {
	if name == "" || fn == nil {
		return errors.New("module task requires a name and a function")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrContextClosed
	}
	if c.running >= c.maxTasks {
		c.mu.Unlock()
		return fmt.Errorf("%w (limit %d)", ErrTooManyTasks, c.maxTasks)
	}
	c.running++
	c.wg.Add(1)
	c.mu.Unlock()

	go func() {
		// 释放必须最先 defer（最后执行），即使下面的日志或回调再次 panic 也不泄漏槽位。
		defer func() {
			c.mu.Lock()
			c.running--
			c.mu.Unlock()
			c.wg.Done()
		}()
		defer func() {
			if r := recover(); r != nil {
				c.reportTaskPanic(name, r)
			}
		}()
		fn(c.ctx)
	}()
	return nil
}

// reportTaskPanic 记录任务 panic 并通知管理器；日志与回调自身的 panic 也被吞掉，
// 保证一个模块的异常不会越过本函数打垮 relay 进程。
func (c *moduleContext) reportTaskPanic(name string, r any) {
	c.mu.Lock()
	c.panicN++
	c.panics = append(c.panics, fmt.Sprintf("task %s panicked: %v", name, r))
	if len(c.panics) > maxPanicReasons {
		c.panics = append([]string(nil), c.panics[len(c.panics)-maxPanicReasons:]...)
	}
	c.mu.Unlock()
	func() {
		defer func() { _ = recover() }()
		c.Logf("task %s panicked: %v\n%s", name, r, debug.Stack())
	}()
	if c.onTaskPanic != nil {
		func() {
			defer func() { _ = recover() }()
			c.onTaskPanic(name, r)
		}()
	}
}

// cancelTasks 拒绝新任务并取消现有任务的 ctx（关闭 Done）。可重复调用。
// 管理器在调用 Module.Stop 之前先调用它，这样 Stop 里等待自身托管任务也不会死等。
func (c *moduleContext) cancelTasks() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
}

// wait 在 ctx 截止前等待所有托管任务退出。
func (c *moduleContext) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ErrTasksNotStopped
	}
}

// close = cancelTasks + wait。可重复调用。
func (c *moduleContext) close(ctx context.Context) error {
	c.cancelTasks()
	return c.wait(ctx)
}

// maxPanicReasons 每个实例保留的 panic 原因条数上限，防止 crash-loop 的任务让状态无限增长。
const maxPanicReasons = 16

// panicReasons 返回最近的托管任务 panic 原因副本；有被丢弃的旧记录时首条注明累计次数。
func (c *moduleContext) panicReasons() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.panicN == 0 {
		return nil
	}
	out := make([]string, 0, len(c.panics)+1)
	if dropped := c.panicN - len(c.panics); dropped > 0 {
		out = append(out, fmt.Sprintf("%d task panics in total (%d older omitted)", c.panicN, dropped))
	}
	return append(out, c.panics...)
}

// runningTasks 返回当前在运行的后台任务数（测试与可观测性用）。
func (c *moduleContext) runningTasks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// NewTestContext 构造一个供模块单测使用的 ModuleContext（ModuleContext 不允许包外实现）。
// 返回的 closeFn 取消并等待其托管任务，语义与框架停止模块时一致。
func NewTestContext(cfg Config, shared SharedInfo, maxTasks int) (mc ModuleContext, closeFn func(ctx context.Context) error) {
	c := newModuleContext(cfg.ModuleID, cfg, shared, maxTasks, nil)
	return c, c.close
}
