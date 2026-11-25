//go:build remoteassist_e2e

package main

import "testing"

// TestE2EBuildDefaultRelay：带 remoteassist_e2e 构建时，内置默认 relay 换成 RAT_E2E_DEFAULT_RELAY，没设时是连不上的本机地址：
// e2e 里没有能用的节点列表时连的是它，绝不能连到线上 relay。
func TestE2EBuildDefaultRelay(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", e2eDeadRelay},
		{"  ", e2eDeadRelay},
		{" 127.0.0.1:18471 ", "127.0.0.1:18471"},
	} {
		if got := e2eDefaultRelay(tc.env); got != tc.want {
			t.Errorf("e2eDefaultRelay(%q) = %q, want %q", tc.env, got, tc.want)
		}
	}
	t.Setenv(relayServerEnv, "")
	if got, want := defaultRelayServer(), e2eDefaultRelay(lookupE2EDefaultRelay()); got != want {
		t.Errorf("defaultRelayServer() = %q, want %q", got, want)
	}
}
