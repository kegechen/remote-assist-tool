// Package module 是 relay 的进程内置模块框架（第一阶段）。
//
// 模块只能通过受限的 ModuleContext 访问核心能力，不能持有 *relay.Server 等核心对象；
// 连接、会话、认证与转发仍完全由 relay 核心负责。第一阶段只支持编译进二进制的内置模块
// （见 Registry），不加载任何外部路径的动态代码，也不做运行时代码替换。
//
// 设计边界由本包的公开契约和 relayapp 的生命周期接线共同定义。
package module

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 生命周期默认值。模块可在 Descriptor 中覆盖，但不得超过上限，避免一个模块把热加载卡死。
const (
	DefaultStartTimeout = 10 * time.Second
	DefaultStopTimeout  = 10 * time.Second
	MaxLifecycleTimeout = 2 * time.Minute
	// DefaultMaxTasks 单个模块同时运行的受控后台任务上限。
	DefaultMaxTasks = 8
	MaxTasksLimit   = 256
)

// CurrentConfigVersion 是本版本 relay 支持的统一配置格式版本（配置根节点 version 字段）。
const CurrentConfigVersion = 1

var moduleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Dependency 描述对另一个模块的依赖。
type Dependency struct {
	ID string
	// MinVersion 依赖模块的最低版本（点分数字，如 "1.2"）；空 = 不限。
	MinVersion string
	// Optional 可选依赖：缺失或未启用时本模块以 degraded 状态运行，而不是拒绝配置。
	Optional bool
}

// Descriptor 是模块注册时声明的元数据，注册后不可变。
type Descriptor struct {
	// ID 模块唯一标识，即配置文件 modules 下的键名，须匹配 ^[a-z][a-z0-9_]{0,31}$。
	ID string
	// Version 模块代码版本（点分数字，如 "1.0.0"），用于依赖版本校验与可观测性。
	Version string
	// Source 模块来源：实现所在的 Go 模块路径，公共仓库内置模块为 SourceCore，私有仓库填自己的
	// 模块路径（如 "example.com/team/relay-modules"）。必填，用于排查与配置中的来源校验。
	Source string
	// APIVersion 模块按哪个模块契约版本编写（见 APIVersion）。必填；核心不支持时注册即失败。
	APIVersion int
	// ConfigVersions 支持的统一配置格式版本；空 = 只支持 CurrentConfigVersion。
	ConfigVersions []int
	Requires       []Dependency
	// Conflicts 与本模块互斥、不能同时启用的模块 ID。
	Conflicts    []string
	StartTimeout time.Duration
	StopTimeout  time.Duration
	// MaxTasks 受控后台任务并发上限；0 = DefaultMaxTasks。
	MaxTasks int
	// MinCoreVersion 要求的最低 relay 核心正式版本（如 "v1.4.0"）；空 = 不限。已知核心更旧时
	// 整个 relay 拒绝启动（--list-modules 退出码非 0），配置校验也会拒绝启用它；核心不是正式版本
	// （dev 构建、目录 replace、伪版本）时无法比较，只记录警告。
	MinCoreVersion string
}

// Config 是交给单个模块的只读配置：modules.<id> 去掉框架字段（enabled）后的 JSON 对象。
type Config struct {
	ModuleID string
	// SchemaVersion 统一配置文件根节点的 version。
	SchemaVersion int
	raw           json.RawMessage
}

// NewConfig 构造模块配置；raw 会被复制，调用方之后修改原切片不影响 Config。
// raw 为空时视为空对象 {}。
func NewConfig(moduleID string, schemaVersion int, raw []byte) Config {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	return Config{ModuleID: moduleID, SchemaVersion: schemaVersion, raw: append(json.RawMessage(nil), raw...)}
}

// Bytes 返回配置 JSON 的副本。
func (c Config) Bytes() []byte {
	if len(c.raw) == 0 {
		return []byte("{}")
	}
	return append([]byte(nil), c.raw...)
}

// Equal 按字节比较两份配置。前提是 raw 已规范化（config 包产出的是重新编码后的 JSON，
// 键序与空白稳定）；手工构造的 Config 若空白不同会被判为不相等。
func (c Config) Equal(other Config) bool {
	return c.ModuleID == other.ModuleID && c.SchemaVersion == other.SchemaVersion && bytes.Equal(c.Bytes(), other.Bytes())
}

