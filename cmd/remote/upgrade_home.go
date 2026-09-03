package main

import (
	"fmt"
	"os"
	"strings"
)

// 升级隔离期把新 share 的 HOME/USERPROFILE 指向暂存目录（upgrade_linux.go / upgrade_windows.go
// 里构造 cmd.Env 那两处），为的是让它在 make-before-break 期间派生出与 old 不同的 ClientID。
//
// 但隔离只在与 old 并存的那段时间需要，改写之后却一直没有还原点：新 share 就是最终长期运行的
// 那个进程。于是升级完成后，远端所有 exec 子进程（internal/agent/tools/exec.go 在调用方没传
// env 时直接继承 share 的 os.Environ()）的 ~ 都落在一个空的临时目录里——git 读不到 .gitconfig、
// ssh 找不到 known_hosts 与私钥、npm/pip 全部失配，且一直持续到下一次重启 share。
//
// 这里用两个内部环境变量把原值带给继任进程，等它抢到标准实例锁（old 已退出、隔离不再必要）
// 时还原。选环境变量而不是命令行旗标：upgradedShareArgs 的语义是「原样透传 old 的显式参数」，
// 塞进去会污染链式升级；而且真实家目录路径也不该出现在远端的 ps 输出里。
const (
	upgradeOrigHomeEnv        = "REMOTE_UPGRADE_ORIG_HOME"
	upgradeOrigUserProfileEnv = "REMOTE_UPGRADE_ORIG_USERPROFILE"
)

// 每项记录：待还原的真实变量名 + 存放原值的内部变量名。
var upgradeHomeVars = []struct{ target, stash string }{
	{"HOME", upgradeOrigHomeEnv},
	{"USERPROFILE", upgradeOrigUserProfileEnv},
}

// 内部变量的值带一位存在性前缀：'1'+原值 表示原本有这个变量，'0' 表示原本没有（还原时要
// Unsetenv 而不是设成空串）。不用「空值即不存在」是因为 Windows 环境块里 KEY= 的语义本身
// 就含混，带前缀后任何情况下值都非空，穿过 CreateProcess 不会被吃掉。
const (
	upgradeEnvPresent = '1'
	upgradeEnvAbsent  = '0'
)

// stashOrigHomeEnv 在 upgrade-stage 构造子进程环境时调用：把 keys 当前的值编码进对应的内部
// 变量，供继任进程还原。必须在 replaceEnv 改写之前调用，否则存下的就是隔离目录本身。
//
// 链式升级（对已升级过的 share 再升级）时，若上一次的还原已经发生，这里读到的就是真实家
// 目录，正确；若还没发生（old 迟迟不退出），env 里仍带着上一层的内部变量，此时保留旧值不
// 覆盖，最原始的那个家目录会一路透传下去。
func stashOrigHomeEnv(env []string, keys ...string) []string {
	for _, key := range keys {
		stash := ""
		for _, v := range upgradeHomeVars {
			if strings.EqualFold(v.target, key) {
				stash = v.stash
			}
		}
		if stash == "" {
			continue
		}
		if prev, already := lookupEnvIn(env, stash); already && prev != "" {
			continue // 上一层升级存的原值更「原」，别覆盖
		}
		encoded := string(upgradeEnvAbsent)
		if val, ok := lookupEnvIn(env, key); ok {
			encoded = string(upgradeEnvPresent) + val
		}
		env = append(env, stash+"="+encoded)
	}
	return env
}

// lookupEnvIn 在 KEY=VALUE 切片里查变量，语义同 os.LookupEnv。
// Windows 环境变量名大小写不敏感，统一按不敏感匹配（Unix 上这几个名字本就都是大写，无影响）。
func lookupEnvIn(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- { // 后写的覆盖先写的，与 replaceEnv 的 append 语义一致
		item := env[i]
		if len(item) >= len(prefix) && strings.EqualFold(item[:len(prefix)], prefix) {
			return item[len(prefix):], true
		}
	}
	return "", false
}

// restoreUpgradeHomeEnv 把被升级隔离改写过的 HOME/USERPROFILE 还原成原值，并清掉内部变量。
// 由继任 share 在抢到标准实例锁后调用——此刻 old 已退出，隔离不再有意义。
// 返回被还原的变量名，供日志说明；非升级继任者（没有内部变量）时返回空。
func restoreUpgradeHomeEnv() []string {
	var restored []string
	for _, v := range upgradeHomeVars {
		encoded, ok := os.LookupEnv(v.stash)
		if !ok || encoded == "" {
			continue
		}
		_ = os.Unsetenv(v.stash)
		switch encoded[0] {
		case upgradeEnvPresent:
			_ = os.Setenv(v.target, encoded[1:])
		case upgradeEnvAbsent:
			_ = os.Unsetenv(v.target) // 隔离前本就没有这个变量：还原成「没有」，而不是空串
		default:
			continue // 值被外部改坏，宁可不动 HOME
		}
		restored = append(restored, v.target)
	}
	return restored
}

// announceRestoredUpgradeHome 执行还原，并在确实还原了东西时在日志里留一行——升级是远端
// 无人值守跑的，没有这行就没法事后确认隔离到底解没解除。
func announceRestoredUpgradeHome() {
	restored := restoreUpgradeHomeEnv()
	if len(restored) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "升级隔离结束：已还原 %s（后续 exec 子进程恢复使用真实用户目录）\n", strings.Join(restored, "/"))
}
