package main

import (
	"fmt"

	"github.com/remote-assist/tool/internal/upgradeflags"
)

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
		if a == "--unsafe-full-system" || a == "--unsafe-full-system=true" {
			// 0.0.5 旧名：当时同时放开文件与 exec；新版空 root 已默认不限制文件，
			// 因此把显式危险授权等价迁移为只剩的 exec 开关。
			out = append(out, "--unsafe-exec")
			continue
		}
		if a == "--unsafe-full-system=false" {
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
	return out, nil
}
