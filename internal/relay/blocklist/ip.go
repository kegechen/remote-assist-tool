package blocklist

import (
	"fmt"
	"net/netip"
	"strings"
)

// ipv6PrefixBits IPv6 计分与封禁的聚合粒度。一个 /64 通常是一个终端子网（SLAAC 下同一台
// 机器可随时换地址），按单地址计分会被轮换地址轻易绕过，也会让跟踪表被同一来源撑爆。
const ipv6PrefixBits = 64

// NormalizeIP 严格解析单个 IPv4/IPv6 地址并返回规范形式。
//
// 去掉 zone（fe80::1%eth0 → fe80::1）、把 IPv4-mapped IPv6 归一为 IPv4。CIDR、带端口、
// 主机名一律拒绝。黑名单的键请用 KeyOf / NormalizeKey。
func NormalizeIP(s string) (string, error) {
	addr, err := parseAddr(s)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

func parseAddr(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "/ \t") {
		return netip.Addr{}, fmt.Errorf("%w: %q", ErrInvalidIP, s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %q", ErrInvalidIP, s)
	}
	return addr.WithZone("").Unmap(), nil
}

// singleAddrV6 这些 IPv6 前缀下的 /64 不对应"一个终端子网"，按 /64 聚合会把大量无关用户
// 绑在一起，因此按单地址（/128）跟踪。判定按"所在 /64 与任一前缀重叠"：前缀比 /64 长时
// （如 64:ff9b::/96），同一 /64 里前缀之外的地址也按单地址，保证一个 /64 要么整体聚合、
// 要么整体按单地址，键规则才能与 NormalizeKey 自洽（否则会产生加载时被拒的键）：
//   - Teredo 2001::/32：/64 只由 Teredo 服务器地址决定，同一服务器的所有用户共享；
//   - NAT64 64:ff9b::/96 与本地 NAT64 64:ff9b:1::/48：内嵌的是 IPv4 地址，/64 是整个转换网关；
//   - 链路本地 fe80::/10：只在本链路有意义，同一 /64 是整条链路上的所有主机。
var singleAddrV6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("fe80::/10"),
}

func isSingleAddrV6(addr netip.Addr) bool {
	p64 := netip.PrefixFrom(addr, ipv6PrefixBits).Masked()
	for _, p := range singleAddrV6 {
		if p.Overlaps(p64) {
			return true
		}
	}
	return false
}

func keyOfAddr(addr netip.Addr) string {
	if addr.Is4() || isSingleAddrV6(addr) {
		return addr.String()
	}
	return netip.PrefixFrom(addr, ipv6PrefixBits).Masked().String()
}

// prefix48 返回 IPv6 键所在的 /48（用于自动封禁的 /48 子配额）；非 IPv6 键返回空串。
func prefix48(key string) string {
	var addr netip.Addr
	if p, err := netip.ParsePrefix(key); err == nil {
		addr = p.Addr()
	} else if a, err := netip.ParseAddr(key); err == nil {
		addr = a
	} else {
		return ""
	}
	if !addr.Is6() {
		return ""
	}
	return netip.PrefixFrom(addr, 48).Masked().String()
}

// KeyOf 返回地址对应的黑名单键：IPv4 为地址本身，IPv6 一般为所在 /64 前缀（如 2001:db8:1:2::/64），
// Teredo / NAT64 / 链路本地地址为单地址（见 singleAddrV6）。
func KeyOf(ip string) (string, error) {
	addr, err := parseAddr(ip)
	if err != nil {
		return "", err
	}
	return keyOfAddr(addr), nil
}

// NormalizeKey 解析管理端输入：IPv4 地址、IPv6 地址（归到所在 /64），或显式的 IPv6 /64 前缀。
// 其他网段（IPv4 CIDR、非 /64 的 IPv6 前缀）一律拒绝——网段封禁极易误伤共享出口。
func NormalizeKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		return KeyOf(s)
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() != ipv6PrefixBits || p.Addr().Zone() != "" {
		return "", fmt.Errorf("%w: only a single address or an IPv6 /64 prefix is accepted: %q", ErrInvalidIP, s)
	}
	// 只接受 KeyOf 本身会产生的 /64 键（与特殊前缀重叠的 /64 按单地址跟踪，不存在 /64 键）。
	if m := p.Masked(); keyOfAddr(m.Addr()) != m.String() {
		return "", fmt.Errorf("%w: %q overlaps a Teredo/NAT64/link-local prefix, tracked per address; enter a single address", ErrInvalidIP, s)
	}
	return p.Masked().String(), nil
}
