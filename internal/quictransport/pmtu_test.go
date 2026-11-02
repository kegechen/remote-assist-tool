package quictransport

import "testing"

// quic-go 的 PMTUD 只会把包长往上探，路径 MTU 事后变小（Wi-Fi 漫游、换出口、隧道）时
// 只在连接迁移时才重置——之后满长度的数据包全被丢，ACK/PING 小包照样能过，QUIC 连接不
// 判空闲超时，控制流却 2 分钟收不到心跳被 relay 以 deadline exceeded 踢掉（实测 seewo
// share 经 QUIC 连 23.95.78.14 时一天出现 3 次）。控制连接流量很小，固定 1280 字节足够。
func TestPathMTUDiscoveryDisabled(t *testing.T) {
	if !ClientConfig().DisablePathMTUDiscovery {
		t.Error("client QUIC config must disable path MTU discovery")
	}
	if !ServerConfig().DisablePathMTUDiscovery {
		t.Error("server QUIC config must disable path MTU discovery")
	}
}