// Decode 严格解码到 v：拒绝未知字段与尾随内容，避免拼写错误被静默忽略。
func (c Config) Decode(v any) error {
	dec := json.NewDecoder(bytes.NewReader(c.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("module %s config: %w", c.ModuleID, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("module %s config: unexpected trailing data", c.ModuleID)
	}
	return nil
}

// Module 是内置模块需要实现的生命周期接口（第一阶段）。
//
// 对应设计中的 Register/Validate/Stop/Health：Register 由 Registry.Register 完成；
// 第一阶段配置变更按「停旧实例 → 用新配置 Start」处理，Prepare/Commit/Drain 见 Reloadable。
// 所有方法都可能在任意 goroutine 被调用，但框架保证同一模块的生命周期调用串行。
type Module interface {
	// Descriptor 返回模块元数据；每次调用必须返回相同内容。
	Descriptor() Descriptor
	// Validate 校验模块配置与运行前置条件，不得产生副作用（不建连接、不起 goroutine）。
	Validate(cfg Config) error
	// Start 用给定配置启动模块。须在 ctx 截止前返回；长期运行的工作用 mc.Go 托管。
	Start(ctx context.Context, mc ModuleContext) error
	// Stop 停止模块并释放资源，须在 ctx 截止前返回。调用 Stop 前框架已取消 mc.Go 托管
	// 任务的 ctx（mc.Done 已关闭），Stop 返回后框架在同一超时内等待这些任务退出。
	Stop(ctx context.Context) error
	// Health 返回当前健康状态，须快速返回、不得阻塞。
	Health() Health
}

// Reloadable 为第二阶段 Prepare/Commit/Drain/Stop 两阶段热切换预留的可选接口。
// 第一阶段框架不调用它；实现它的模块仍按 Module 的停启语义工作。
type Reloadable interface {
	// Prepare 用新配置创建候选实例并完成资源检查，不得影响当前实例。
	Prepare(ctx context.Context, cfg Config, mc ModuleContext) (Candidate, error)
}

// Candidate 是 Prepare 产出的候选实例。
type Candidate interface {
	// Commit 原子地把候选实例切换为当前实例，须快速返回、不得阻塞 I/O。
	Commit() error
	// Abort 放弃未提交的候选实例并释放其资源。
	Abort(ctx context.Context) error
}

// Drainer 为第二阶段预留：旧 generation 停止接收新任务并等待在途任务完成。
type Drainer interface {
	Drain(ctx context.Context) error
}

// HealthStatus 模块自报的健康状态。
type HealthStatus string

const (
	HealthUnknown   HealthStatus = "unknown"
	HealthOK        HealthStatus = "ok"
	HealthDegraded  HealthStatus = "degraded"
	HealthUnhealthy HealthStatus = "unhealthy"
)

// Health 是模块健康快照。零值 Status ""（模块未填写）按 HealthUnknown 处理。
type Health struct {
	Status  HealthStatus
	Message string
}

// ValidateDescriptor 校验模块元数据本身是否合法（与配置无关）。
func ValidateDescriptor(d Descriptor) error {
	if !moduleIDPattern.MatchString(d.ID) {
		return fmt.Errorf("module id %q must match %s", d.ID, moduleIDPattern.String())
	}
	if _, err := parseVersion(d.Version); err != nil {
		return fmt.Errorf("module %s: version %q: %w", d.ID, d.Version, err)
	}
	if !sourcePattern.MatchString(d.Source) {
		return fmt.Errorf("module %s: source %q must be the implementing Go module path (e.g. %s)", d.ID, d.Source, SourceCore)
	}
	if !apiVersionSupported(d.APIVersion) {
		return fmt.Errorf("module %s (%s %s) is built for module API v%d, this relay core supports %v; rebuild the module against a compatible core", d.ID, d.Source, d.Version, d.APIVersion, SupportedAPIVersions)
	}
	if d.MinCoreVersion != "" {
		if _, err := parseVersion(d.MinCoreVersion); err != nil {
			return fmt.Errorf("module %s: min core version %q: %w", d.ID, d.MinCoreVersion, err)
		}
	}
	seenVersion := map[int]bool{}
	for _, v := range d.ConfigVersions {
		if v < 1 {
			return fmt.Errorf("module %s: config version %d must be positive", d.ID, v)
		}
		if seenVersion[v] {
			return fmt.Errorf("module %s: duplicate config version %d", d.ID, v)
		}
		seenVersion[v] = true
	}
	seen := map[string]bool{}
	for _, dep := range d.Requires {
		if !moduleIDPattern.MatchString(dep.ID) {
			return fmt.Errorf("module %s: dependency id %q is invalid", d.ID, dep.ID)
		}
		if dep.ID == d.ID {
			return fmt.Errorf("module %s: depends on itself", d.ID)
		}
		if seen[dep.ID] {
			return fmt.Errorf("module %s: duplicate dependency %s", d.ID, dep.ID)
		}
		seen[dep.ID] = true
		if dep.MinVersion != "" {
			if _, err := parseVersion(dep.MinVersion); err != nil {
				return fmt.Errorf("module %s: dependency %s min version %q: %w", d.ID, dep.ID, dep.MinVersion, err)
			}
		}
	}
	seenConflict := map[string]bool{}
	for _, c := range d.Conflicts {
		if !moduleIDPattern.MatchString(c) {
			return fmt.Errorf("module %s: conflict id %q is invalid", d.ID, c)
		}
		if c == d.ID {
			return fmt.Errorf("module %s: conflicts with itself", d.ID)
		}
		if seen[c] {
			return fmt.Errorf("module %s: %s is both a dependency and a conflict", d.ID, c)
		}
		if seenConflict[c] {
			return fmt.Errorf("module %s: duplicate conflict %s", d.ID, c)
		}
		seenConflict[c] = true
	}
	for name, t := range map[string]time.Duration{"start timeout": d.StartTimeout, "stop timeout": d.StopTimeout} {
		if t < 0 || t > MaxLifecycleTimeout {
			return fmt.Errorf("module %s: %s %s must be between 0 and %s", d.ID, name, t, MaxLifecycleTimeout)
		}
	}
	if d.MaxTasks < 0 || d.MaxTasks > MaxTasksLimit {
		return fmt.Errorf("module %s: max tasks %d must be between 0 and %d", d.ID, d.MaxTasks, MaxTasksLimit)
	}
	return nil
}

// SupportsConfigVersion 报告模块是否支持给定的统一配置格式版本。
func (d Descriptor) SupportsConfigVersion(v int) bool {
	if len(d.ConfigVersions) == 0 {
		return v == CurrentConfigVersion
	}
	for _, s := range d.ConfigVersions {
		if s == v {
			return true
		}
	}
	return false
}

func (d Descriptor) startTimeout() time.Duration {
	if d.StartTimeout > 0 {
		return d.StartTimeout
	}
	return DefaultStartTimeout
}

func (d Descriptor) stopTimeout() time.Duration {
	if d.StopTimeout > 0 {
		return d.StopTimeout
	}
	return DefaultStopTimeout
}

func (d Descriptor) maxTasks() int {
	if d.MaxTasks > 0 {
		return d.MaxTasks
	}
	return DefaultMaxTasks
}

// parseVersion 解析点分数字版本（允许前缀 v），如 "1"、"1.2"、"v1.2.3"。
func parseVersion(s string) ([]int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, errors.New("empty version")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return nil, errors.New("too many components")
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" || (len(p) > 1 && p[0] == '0') {
			return nil, fmt.Errorf("component %q is not a non-negative integer", p)
		}
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out, nil
}

// versionAtLeast 报告 have >= want；两者都须是合法版本。
func versionAtLeast(have, want string) (bool, error) {
	h, err := parseVersion(have)
	if err != nil {
		return false, err
	}
	w, err := parseVersion(want)
	if err != nil {
		return false, err
	}
	for i := 0; i < len(h) || i < len(w); i++ {
		var a, b int
		if i < len(h) {
			a = h[i]
		}
		if i < len(w) {
			b = w[i]
		}
		if a != b {
			return a > b, nil
		}
	}
	return true, nil
}
