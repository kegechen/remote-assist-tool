package config

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/pkg/relay/module"
)

// testModule 是只记录启停的最小内置模块；配置 {"fail":true} 让 Start 失败。
type testModule struct {
	id  string
	rec *recorder
}

type testModuleCfg struct {
	Value string `json:"value,omitempty"`
	Fail  bool   `json:"fail,omitempty"`
}

type recorder struct {
	mu     sync.Mutex
	events []string
	// blockValidate 模拟依赖外部环境的前置条件：为 true 时 Validate 失败。
	blockValidate bool
	// startGate 非 nil 时 Start 阻塞直到它被关闭。
	startGate chan struct{}
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func (m *testModule) Descriptor() module.Descriptor {
	return module.Descriptor{ID: m.id, Version: "1.0.0", Source: module.SourceCore, APIVersion: module.APIVersion, StartTimeout: time.Second, StopTimeout: time.Second}
}

func (m *testModule) Validate(cfg module.Config) error {
	var c testModuleCfg
	if err := cfg.Decode(&c); err != nil {
		return err
	}
	m.rec.mu.Lock()
	blocked := m.rec.blockValidate
	m.rec.mu.Unlock()
	if blocked {
		return errors.New("output directory not available")
	}
	return nil
}

func (m *testModule) Start(ctx context.Context, mc module.ModuleContext) error {
	var c testModuleCfg
	if err := mc.Config().Decode(&c); err != nil {
		return err
	}
	m.rec.add("start:" + m.id + ":" + c.Value)
	m.rec.mu.Lock()
	gate := m.rec.startGate
	m.rec.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if c.Fail {
		return errors.New("start failed")
	}
	return nil
}

func (m *testModule) Stop(ctx context.Context) error {
	m.rec.add("stop:" + m.id)
	return nil
}

func (m *testModule) Health() module.Health { return module.Health{Status: module.HealthOK} }

// newTestStack 返回注册了 audit/notice 两个测试模块的管理器与 Reloader。
func newTestStack(t *testing.T) (*Reloader, *module.Manager, *recorder, *[]string) {
	t.Helper()
	rec := &recorder{}
	reg := module.NewRegistry()
	for _, id := range []string{"audit", "notice"} {
		id := id
		if err := reg.Register(func() module.Module { return &testModule{id: id, rec: rec} }); err != nil {
			t.Fatal(err)
		}
	}
	mgr := module.NewManager(reg, module.SharedInfo{RelayVersion: "test"})
	t.Cleanup(func() { _ = mgr.Shutdown() })
	r := NewReloader(mgr)
	var logs []string
	var mu sync.Mutex
	r.logf = func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, format)
		mu.Unlock()
	}
	return r, mgr, rec, &logs
}
