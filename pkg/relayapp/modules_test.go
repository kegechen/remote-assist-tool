package relayapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	relayconfig "github.com/remote-assist/tool/internal/relay/config"
	"github.com/remote-assist/tool/pkg/relay/module"
)

func TestModuleConfigFlag(t *testing.T) {
	opts, err := parseRelayOptions(nil, &bytes.Buffer{})
	if err != nil || opts.moduleConfig != "" {
		t.Fatalf("default module-config = %q, %v", opts.moduleConfig, err)
	}
	if _, err := parseRelayOptions([]string{"--module-config", "modules.json"}, io.Discard); err == nil {
		t.Fatal("relative module-config accepted")
	}
	abs := filepath.Join(t.TempDir(), "modules.json")
	opts, err = parseRelayOptions([]string{"--module-config", abs}, io.Discard)
	if err != nil || opts.moduleConfig != abs {
		t.Fatalf("absolute module-config: %q, %v", opts.moduleConfig, err)
	}
}

func TestModuleFrameworkDisabledWithoutConfig(t *testing.T) {
	stop := startModuleFramework(context.Background(), relayOptions{})
	stop()
	stop()
}

// demoModule 是只在测试里注册的内置模块，用来验证 relay 接线的加载、热加载与停止。
type demoModule struct{ events *demoEvents }

type demoEvents struct {
	mu  sync.Mutex
	log []string
}

func (e *demoEvents) add(s string) {
	e.mu.Lock()
	e.log = append(e.log, s)
	e.mu.Unlock()
}

func (e *demoEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

func (m *demoModule) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "demo", Version: "1.0.0", Source: "example.com/private/relay-modules", APIVersion: module.APIVersion}
}

func (m *demoModule) Validate(cfg module.Config) error {
	var c struct {
		Value string `json:"value"`
	}
	return cfg.Decode(&c)
}

func (m *demoModule) Start(ctx context.Context, mc module.ModuleContext) error {
	var c struct {
		Value string `json:"value"`
	}
	if err := mc.Config().Decode(&c); err != nil {
		return err
	}
	m.events.add("start:" + c.Value)
	return nil
}

func (m *demoModule) Stop(ctx context.Context) error {
	m.events.add("stop")
	return nil
}

