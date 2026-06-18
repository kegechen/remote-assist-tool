package client

import "net"

// defaultRelayPort 是 --server 省略端口时补全的默认 relay 端口。
const defaultRelayPort = "8443"

// NormalizeServerAddr 补默认 relay 端口：传入只有 host（无 :port）时补全，
// 让 `--server 192.168.1.1` 与 `--server 192.168.1.1:8443` 等价。
// 已含端口、为空、IPv6 带括号的地址原样返回。
func NormalizeServerAddr(addr string) string {
	if addr == "" {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr // 已含端口
	}
	// 无端口（SplitHostPort 报 "missing port"）→ 补默认；JoinHostPort 会给 IPv6 加括号
	return net.JoinHostPort(addr, defaultRelayPort)
}

// IsLANServer 判断 relay 地址是否为 loopback / 私网（standalone 或同 LAN 场景）。
// 这类地址下 relay 本就是 LAN/本地直连，P2P 没有意义；且 standalone 模式不启 STUN，
// P2P 打洞必然超时失败。故 auto 模式可据此跳过 P2P（避免 8s 超时 + 后台徒劳重试）。
// required 模式不受影响——用户显式要 P2P 就尊重。
func IsLANServer(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 无端口，整串即 host
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // 普通域名 → 视为公网 relay
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
