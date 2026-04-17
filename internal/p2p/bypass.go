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
}

// cgnatNet 代理常用的 CGNAT 地址范围 198.18.0.0/15
var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("198.18.0.0/15")
	return n
}()

// DetectPhysicalIP 自动检测物理网卡 IP，绕过 TUN 代理
// 返回最佳物理网卡 IPv4 地址，如果检测失败返回 nil（回退到默认行为）
func DetectPhysicalIP() net.IP {
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

	// 优先选择有广播能力的接口（物理网卡通常有）
	for _, c := range candidates {
		if c.hasBroadcast {
			return c.ip
		}
	}

	return candidates[0].ip
}
