package relayapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"strings"
	"sync"

	relayconfig "github.com/remote-assist/tool/internal/relay/config"
	"github.com/remote-assist/tool/internal/update"
	"github.com/remote-assist/tool/internal/version"
	"github.com/remote-assist/tool/pkg/relay/module"
)

// moduleFramework 是 relay 进程内的模块框架实例：内置模块管理器 + 统一配置热加载。
type moduleFramework struct {
	manager  *module.Manager
	reloader *relayconfig.Reloader
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// startModuleFramework 在 --module-config 非空时启动模块框架：后台加载并持续监听统一模块配置，
// 交给内置模块管理器应用。返回的函数停止监听并按依赖逆序停止所有模块，可重复（含并发）调用。
//
// 首次加载也在后台进行，不推迟 relay 开始服务；任何配置或模块错误都只记日志。
func startModuleFramework(ctx context.Context, opts relayOptions) (stop func()) {
	f := newModuleFramework(ctx, opts, module.Builtin(), relayconfig.WatchOptions{Path: opts.moduleConfig})
	if f == nil {
		return func() {}
	}
	return f.stop
}

// newModuleFramework 未配置 --module-config 或路径无效时返回 nil（不创建任何框架对象）。
func newModuleFramework(ctx context.Context, opts relayOptions, reg *module.Registry, watch relayconfig.WatchOptions) *moduleFramework {
	if opts.moduleConfig == "" {
		return nil
	}
	mgr := module.NewManager(reg, module.SharedInfo{RelayVersion: version.Info(), CoreVersion: coreVersion()})
	reloader := relayconfig.NewReloader(mgr)
	watch.Path = opts.moduleConfig
	watcher, err := relayconfig.NewWatcher(watch, reloader)
	if err != nil {
		log.Printf("module framework disabled: %v", err)
		return nil
	}
	if ids := reg.IDs(); len(ids) == 0 {
		log.Printf("Module config: %s (no built-in modules in this build)", opts.moduleConfig)
	} else {
		log.Printf("Module config: %s (built-in modules: %v)", opts.moduleConfig, ids)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	f := &moduleFramework{manager: mgr, reloader: reloader, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		defer func() {
			// 框架自身的缺陷也不能带倒 relay：记录后停止监听，核心继续服务。
			if r := recover(); r != nil {
				log.Printf("module framework stopped after panic: %v\n%s", r, debug.Stack())
			}
		}()
		if watchCtx.Err() != nil {
			return // relay 已在退出，不必再加载
		}
		watcher.LoadInitial(watchCtx)
		watcher.Run(watchCtx)
	}()
	return f
}

// stop 停止监听，等待进行中的加载结束，再按依赖逆序停止所有模块。
func (f *moduleFramework) stop() {
	f.stopOnce.Do(func() {
		f.cancel()
		<-f.done
		if err := f.manager.Shutdown(); err != nil {
			log.Printf("module shutdown: %v", err)
		}
	})
}

// coreModulePath 是公共核心的 Go 模块路径。
const coreModulePath = "github.com/remote-assist/tool"

// coreVersion 返回用于模块兼容校验的核心版本，见 resolveCoreVersion。
func coreVersion() string {
	mainPath, dep := readBuildInfo()
	v, _ := resolveCoreVersion(mainPath, dep, version.Version)
	return v
}

// coreVersionWarning 返回核心版本来源不一致时的提示（启动时记录一次）。
func coreVersionWarning() string {
	mainPath, dep := readBuildInfo()
	_, warn := resolveCoreVersion(mainPath, dep, version.Version)
	return warn
}

// readBuildInfo 返回主模块路径与所依赖公共核心模块的版本（未依赖时为空）。
func readBuildInfo() (mainPath, depVersion string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	for _, dep := range bi.Deps {
		if dep.Path != coreModulePath {
			continue
		}
		depVersion = dep.Version
		if dep.Replace != nil {
			depVersion = dep.Replace.Version // 目录 replace 时为空或 "(devel)"：没有可信版本
		}
	}
	return bi.Main.Path, depVersion
}

// resolveCoreVersion 决定核心版本：
//   - 私有仓库组装（主模块不是公共核心）时以 go.mod 实际依赖的公共核心版本为准：它由 go 工具链
//     记录，私有仓库不需要（也不应依赖）注入任何 internal 符号。若同时存在 ldflags 注入的正式版本
//     且与之不一致，返回警告；只有依赖本身没有正式版本（本地目录 replace、伪版本）时，才退而使用
//     注入的正式版本；
//   - 公共仓库自身构建时用发布流程注入的版本，否则为 "dev"。
//
// 返回值不是正式版本时，模块的核心版本要求只能告警、无法验证。
func resolveCoreVersion(mainPath, depVersion, injected string) (core, warning string) {
	if mainPath != "" && mainPath != coreModulePath && depVersion != "" {
		if nd, ok := update.NormalizeRelease(depVersion); ok {
			if ni, ok := update.NormalizeRelease(injected); ok && ni != nd {
				warning = fmt.Sprintf("injected relay version %s differs from the required core module %s %s; module compatibility uses %s", injected, coreModulePath, depVersion, depVersion)
			}
			return depVersion, warning
		}
	}
	if update.IsRelease(injected) {
		return injected, ""
	}
	if depVersion != "" && depVersion != "(devel)" {
		return depVersion, ""
	}
	return injected, ""
}

// checkBuiltinModules 汇总内置模块的注册失败与已知的核心版本不兼容；有任一错误即返回，
// 调用方拒绝启动 relay。只能告警的情况（核心不是正式版本）写日志。
func checkBuiltinModules(reg *module.Registry, regErrs []error, core string) error {
	if len(reg.IDs()) > 0 {
		if warn := coreVersionWarning(); warn != "" {
			log.Printf("WARNING: %s", warn)
		}
	}
	notes, errs := reg.CheckCore(core)
	for _, n := range notes {
		log.Printf("WARNING: %s", n)
	}
	all := append(append([]error(nil), regErrs...), errs...)
	if len(all) == 0 {
		return nil
	}
	msgs := make([]string, len(all))
	for i, e := range all {
		msgs[i] = e.Error()
	}
	return fmt.Errorf("built-in modules are incompatible with this relay core %s (module API v%d): %s", core, module.APIVersion, strings.Join(msgs, "; "))
}

// listBuiltinModules 实现 --list-modules：列出编译进二进制的内置模块及兼容性检查结果。
// 存在不兼容时返回错误（进程退出码非 0），便于构建流水线校验组装结果。
func listBuiltinModules(w io.Writer, reg *module.Registry, regErrs []error, core string) error {
	fmt.Fprintf(w, "relay core %s, module API v%d (supported %v)\n", core, module.APIVersion, module.SupportedAPIVersions)
	ids := reg.IDs()
	if len(ids) == 0 {
		fmt.Fprintln(w, "built-in modules: none")
	}
	for _, id := range ids {
		d, _ := reg.Descriptor(id)
		status := "ok"
		if note, err := module.CheckCoreCompatibility(d, core); err != nil {
			status = "INCOMPATIBLE: " + err.Error()
		} else if note != "" {
			status = "unverified: core is not a release version"
		}
		fmt.Fprintf(w, "module %s [%s]\n", d.Summary(), status)
	}
	for _, err := range regErrs {
		fmt.Fprintf(w, "registration FAILED: %v\n", err)
	}
	if err := checkBuiltinModules(reg, regErrs, core); err != nil {
		return errors.New("built-in module check failed")
	}
	return nil
}
