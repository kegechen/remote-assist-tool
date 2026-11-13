package main

import (
	"fmt"
	"strings"

	"github.com/remote-assist/tool/internal/upgradeflags"
)

const upgradeSuccessorFlag = "--upgrade-successor"

// upgradedShareArgs 复用 old share 的全部显式参数，只替换 server/code-file。这样 root、
// allow/deny-exec、TLS、P2P 等策略不会在升级过程中被悄悄改变。
//
// 兼容 --code-file：old 若靠 --code-file 让宿主管家读协助码，新 share 除了写升级目录里的
// code.json（help 端握手用）外，还要继续写原路径。原路径以 --code-file-mirror 透传给新
// share。链式升级（对已升级过的 share 再升级）时，真正的宿主路径在上一次的
// --code-file-mirror 里，故用 HostCodeFile 优先取 mirror。
func upgradedShareArgs(oldArgv []string, server, codeFile string) ([]string, error) {
	if len(oldArgv) == 0 {
		return nil, fmt.Errorf("old process command line is empty")
	}
	args := append([]string(nil), oldArgv[1:]...)
	if len(args) == 0 {
		args = []string{"share"} // 无参数双击启动的 old
	}
	if args[0] != "share" {
		return nil, fmt.Errorf("old process is not in share mode")
	}
	if err := upgradeflags.RejectStandaloneNoAuth(args); err != nil {
		return nil, err
	}
	hostCodeFile := upgradeflags.HostCodeFile(args)

	out := []string{"share"}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == upgradeSuccessorFlag {
			continue
		}
		if a == "--unsafe-full-system" || a == "--unsafe-full-system=true" {
			// 0.0.5 旧名：当时同时放开文件与 exec；新版空 root 已默认不限制文件，
			// 因此把显式危险授权等价迁移为只剩的 exec 开关。
			out = append(out, "--unsafe-exec")
			continue
		}
		if a == "--unsafe-full-system=false" {
			continue
		}
		if isEnableDesktopFlag(a) {
			// 升级后的 share 由升级器以隐藏、脱离控制台的方式启动，输出只进日志文件：远程桌面的
			// 「协助端正在查看/控制你的屏幕」提示被协助方看不到，提示里的「关闭本窗口」也无从执行。
			// 远程桌面必须让被协助方知情，所以升级后一律关闭；需要时由被协助方在可见窗口里
			// 带 --enable-desktop 重新启动 share。
			continue
		}
		// 由 upgrade 接管的托管旗标：剥掉旧值，末尾统一重写。--standalone=false /
		// --no-auth=0 等显式 false 不属于托管旗标，落到最后原样保留。
		switch a {
		case "--server", "--code-file", "--code-file-mirror":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s is missing its value", a)
			}
			i++
			continue
		}
		if upgradeflags.HasValuePrefix(a, "--server") ||
			upgradeflags.HasValuePrefix(a, "--code-file") ||
			upgradeflags.HasValuePrefix(a, "--code-file-mirror") {
			continue
		}
		out = append(out, a)
	}
	if server == "" {
		return nil, fmt.Errorf("effective relay server is empty")
	}
	out = append(out, "--server", server, "--code-file", codeFile)
	if hostCodeFile != "" {
		out = append(out, "--code-file-mirror", hostCodeFile)
	}
	// new share 在 make-before-break 阶段先上线，并排队接管 old 持有的标准实例锁。
	out = append(out, upgradeSuccessorFlag)
	return out, nil
}

// isEnableDesktopFlag 识别 --enable-desktop 的各种写法（-/-- 前缀、=true/=false）。
func isEnableDesktopFlag(a string) bool {
	if !strings.HasPrefix(a, "-") {
		return false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name == "enable-desktop"
}
