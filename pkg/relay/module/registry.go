package module

import (
	"fmt"
	"sort"
	"sync"
)

// Factory 创建一个新的模块实例。框架每次启动模块都会调用它，以免停掉的实例残留状态。
type Factory func() Module

type registryEntry struct {
	desc    Descriptor
	factory Factory
}

// Registry 是内置模块注册表。第一阶段只接受编译进二进制的工厂函数，不提供按路径加载
// 外部代码的入口；未注册的模块 ID 一律被配置校验拒绝。
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
}

// NewRegistry 创建空注册表（测试或独立 relay 实例使用）。
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]registryEntry)}
}

var builtinRegistry = NewRegistry()

// Builtin 返回进程级内置模块注册表。内置模块（公共仓库或私有仓库的源码）在各自包的 init 中
// 调用 RegisterBuiltin 接入。第一阶段公共仓库没有迁移任何功能，纯核心构建时它是空的。
func Builtin() *Registry { return builtinRegistry }

var (
	builtinErrMu sync.Mutex
	builtinErrs  []error
)

// RegisterBuiltin 把模块工厂注册到进程级内置注册表，供模块包在 init 中调用。
//
// 与 MustRegister 不同，它不在 init 中 panic：元数据非法、模块契约版本不受支持、ID 冲突等错误
// 被记录下来，由 relay 启动时统一报告并拒绝启动（BuiltinErrors），给出可读的失败原因。
func RegisterBuiltin(factory Factory) {
	if err := builtinRegistry.Register(factory); err != nil {
		builtinErrMu.Lock()
		builtinErrs = append(builtinErrs, err)
		builtinErrMu.Unlock()
	}
}

// BuiltinErrors 返回 RegisterBuiltin 记录的注册失败（按发生顺序）。
func BuiltinErrors() []error {
	builtinErrMu.Lock()
	defer builtinErrMu.Unlock()
	return append([]error(nil), builtinErrs...)
}

// CheckCore 对注册表中所有模块校验核心版本要求：返回已知不兼容的错误，以及因核心不是正式版本
// 而无法验证的提示。
func (r *Registry) CheckCore(coreVersion string) (notes []string, errs []error) {
	for _, id := range r.IDs() {
		d, _ := r.Descriptor(id)
		note, err := CheckCoreCompatibility(d, coreVersion)
		if err != nil {
			errs = append(errs, err)
		} else if note != "" {
			notes = append(notes, note)
		}
	}
	return notes, errs
}

// Register 注册一个模块工厂：实例化一次读取并校验其元数据，ID 重复时拒绝。
func (r *Registry) Register(factory Factory) error {
	if factory == nil {
		return fmt.Errorf("register module: nil factory")
	}
	desc, err := descriptorOf(factory)
	if err != nil {
		return fmt.Errorf("register module: %w", err)
	}
	if err := ValidateDescriptor(desc); err != nil {
		return fmt.Errorf("register module: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.entries[desc.ID]; ok {
		return fmt.Errorf("register module: %s already registered (existing source %s, new source %s)", desc.ID, prev.desc.Source, desc.Source)
	}
	r.entries[desc.ID] = registryEntry{desc: copyDescriptor(desc), factory: factory}
	return nil
}

// MustRegister 同 Register，失败时 panic（用于测试或自建注册表；内置模块请用 RegisterBuiltin）。
func (r *Registry) MustRegister(factory Factory) {
	if err := r.Register(factory); err != nil {
		panic(err)
	}
}

// Descriptor 返回已注册模块元数据的副本。
func (r *Registry) Descriptor(id string) (Descriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok {
		return Descriptor{}, false
	}
	return copyDescriptor(e.desc), true
}

// IDs 返回所有已注册模块 ID（升序）。
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// newInstance 创建模块新实例并核对其 ID；框架始终使用注册时保存的元数据副本，不信任实例后续返回的 Descriptor。
func (r *Registry) newInstance(id string) (Module, Descriptor, error) {
	r.mu.RLock()
	e, ok := r.entries[id]
	r.mu.RUnlock()
	if !ok {
		return nil, Descriptor{}, fmt.Errorf("module %s is not registered", id)
	}
	var mod Module
	err := callSafely(func() error {
		mod = e.factory()
		if mod == nil {
			return fmt.Errorf("factory returned nil")
		}
		if got := mod.Descriptor().ID; got != id {
			return fmt.Errorf("factory returned module %q", got)
		}
		return nil
	})
	if err != nil {
		return nil, Descriptor{}, fmt.Errorf("module %s: %w", id, err)
	}
	return mod, copyDescriptor(e.desc), nil
}

func descriptorOf(factory Factory) (Descriptor, error) {
	var desc Descriptor
	err := callSafely(func() error {
		mod := factory()
		if mod == nil {
			return fmt.Errorf("factory returned nil")
		}
		desc = mod.Descriptor()
		return nil
	})
	return desc, err
}

func copyDescriptor(d Descriptor) Descriptor {
	d.ConfigVersions = append([]int(nil), d.ConfigVersions...)
	d.Requires = append([]Dependency(nil), d.Requires...)
	d.Conflicts = append([]string(nil), d.Conflicts...)
	return d
}

// callSafely 执行 fn，把 panic 转成 error：模块代码的任何异常都不能越过框架边界。
func callSafely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}
