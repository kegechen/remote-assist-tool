package module

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCfg 用配置驱动假模块的行为，便于在一次 Apply 中构造各种失败。
type fakeCfg struct {
	Value    string `json:"value,omitempty"`
	Invalid  bool   `json:"invalid,omitempty"`   // Validate 报错
	Fail     bool   `json:"fail,omitempty"`      // Start 返回错误
	Hang     bool   `json:"hang,omitempty"`      // Start 阻塞直到超时
	HangHard bool   `json:"hang_hard,omitempty"` // Start 无视 ctx 一直阻塞（直到测试结束）
	Panic    bool   `json:"panic,omitempty"`     // Start panic
	StopHang bool   `json:"stop_hang,omitempty"` // Stop 阻塞直到超时
	Task     string `json:"task,omitempty"`      // "loop" / "panic" / "panic_during_start"
	Health   string `json:"health,omitempty"`    // ok/degraded/""/"block"/"panic"
}

type fakeEnv struct {
	mu         sync.Mutex
	events     []string
	release    chan struct{}  // 测试结束时关闭，放行所有阻塞的假模块
	failStarts map[string]int // 与配置无关的瞬时启动失败次数
	onStart    func(id string)
}

func newFakeEnv(t *testing.T) *fakeEnv {
	env := &fakeEnv{release: make(chan struct{}), failStarts: map[string]int{}}
	t.Cleanup(func() { close(env.release) })
	return env
}

func (e *fakeEnv) record(format string, args ...any) {
	e.mu.Lock()
	e.events = append(e.events, fmt.Sprintf(format, args...))
	e.mu.Unlock()
}

// take 返回并清空已记录事件。
func (e *fakeEnv) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.events
	e.events = nil
	return out
}

type fakeModule struct {
	env  *fakeEnv
	desc Descriptor
	cfg  fakeCfg
}

func (f *fakeModule) Descriptor() Descriptor { return f.desc }

func (f *fakeModule) Validate(cfg Config) error {
	var c fakeCfg
	if err := cfg.Decode(&c); err != nil {
		return err
	}
	if c.Invalid {
		return errors.New("invalid by request")
	}
	return nil
}

func (f *fakeModule) Start(ctx context.Context, mc ModuleContext) error {
	if err := mc.Config().Decode(&f.cfg); err != nil {
		return err
	}
	f.env.record("start:%s:%s", f.desc.ID, f.cfg.Value)
	f.env.mu.Lock()
	hook := f.env.onStart
	transient := f.env.failStarts[f.desc.ID] > 0
	if transient {
		f.env.failStarts[f.desc.ID]--
	}
	f.env.mu.Unlock()
	if hook != nil {
		hook(f.desc.ID)
	}
	if transient {
		return errors.New("transient start failure")
	}
	switch {
	case f.cfg.Hang:
		select {
		case <-ctx.Done():
		case <-f.env.release:
		}
		f.env.record("start-returned:%s", f.desc.ID)
		return ctx.Err()
	case f.cfg.HangHard:
		<-f.env.release
		return nil
	case f.cfg.Panic:
		panic("start panic")
	case f.cfg.Fail:
		return errors.New("start failed by request")
	}
	switch f.cfg.Task {
	case "loop":
		return mc.Go("loop", func(ctx context.Context) { <-ctx.Done() })
	case "panic":
		return mc.Go("boom", func(ctx context.Context) { panic("task panic") })
	case "panic_during_start":
		// 需 MaxTasks=1：probe 能提交成功说明 boom 已完整走完 panic 上报与槽位释放。
		if err := mc.Go("boom", func(ctx context.Context) { panic("task panic") }); err != nil {
			return err
		}
		for mc.Go("probe", func(ctx context.Context) {}) != nil {
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

func (f *fakeModule) Stop(ctx context.Context) error {
	f.env.record("stop:%s", f.desc.ID)
	if f.cfg.StopHang {
		select {
		case <-ctx.Done():
		case <-f.env.release:
		}
		return ctx.Err()
	}
	return nil
}

func (f *fakeModule) Health() Health {
	switch f.cfg.Health {
	case "block":
		<-f.env.release
	case "panic":
		panic("health panic")
	case "":
		return Health{}
	}
	return Health{Status: HealthStatus(f.cfg.Health)}
}

func registerFake(t *testing.T, reg *Registry, env *fakeEnv, desc Descriptor) {
	t.Helper()
	if desc.Source == "" {
		desc.Source = SourceCore
	}
	if desc.APIVersion == 0 {
		desc.APIVersion = APIVersion
	}
	if desc.Version == "" {
		desc.Version = "1.0.0"
	}
	if desc.StartTimeout == 0 {
		desc.StartTimeout = 100 * time.Millisecond
	}
	if desc.StopTimeout == 0 {
		desc.StopTimeout = 100 * time.Millisecond
	}
	if err := reg.Register(func() Module { return &fakeModule{env: env, desc: desc} }); err != nil {
		t.Fatalf("register %s: %v", desc.ID, err)
	}
}

// desired 构造期望状态；mods 形如 "id" 或 "id=<json>"。
func desired(rev uint64, mods ...string) Desired {
	d := Desired{Revision: rev, Digest: fmt.Sprintf("digest-%d", rev), SchemaVersion: CurrentConfigVersion, Modules: map[string]Config{}}
	for _, m := range mods {
		id, raw, _ := strings.Cut(m, "=")
		if raw != "" && !json.Valid([]byte(raw)) {
			panic("bad json in test: " + raw)
		}
		d.Modules[id] = NewConfig(id, CurrentConfigVersion, []byte(raw))
	}
	return d
}

func newTestManager(reg *Registry) *Manager {
	m := NewManager(reg, SharedInfo{RelayVersion: "v1.0.0", Instance: "default"})
	m.logf = func(string, ...any) {}
	return m
}
