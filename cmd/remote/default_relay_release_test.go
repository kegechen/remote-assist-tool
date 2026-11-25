//go:build !remoteassist_e2e

package main

import (
	"net"
	"testing"
)

// TestReleaseBuildDefaultRelayIsPublic：正式构建（不带 remoteassist_e2e）的内置默认 relay 不能是本机地址。
// e2e 的替换（default_relay_e2e.go）只能编进带 tag 的构建，tag 丢了的话所有没写 --server 的用户都连不上。
func TestReleaseBuildDefaultRelayIsPublic(t *testing.T) {
	host, _, err := net.SplitHostPort(defaultRelayServerAddr)
	if err != nil {
		t.Fatalf("内置默认 relay %q 不是 host:port: %v", defaultRelayServerAddr, err)
	}
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		t.Fatalf("内置默认 relay 是本机地址 %q", defaultRelayServerAddr)
	}
}
