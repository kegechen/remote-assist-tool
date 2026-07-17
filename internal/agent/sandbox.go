package agent

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SandboxConfig share 启动时来自 CLI 的策略
type SandboxConfig struct {
	Root       string   // 文件操作限制在此子树内；空 = 不限制（见 ResolvePath 注释）
	AllowExec  []string // 若非空，argv[0] 必须在此列表（basename 比较）
	DenyExec   []string // argv[0] 命中即拒绝（basename 比较）
	UnsafeExec bool     // 关闭 exec 黑/白名单（启动时强制红色横幅 + 倒计时确认）；不影响 Root
}

// Sandbox 封装路径/exec 决策
type Sandbox struct {
	cfg        SandboxConfig
	root       string // EvalSymlinks 后的绝对 root
	restricted bool   // 用户显式传了 Root；与 root=="" 区分开以便解析失败时 fail closed
}

// NewSandbox 注意：root 若指定，构造时已 EvalSymlinks + Abs；运行时短路 stale root 重算无意义
func NewSandbox(cfg SandboxConfig) *Sandbox {
	sb := &Sandbox{cfg: cfg}
	if cfg.Root != "" {
		sb.restricted = true
		if abs, err := filepath.Abs(cfg.Root); err == nil {
			if eval, err := filepath.EvalSymlinks(abs); err == nil {
				sb.root = eval
			} else {
				sb.root = abs
			}
		}
	}
	return sb
}

// ResolvePath 校验并返回规范化路径；不存在的目标走父目录解析（write 路径）。
//
// 未配置 Root 时不做任何限制：share 是本机用户主动发起、凭协助码授权的，信任边界
// 是“协助码交给谁”，不是这里。Root 只是可选的防手滑护栏，不是安全边界——exec 不走
// 本函数（见 CheckExec），一句 `sh -c 'cp /etc/passwd <root>/'` 即可绕过。需要真隔离
// 请在进程外面套（容器 / 专用低权限账号）。
func (s *Sandbox) ResolvePath(p string) (string, error) {
	if !s.restricted {
		abs, err := filepath.Abs(p)
		return abs, err
	}
	if s.root == "" {
		return "", fmt.Errorf("path_outside_root: --root %q could not be resolved", s.cfg.Root)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(abs)
	// 对已存在路径走 EvalSymlinks；不存在的（write_file 创建）对父目录 EvalSymlinks 再拼文件名
	resolved := cleaned
	if eval, err := filepath.EvalSymlinks(cleaned); err == nil {
		resolved = eval
	} else {
		// 路径不存在时，对父目录解析符号链接（处理 Windows 短路径等问题）
		parent := filepath.Dir(cleaned)
		if evalParent, err := filepath.EvalSymlinks(parent); err == nil {
			resolved = filepath.Join(evalParent, filepath.Base(cleaned))
		}
	}
	rel, err := filepath.Rel(s.root, resolved)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("path_outside_root: %s", p)
	}
	return resolved, nil
}

// CheckExec argv[0] 的 basename 比较。注意这是防手滑的护栏而非安全边界：basename 过滤
// 拦不住 `sh -c 'rm ...'` / `python -c ...` 这类等价写法。
func (s *Sandbox) CheckExec(argv []string) error {
	if s.cfg.UnsafeExec {
		return nil
	}
	if len(argv) == 0 {
		return fmt.Errorf("exec_denied: empty argv")
	}
	name := filepath.Base(argv[0])
	for _, d := range s.cfg.DenyExec {
		if d == name {
			return fmt.Errorf("exec_denied: %s in deny list", name)
		}
	}
	if len(s.cfg.AllowExec) > 0 {
		for _, a := range s.cfg.AllowExec {
			if a == name {
				return nil
			}
		}
		return fmt.Errorf("exec_denied: %s not in allow list", name)
	}
	return nil
}