func (m *demoModule) Health() module.Health { return module.Health{Status: module.HealthOK} }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeModuleConfig(t *testing.T, path, content string, step int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 10, 24, 12, 0, step, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestModuleFrameworkLoadsReloadsAndStops(t *testing.T) {
	events := &demoEvents{}
	reg := module.NewRegistry()
	reg.MustRegister(func() module.Module { return &demoModule{events: events} })
	path := filepath.Join(t.TempDir(), "modules.json")
	writeModuleConfig(t, path, `{"version":1,"modules":{"demo":{"value":"v1"}}}`, 1)
	fast := relayconfig.WatchOptions{PollInterval: 10 * time.Millisecond, Debounce: 30 * time.Millisecond, StableDelay: time.Millisecond}
	f := newModuleFramework(context.Background(), relayOptions{moduleConfig: path}, reg, fast)
	if f == nil {
		t.Fatal("framework not started")
	}
	waitFor(t, "initial load", func() bool { return f.reloader.Status().Revision == 1 })

	// 坏配置：被拒绝，继续使用旧配置，模块不受影响。
	writeModuleConfig(t, path, `{"version":1,"modules":{"demo":{"valu":"typo"}}}`, 2)
	waitFor(t, "bad config rejected", func() bool {
		st := f.reloader.Status()
		return st.LastOutcome == relayconfig.OutcomeRejected && strings.Contains(st.LastError, "valu")
	})
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{"start:v1"}) {
		t.Fatalf("invalid config touched the module: %v", got)
	}
	writeModuleConfig(t, path, `{"version":1,"modules":{"demo":{"value":"v2"}}}`, 3)
	waitFor(t, "reload", func() bool { return f.reloader.Status().Revision == 2 })
	f.stop()
	f.stop()
	if got, want := events.snapshot(), []string{"start:v1", "stop", "start:v2", "stop"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// syncBuffer 收集 relay 日志，供测试等待特定日志行。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// 统一模块配置不存在、无效或引用未知模块时，relay 照常启动和服务，只记录拒绝原因。
func TestRunRelayStartsWithBrokenModuleConfig(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, content, wantLog string }{
		{"missing.json", "", "does not exist"},
		{"invalid.json", `{"version":1,"modules":`, "parse config"},
		{"unknown.json", `{"version":1,"modules":{"no_such_module":{}}}`, `unknown module "no_such_module"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := &syncBuffer{}
			prev := log.Writer()
			log.SetOutput(logs)
			defer log.SetOutput(prev)
			path := filepath.Join(dir, c.name)
			if c.content != "" {
				if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{})
			errc := make(chan error, 1)
			args := []string{
				"--plain", "--listen", "127.0.0.1:0", "--audit", "", "--module-config", path,
			}
			go func() { errc <- runRelayWithReady(ctx, args, io.Discard, io.Discard, func() { close(ready) }) }()
			select {
			case <-ready:
			case err := <-errc:
				t.Fatalf("relay exited before ready: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("relay not ready")
			}
			waitFor(t, "module config rejection log", func() bool {
				out := logs.String()
				return strings.Contains(out, "module config reload #1 rejected") && strings.Contains(out, c.wantLog)
			})
			select {
			case err := <-errc:
				t.Fatalf("relay stopped after module config rejection: %v", err)
			default:
			}
			cancel()
			select {
			case err := <-errc:
				if err != nil {
					t.Fatalf("relay returned %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("relay did not stop")
			}
		})
	}
}

type incompatibleCoreModule struct{ demoModule }

func (*incompatibleCoreModule) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "future", Version: "1.0.0", Source: "example.com/private/relay-modules", APIVersion: module.APIVersion, MinCoreVersion: "v9.0.0"}
}

func TestCheckBuiltinModules(t *testing.T) {
	reg := module.NewRegistry()
	if err := checkBuiltinModules(reg, nil, "dev"); err != nil {
		t.Fatalf("empty registry (pure core build) rejected: %v", err)
	}
	reg.MustRegister(func() module.Module { return &incompatibleCoreModule{} })
	if err := checkBuiltinModules(reg, nil, "dev"); err != nil {
		t.Fatalf("unverifiable core must only warn: %v", err)
	}
	err := checkBuiltinModules(reg, nil, "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "requires relay core >= v9.0.0") {
		t.Fatalf("incompatible core accepted: %v", err)
	}
	regErr := errors.New("register module: module legacy is built for module API v99")
	if err := checkBuiltinModules(module.NewRegistry(), []error{regErr}, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "module API v99") {
		t.Fatalf("registration error not reported: %v", err)
	}
}

func TestListBuiltinModules(t *testing.T) {
	var out bytes.Buffer
	if err := listBuiltinModules(&out, module.NewRegistry(), nil, "v1.2.0"); err != nil || !strings.Contains(out.String(), "built-in modules: none") {
		t.Fatalf("%v %s", err, out.String())
	}
	reg := module.NewRegistry()
	reg.MustRegister(func() module.Module { return &demoModule{} })
	reg.MustRegister(func() module.Module { return &incompatibleCoreModule{} })
	out.Reset()
	err := listBuiltinModules(&out, reg, nil, "v1.2.0")
	text := out.String()
	if err == nil || !strings.Contains(text, "module demo 1.0.0 source=example.com/private/relay-modules api=v1 [ok]") || !strings.Contains(text, "INCOMPATIBLE") {
		t.Fatalf("%v\n%s", err, text)
	}
}

func TestRunRelayListModulesOnPureCore(t *testing.T) {
	var out bytes.Buffer
	if err := runRelay(context.Background(), []string{"--list-modules"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "built-in modules: none") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestResolveCoreVersion(t *testing.T) {
	const private = "example.com/private/relay-modules"
	tests := []struct {
		name, main, dep, injected, want string
		warn                            bool
	}{
		{"public dev build", coreModulePath, "", "dev", "dev", false},
		{"public release build", coreModulePath, "", "v1.4.0", "v1.4.0", false},
		{"private uses go.mod core version", private, "v1.4.0", "dev", "v1.4.0", false},
		{"go.mod wins over injected", private, "v1.4.0", "v9.0.0", "v1.4.0", true},
		{"same injected no warning", private, "v1.4.0", "v1.4.0", "v1.4.0", false},
		{"injected without v prefix", private, "v1.4.0", "1.4.0", "v1.4.0", false},
		{"directory replace falls back to injected", private, "", "v1.0.0", "v1.0.0", false},
		{"pseudo-version falls back to injected", private, "v0.0.0-20261024000000-abcdef123456", "v1.0.0", "v1.0.0", false},
		{"pseudo-version without injection", private, "v0.0.0-20261024000000-abcdef123456", "dev", "v0.0.0-20261024000000-abcdef123456", false},
		{"devel without injection", private, "(devel)", "dev", "dev", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warn := resolveCoreVersion(tt.main, tt.dep, tt.injected)
			if got != tt.want || (warn != "") != tt.warn {
				t.Fatalf("resolveCoreVersion(%q, %q, %q) = %q, %q; want %q, warn=%v", tt.main, tt.dep, tt.injected, got, warn, tt.want, tt.warn)
			}
		})
	}
}
