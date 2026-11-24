package main

import (
	"flag"
	"os"
	"runtime"
	"strings"

	"github.com/remote-assist/tool/internal/client"
)

// relayServerEnv 设了它就是显式模式：自建 relay 想长期固定使用就设它（spec 6.1、6.5）。
const relayServerEnv = "REMOTE_RELAY_SERVER"

// explicitRelay 报告用户有没有指定 relay：命令行写了 --server（写的正是内置地址也算），或者设了 REMOTE_RELAY_SERVER。
func explicitRelay(fs *flag.FlagSet) bool {
	if strings.TrimSpace(os.Getenv(relayServerEnv)) != "" {
		return true
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "server" {
			explicit = true
		}
	})
	return explicit
}

// shareUsesNodeList 判断 share 是不是自动模式（spec 6.1）：没指定 relay、也不是 --standalone 时按节点列表选节点。
// --plain 只用于开发：节点都要 TLS 并按列表里的证书指纹校验，明文连不了，所以也按显式处理，照旧只连 --server。
func shareUsesNodeList(fs *flag.FlagSet, standalone, plain bool) bool {
	return !standalone && !plain && !explicitRelay(fs)
}

// upgradeRealHome 返回升级隔离前的家目录（os.UserHomeDir 在本平台看的那个变量的原值）。升级继任的 share 和旧进程并存时
// HOME/USERPROFILE 指向暂存目录，节点列表缓存要用真正的家目录（见 client.ShareMode.UseNodeList）。
// 不是升级继任者、隔离前本就没有这个变量、或者值被改坏时返回空串，调用方照常用 os.UserHomeDir。
func upgradeRealHome() string {
	stash := upgradeOrigHomeEnv
	if runtime.GOOS == "windows" {
		stash = upgradeOrigUserProfileEnv
	}
	encoded := os.Getenv(stash)
	if len(encoded) < 2 || encoded[0] != upgradeEnvPresent {
		return ""
	}
	return encoded[1:]
}

// shareRelay 是 share 连 relay 的方式（spec 6.1）。要在 fs.Parse 之后、还原升级隔离的 HOME 之前取：
// 还原时会清掉记着原家目录的内部变量。
type shareRelay struct {
	auto     bool   // 自动模式：按节点列表选节点
	hint     string // --relay-hint（升级前所在的节点），已补默认端口
	listHome string // 节点列表缓存所在的家目录；空串时用 os.UserHomeDir
}

// 显式模式返回零值：--relay-hint 和节点列表缓存都用不上。
func newShareRelay(fs *flag.FlagSet, standalone, plain, upgradeSuccessor bool, hint string) shareRelay {
	if !shareUsesNodeList(fs, standalone, plain) {
		return shareRelay{}
	}
	r := shareRelay{auto: true, hint: client.NormalizeServerAddr(strings.TrimSpace(hint))}
	if upgradeSuccessor {
		r.listHome = upgradeRealHome()
	}
	return r
}

// stun 返回 share 用的 STUN 服务器：用户给了 --stun 就用它，否则是 relay 主机的 3478。STUN 也是 UDP 中继的地址，
// 自动模式下不能按 --server（这时是内置默认节点）推导，留空让 share 跟着实际连上的节点走（见 client.ShareMode.UseNodeList）。
func (r shareRelay) stun(stun, server string) string {
	if stun != "" || r.auto {
		return stun
	}
	return client.RelaySTUNAddr(server)
}

// nodeListUser 是 *client.ShareMode 里 apply 用到的那部分，测试拿假的替换它。
type nodeListUser interface {
	UseNodeList(hint, home string)
}

// apply 把自动模式接到 share 上。share 的 --server 此时是内置默认节点：没有能用的节点列表时连它。
func (r shareRelay) apply(share nodeListUser) {
	if r.auto {
		share.UseNodeList(r.hint, r.listHome)
	}
}
