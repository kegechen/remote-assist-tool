package p2p

import (
	"log"
	"net"
)

// NAT 类型常量
const (
	NATUnknown   = ""          // 未检测
	NATOpen      = "open"      // 公网直连（无 NAT）
	NATCone      = "cone"      // 锥型 NAT（NAT1/2/3）— 打洞友好
	NATSymmetric = "symmetric" // 对称型 NAT（NAT4）— 需要生日攻击
)

// pickTwoDistinctSTUN 从服务器列表中选取两个不同 IP 的 STUN 服务器
// 避免同 IP 不同端口导致 Symmetric NAT 误判为 Cone
func pickTwoDistinctSTUN(servers []string) (string, string, bool) {
	type resolved struct {
		addr string
		ip   string
	}
	var list []resolved
	for _, s := range servers {
		host, _, err := net.SplitHostPort(s)
		if err != nil {
			continue
		}
		ips, err := net.LookupHost(host)
		if err != nil || len(ips) == 0 {
			continue
		}
		list = append(list, resolved{addr: s, ip: ips[0]})
	}
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[i].ip != list[j].ip {
				return list[i].addr, list[j].addr, true
			}
		}
	}
	return "", "", false
}

// DetectNATType 从临时 socket 向 2 个不同 IP 的 STUN 服务器发请求，
// 比较映射端口判断 NAT 类型。
// publicAddr 是主 socket 的 STUN 映射地址，用于判断是否公网直连。
func DetectNATType(localIP net.IP, publicAddr *net.UDPAddr, stunServers []string) string {
	if publicAddr == nil {
		return NATCone // 数据不足，保守策略
	}

	// 检查是否公网直连：STUN 映射 IP 等于本机 IP
	if localIP != nil && publicAddr.IP.Equal(localIP) {
		return NATOpen
	}

	// 选取两个不同 IP 的 STUN 服务器
	server1, server2, ok := pickTwoDistinctSTUN(stunServers)
	if !ok {
		log.Printf("NAT detect: cannot find 2 STUN servers with distinct IPs, assuming Cone")
		return NATCone
	}

	// 创建临时 socket（不干扰主通讯）
	listenIP := net.IPv4zero
	if physIP := DetectPhysicalIP(); physIP != nil {
		listenIP = physIP
	}
	testConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: listenIP, Port: 0})
	if err != nil {
		log.Printf("NAT detect: failed to create test socket: %v", err)
		return NATCone
	}
	defer testConn.Close()

	// 向两个不同 STUN 服务器查询映射端口
	port1, err1 := StunQueryPort(testConn, server1)
	if err1 != nil {
		log.Printf("NAT detect: STUN query 1 (%s) failed: %v", server1, err1)
		return NATCone
	}

	port2, err2 := StunQueryPort(testConn, server2)
	if err2 != nil {
		log.Printf("NAT detect: STUN query 2 (%s) failed: %v", server2, err2)
		return NATCone
	}

	if port1 == port2 {
		log.Printf("NAT detect: same mapped port %d from both servers → Cone NAT", port1)
		return NATCone
	}

	log.Printf("NAT detect: different mapped ports %d vs %d → Symmetric NAT", port1, port2)
	return NATSymmetric
}
