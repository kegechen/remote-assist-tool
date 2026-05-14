package agent

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SandboxConfig share 启动时来自 CLI 的策略
type SandboxConfig struct {
	Root      string   // 文件操作必须在此子树内；空 = 拒绝所有文件操作
	AllowExec []string // 若非空，argv[0] 必须在此列表（basename 比较）
	DenyExec  []string // argv[0] 命中即拒绝（basename 比较）
	Unsafe    bool     // 关闭全部沙箱（启动时强制红色横幅 + 倒计时确认）
}

// Sandbox 封装路径/exec 决策
type Sandbox struct {
	cfg  SandboxConfig
	root string // EvalSymlinks 后的绝对 root
}

// NewSandbox 注意：root 若指定，构造时已 EvalSymlinks + Abs；运行时短路 stale root 重算无意义
func NewSandbox(cfg SandboxConfig) *Sandbox {
	sb := &Sandbox{cfg: cfg}
	if cfg.Root != "" {
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

// ResolvePath 校验并返回规范化路径；不存在的目标走父目录解析（write 路径）
func (s *Sandbox) ResolvePath(p string) (string, error) {
	if s.cfg.Unsafe {
		abs, err := filepath.Abs(p)
		return abs, err
	}
	if s.root == "" {
		return "", fmt.Errorf("path_outside_root: no --root configured")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(abs)
	// 对已存在路径走 EvalSymlinks；不存在的（write_file 创建）退化到 lexical
	resolved := cleaned
	if eval, err := filepath.EvalSymlinks(cleaned); err == nil {
		resolved = eval
	}
	rel, err := filepath.Rel(s.root, resolved)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("path_outside_root: %s", p)
	}
	return resolved, nil
}

// CheckExec argv[0] 的 basename 比较
func (s *Sandbox) CheckExec(argv []string) error {
	if s.cfg.Unsafe {
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
