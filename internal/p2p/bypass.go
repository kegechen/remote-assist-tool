package p2p

import (
	"log"
	"net"
	"strings"
)

// virtualPrefixes 虚拟网卡名称前缀（TUN/TAP/容器/VPN）
var virtualPrefixes = []string{
	"utun", "tun", "tap",
	"docker", "br-", "veth",
	"wg", "tailscale",
	"clash", "wintun",
	"vethernet", "vmnet", "virbr",
	"isatap", "teredo",
	"vboxnet", "virtualbox", "vmware", // VirtualBox/VMware 虚拟网卡（host-only 无外网路由）
}

// cgnatNet 代理常用的 CGNAT 地址范围 198.18.0.0/15
var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("198.18.0.0/15")
	return n
}()

// DetectPhysicalIP 自动检测物理网卡 IP，绕过 TUN 代理
// 返回最佳物理网卡 IPv4 地址，如果检测失败返回 nil（回退到默认行为）
func DetectPhysicalIP() net.IP {
	if c := physicalIPCandidates(); len(c) > 0 {
		return c[0]
	}
	return nil
}

// physicalIPCandidates 返回可用于 P2P 的本机 IPv4，按优先级排序（第一个即 DetectPhysicalIP
// 的结果）：OS 默认路由出口 → 有广播能力的网卡 → 其余。名称前缀与 CGNAT 过滤只用于去掉
// 明显不该绑的网卡；哪块网卡的 UDP 真能出去，由 Start 里按网卡并行的 STUN 实测决定。
func physicalIPCandidates() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("DetectPhysicalIP: list interfaces: %v", err)
		return nil
	}

	type candidate struct {
		ip           net.IP
		hasBroadcast bool
	}
	var candidates []candidate

	for _, iface := range ifaces {
		// 跳过 down 或 loopback
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		// 跳过虚拟接口
		nameLower := strings.ToLower(iface.Name)
		isVirtual := false
		for _, prefix := range virtualPrefixes {
			if strings.HasPrefix(nameLower, prefix) {
				isVirtual = true
				break
			}
		}
		if isVirtual {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue
			}
			// 跳过 loopback
			if ip.IsLoopback() {
				continue
			}
			// 跳过 CGNAT 范围（代理常用）
			if cgnatNet.Contains(ip) {
				log.Printf("DetectPhysicalIP: skipping CGNAT address %v on %s", ip, iface.Name)
				continue
			}

			candidates = append(candidates, candidate{
				ip:           ip,
				hasBroadcast: iface.Flags&net.FlagBroadcast != 0,
			})
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	var out []net.IP
	seen := make(map[string]bool)
	add := func(ip net.IP) {
		if !seen[ip.String()] {
			seen[ip.String()] = true
			out = append(out, ip)
		}
	}

	// 首选 OS 默认路由出口网卡：getOutboundIP 拨 8.8.8.8 取源 IP，通常是真正能上网的
	// 物理网卡，绝不会是 host-only 的 VirtualBox/VMware 网卡（它们没有默认路由）。
	// 当 TUN 代理（Clash 等）劫持默认路由时，出口 IP 指向 TUN 网卡，而 TUN 已被
	// virtualPrefixes 过滤、不在 candidates 里，于是自然回退到下面的广播启发式，仍能绕过代理。
	// 全隧道 VPN 接管默认路由、网卡名又认不出来时，出口 IP 会是 VPN 地址——这种情况不靠
	// 猜，由 Start 实测各候选的 STUN 连通性后改绑。
	if outbound := getOutboundIP(); outbound != nil {
		for _, c := range candidates {
			if c.ip.Equal(outbound) {
				add(c.ip)
			}
		}
	}

	// 其次：有广播能力的接口（物理网卡通常有）
	for _, c := range candidates {
		if c.hasBroadcast {
			add(c.ip)
		}
	}
	for _, c := range candidates {
		add(c.ip)
	}
	return out
}
