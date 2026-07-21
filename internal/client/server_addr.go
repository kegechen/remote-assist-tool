package client

import (
	"fmt"
	"net"
	"strings"
)

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

// ValidateServerAddr 挡住带 URL scheme / 路径的 relay 地址。
//
// 必须显式挡：NormalizeServerAddr 不会拒绝这类输入，只会把它揉成更离谱的东西——
// "http://1.2.3.4:8443" 会变成 "[http://1.2.3.4:8443]:8443"（含冒号的 host 被当成 IPv6
// 加了方括号），"https://host" 则被解析成 host="https"、port="//host"。两者都要等到拨号
// 才失败，报错完全指不到根因。relay 是裸 TCP/TLS，本来也没有 URL 一说。
func ValidateServerAddr(addr string) error {
	if i := strings.Index(addr, "://"); i >= 0 {
		return fmt.Errorf("server 只接受 host:port，不能带 %q 这样的 URL scheme（relay 是裸 TCP/TLS，不是 HTTP）；例：server=\"113.44.139.100:8443\"", addr[:i+3])
	}
	if strings.ContainsAny(addr, "/?#") {
		return fmt.Errorf("server 只接受 host:port，不能带路径或查询串: %q", addr)
	}
	return nil
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
	// IsPrivate 只含 RFC1918 + fc00::/7；再补 IsLinkLocalUnicast 覆盖 169.254/16
	// （IPv4 APIPA，DHCP 失败时自分配）与 fe80::/10（IPv6 link-local）——同属同链路/
	// 本地场景，P2P 一样无意义。
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
