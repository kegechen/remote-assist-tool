package module

import (
	"fmt"
	"regexp"
	"strings"
)

// APIVersion 是本核心实现的模块契约版本（本包导出的接口与语义）。模块在 Descriptor.APIVersion
// 中声明自己按哪个版本编写；契约发生不兼容变化时递增，并在 SupportedAPIVersions 中决定是否继续接受旧版本。
const APIVersion = 1

// SupportedAPIVersions 本核心能加载的模块契约版本。
var SupportedAPIVersions = []int{APIVersion}

// SourceCore 是公共仓库内置模块的来源标识。
const SourceCore = "github.com/remote-assist/tool"

// sourcePattern 来源标识：Go 模块路径形式（域名 + 路径），例如 example.com/team/relay-modules。
var sourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]*(\.[a-z0-9\-]+)+(/[A-Za-z0-9._~\-]+)*$`)

func apiVersionSupported(v int) bool {
	for _, s := range SupportedAPIVersions {
		if s == v {
			return true
		}
	}
	return false
}

// Requirement 是配置文件对某个模块构建的期望（modules.<id> 中的可选 source / min_version）。
// 用于发现「配置为私有模块 A 编写，但二进制里是另一个来源或更旧版本的同名模块」。
type Requirement struct {
	Source     string
	MinVersion string
}

// IsReleaseVersion 报告 v 是否是可比较的正式版本号（点分数字，可带前缀 v），dev 构建、
// 预发布与伪版本都不是。
func IsReleaseVersion(v string) bool {
	_, err := parseVersion(v)
	return err == nil
}

// CheckCoreCompatibility 校验模块对核心版本的要求。coreVersion 不是正式版本（dev 构建、伪版本）
// 时无法比较，返回 ok=true 与说明，由调用方记录警告；已知不满足时返回 error。
func CheckCoreCompatibility(d Descriptor, coreVersion string) (note string, err error) {
	if d.MinCoreVersion == "" {
		return "", nil
	}
	if !IsReleaseVersion(coreVersion) {
		return fmt.Sprintf("module %s requires relay core >= %s, but core version %q is not a release; requirement not verified", d.ID, d.MinCoreVersion, coreVersion), nil
	}
	ok, err := versionAtLeast(coreVersion, d.MinCoreVersion)
	if err != nil {
		return "", fmt.Errorf("module %s: min core version %q: %w", d.ID, d.MinCoreVersion, err)
	}
	if !ok {
		return "", fmt.Errorf("module %s (%s %s) requires relay core >= %s, this relay core is %s; rebuild against a newer core", d.ID, d.Source, d.Version, d.MinCoreVersion, coreVersion)
	}
	return "", nil
}

// CheckRequirement 校验配置对模块来源与版本的期望。
func CheckRequirement(d Descriptor, r Requirement) error {
	if r.Source != "" && r.Source != d.Source {
		return fmt.Errorf("module %s: config expects source %s, this relay was built with %s", d.ID, r.Source, d.Source)
	}
	if r.MinVersion != "" {
		ok, err := versionAtLeast(d.Version, r.MinVersion)
		if err != nil {
			return fmt.Errorf("module %s: min_version %q: %w", d.ID, r.MinVersion, err)
		}
		if !ok {
			return fmt.Errorf("module %s: config requires version >= %s, this relay was built with %s %s", d.ID, r.MinVersion, d.Source, d.Version)
		}
	}
	return nil
}

// ValidateRequirement 校验 Requirement 本身的格式（不涉及具体模块）。
func ValidateRequirement(r Requirement) error {
	if r.Source != "" && !sourcePattern.MatchString(r.Source) {
		return fmt.Errorf("source %q must be a module path such as example.com/team/relay-modules", r.Source)
	}
	if r.MinVersion != "" {
		if _, err := parseVersion(r.MinVersion); err != nil {
			return fmt.Errorf("min_version %q: %w", r.MinVersion, err)
		}
	}
	return nil
}

// Summary 返回便于日志/命令行展示的一行元数据。
func (d Descriptor) Summary() string {
	parts := []string{d.ID, d.Version, "source=" + d.Source, fmt.Sprintf("api=v%d", d.APIVersion)}
	if d.MinCoreVersion != "" {
		parts = append(parts, "min_core="+d.MinCoreVersion)
	}
	return strings.Join(parts, " ")
}
