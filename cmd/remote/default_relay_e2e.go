//go:build remoteassist_e2e

package main

import (
	"os"
	"strings"
)

// 只编进 e2e 测试构建（tests/e2e 用 -tags remoteassist_e2e 自己编的 remote）：内置默认 relay 换成 RAT_E2E_DEFAULT_RELAY，
// 没设时是连不上的本机地址（discard 端口）。e2e 里没有能用的节点列表时连的就是内置默认 relay，绝不能连到线上。
// 正式构建（scripts/build.*）不带这个 tag，TestReleaseBuildDefaultRelayIsPublic 守着。

const (
	e2eDefaultRelayEnv = "RAT_E2E_DEFAULT_RELAY"
	e2eDeadRelay       = "127.0.0.1:9"
)

func init() {
	defaultRelayServerAddr = e2eDefaultRelay(lookupE2EDefaultRelay())
}

func lookupE2EDefaultRelay() string { return os.Getenv(e2eDefaultRelayEnv) }

// e2eDefaultRelay 返回 e2e 构建的内置默认 relay：v（去掉首尾空白）非空时用它，否则是连不上的本机地址。
func e2eDefaultRelay(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return e2eDeadRelay
}
