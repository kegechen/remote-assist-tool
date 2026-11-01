package p2p

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// FallbackSTUNServers 是内置兜底 STUN：relay 自己的 3478 不通（如云厂商安全组没放行 UDP）
// 时仍能拿到公网地址。国内公共服务器在前（实测 B 站约 20ms、小米约 0.5s），Google 的
// 域名在不少国内网络被 DNS 污染，放最后。竞速发现下某台下线只是少一个候选，不会拖慢。
var FallbackSTUNServers = []string{
	"stun.chat.bilibili.com:3478",
	"stun.miwifi.com:3478",
	"stun.cloudflare.com:3478",
	"stun.l.google.com:19302",
}

const (
	// maxAdvertisedSTUN 客户端最多采用 relay 下发的几项（与 relay 端 --advertise-stun 上限一致）。
	// 客户端自己也截断：异常或恶意的 relay 不能用长列表把内置兜底挤出候选。
	maxAdvertisedSTUN = 4
	// maxSTUNCandidates = 本端配置 1 + 下发 4 + 兜底 4。每个候选都会在 NAT 上开一条映射、发几个包。
	maxSTUNCandidates = 9
)

// MergeSTUNCandidates 按优先级合并 STUN 候选并去重：本端配置（--stun 或默认的
// relay:3478）→ relay 下发（校验后最多 maxAdvertisedSTUN 项）→ 内置兜底。
// 顺序只影响 NAT 判定选服务器时的先后，谁先应答由竞速决定。
func MergeSTUNCandidates(configured string, advertised, fallback []string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || len(out) >= maxSTUNCandidates {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(configured)
	taken := 0
	for _, s := range advertised {
		if taken >= maxAdvertisedSTUN {
			break
		}
		if s = strings.TrimSpace(s); CheckSTUNServer(s) == nil {
			add(s)
			taken++
		}
	}
	for _, s := range fallback {
		add(s)
	}
	return out
}

// CheckSTUNServer 校验一项「别处下发的」STUN 地址，relay 的 --advertise-stun 与客户端对下发
// 列表的过滤共用它：host:port，端口为 1–65535 的十进制数字；host 为单播 IPv4 字面量（不能是
// 回环、未指定、0.0.0.0/8、链路本地、多播、广播），或只含字母数字、点、连字符的域名（不能是
// localhost）。客户端的 STUN socket 只走 IPv4，IPv6 字面量也拒绝。私网地址有意放行（LAN 部署）。
// 只校验格式，不解析域名。
func CheckSTUNServer(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || len(s) > 255 {
		return fmt.Errorf("invalid STUN server %q (want host:port)", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
		return fmt.Errorf("invalid STUN server %q (port must be 1-65535)", s)
	}
	if ip := net.ParseIP(host); ip != nil {
		v4 := ip.To4()
		if v4 == nil || strings.Contains(host, ":") {
			return fmt.Errorf("invalid STUN server %q (clients use IPv4 only)", s)
		}
		if v4.IsLoopback() || v4.IsUnspecified() || v4[0] == 0 || v4.IsLinkLocalUnicast() || v4.IsMulticast() || v4.Equal(net.IPv4bcast) {
			return fmt.Errorf("invalid STUN server %q (not a unicast address reachable by clients)", s)
		}
		return nil
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return fmt.Errorf("invalid STUN server %q (localhost is not reachable by clients)", s)
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return fmt.Errorf("invalid STUN server %q (host may contain only letters, digits, '.' and '-')", s)
		}
	}
	return nil
}

// STUNProbe 是一台 STUN 服务器的应答：主 socket 在它那里的映射地址。
type STUNProbe struct {
	Server   string        // 候选原文（host:port）
	ServerIP string        // 实际发往的 IP，NAT 判定按它区分「不同服务器」
	Mapped   *net.UDPAddr  // 主 socket 的公网映射
	Elapsed  time.Duration // 从竞速开始到收到应答（含 DNS 解析）
}

// RaceOptions 控制竞速发现的时序。
type RaceOptions struct {
	Deadline       time.Duration   // 总时限；到点还没有任何应答就失败
	Retransmit     []time.Duration // 相对首次发送的重传时刻（丢包重传，沿用同一 TID）
	Grace          time.Duration   // 第一个应答后再收集多久（给 NAT 判定攒第二台）
	ResolveTimeout time.Duration   // 单个域名解析上限
}

// DefaultRaceOptions：1s、2.5s 各重传一次，4s 封顶。跨洋 STUN 的 RTT 约 0.6s，
// 留足余量；旧实现单台不通就要等 16s。
var DefaultRaceOptions = RaceOptions{
	Deadline:       4 * time.Second,
	Retransmit:     []time.Duration{time.Second, 2500 * time.Millisecond},
	Grace:          300 * time.Millisecond,
	ResolveTimeout: 2 * time.Second,
}

type raceTarget struct {
	server   string
	index    int      // 在候选列表里的位置：同址时保留靠前的名字，结果不随解析先后变化
	aliases  []string // 解析到同一地址、被合并掉的其它候选（只用于诊断）
	addr     *net.UDPAddr
	req      []byte
	tid      [12]byte
	sentAt   time.Time // 首包时刻，重传以它为基准
	resends  int
	answered bool
	dead     bool // 首包就发不出去（如本机无路由），不会有应答
}

type resolvedTarget struct {
	server string
	index  int
	addr   *net.UDPAddr
}

// RaceDiscover 在主 socket 上同时向所有候选发 Binding Request，按到达顺序返回应答
// （第一个就是采用的公网地址）。必须用打洞的那个 socket：映射是按 socket 建立的。
//
// 应答按 96 位随机 TID 认领（与旧实现一致，旧实现不校验来源）。来源地址不是请求目标
// 时仍接受并记一行日志：多 IP 的 STUN 主机可能从另一个地址回包，严格比对会让它
// 静默「不通」。调用方须保证此时没有别的 goroutine 在读这个 socket。
func RaceDiscover(conn *net.UDPConn, servers []string, opts RaceOptions) ([]STUNProbe, error) {
	start := time.Now()
	deadline := start.Add(opts.Deadline)

	// 域名并发解析：被污染或超时的域名不能挡住其它候选。channel 容量等于 goroutine 数，
	// 函数提前返回后它们也不会阻塞。
	resolved := make(chan resolvedTarget, len(servers))
	pending := 0
	var unresolved []string
	for i, s := range servers {
		host, port, err := net.SplitHostPort(s)
		if err != nil || host == "" {
			unresolved = append(unresolved, s)
			continue
		}
		pending++
		go func(i int, s, host, port string) {
			ctx, cancel := context.WithTimeout(context.Background(), opts.ResolveTimeout)
			defer cancel()
			var addr *net.UDPAddr
			if ips, err := net.DefaultResolver.LookupIPAddr(ctx, host); err == nil {
				for _, ip := range ips {
					if v4 := ip.IP.To4(); v4 != nil { // 主 socket 是 udp4
						if p, err := net.LookupPort("udp", port); err == nil {
							addr = &net.UDPAddr{IP: v4, Port: p}
						}
						break
					}
				}
			}
			resolved <- resolvedTarget{server: s, index: i, addr: addr}
		}(i, s, host, port)
	}

	var targets []*raceTarget
	var probes []STUNProbe
	var firstAt time.Time
	byAddr := make(map[string]*raceTarget) // 不同名字解析到同一地址只发一份
	buf := make([]byte, 1500)
	defer conn.SetReadDeadline(time.Time{})

	for {
		// 收下已解析完的目标并立即发首包。
	drain:
		for {
			select {
			case r := <-resolved:
				pending--
				if r.addr == nil {
					unresolved = append(unresolved, r.server)
					continue
				}
				if t := byAddr[r.addr.String()]; t != nil {
					if r.index < t.index { // 名字取候选列表里靠前的那个
						t.aliases = append(t.aliases, t.server)
						t.server, t.index = r.server, r.index
					} else {
						t.aliases = append(t.aliases, r.server)
					}
					continue
				}
				req := NewBindingRequest()
				req.AddSoftwareAttribute("remote-assist-tool/1.0")
				t := &raceTarget{server: r.server, index: r.index, addr: r.addr, req: req.Pack(), tid: req.TID, sentAt: time.Now()}
				byAddr[r.addr.String()] = t
				targets = append(targets, t)
				if _, err := conn.WriteToUDP(t.req, t.addr); err != nil {
					t.dead = true
				}
			default:
				break drain
			}
		}

		// 还可能应答的目标：没应答、首包也发出去了。解析都结束且没有这样的目标时立即结束——
		// 有应答就返回，没有就失败，不必等 Grace 或 deadline。
		now := time.Now()
		live := 0
		for _, t := range targets {
			if !t.answered && !t.dead {
				live++
			}
		}
		if (pending == 0 && live == 0) || (!firstAt.IsZero() && now.Sub(firstAt) >= opts.Grace) || now.After(deadline) {
			break
		}

		// 到期的重传：以各自首包时刻为基准，沿用同一 TID（RFC 5389 §7.2.1）。
		wake := deadline
		if !firstAt.IsZero() {
			wake = minTime(wake, firstAt.Add(opts.Grace))
		}
		for _, t := range targets {
			if t.answered || t.dead {
				continue
			}
			if t.resends < len(opts.Retransmit) && !now.Before(t.sentAt.Add(opts.Retransmit[t.resends])) {
				t.resends++
				// 重传失败不标 dead：首包已经发出，仍可能收到它的应答。
				_, _ = conn.WriteToUDP(t.req, t.addr)
			}
			if t.resends < len(opts.Retransmit) {
				wake = minTime(wake, t.sentAt.Add(opts.Retransmit[t.resends]))
			}
		}
		if pending > 0 {
			wake = minTime(wake, now.Add(20*time.Millisecond)) // 轮询解析结果
		}

		_ = conn.SetReadDeadline(wake)
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return nil, err
			}
			// 其它瞬时错误（如 Windows 上的 ICMP 端口不可达）：稍等再读，避免空转。
			time.Sleep(5 * time.Millisecond)
			continue
		}
		t, mapped := claimResponse(targets, buf[:n])
		if t == nil {
			continue
		}
		if !t.addr.IP.Equal(from.IP) || t.addr.Port != from.Port {
			log.Printf("STUN: answer for %s came from %v (multi-homed server?); accepted by transaction ID", t.server, from)
		}
		t.answered = true
		if firstAt.IsZero() {
			firstAt = time.Now()
		}
		probes = append(probes, STUNProbe{Server: t.server, ServerIP: t.addr.IP.String(), Mapped: mapped, Elapsed: time.Since(start)})
	}

	if len(probes) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSTUNTimeout, raceSummary(targets, unresolved))
	}
	return probes, nil
}

// claimResponse 按 TID 把应答认领给尚未应答的目标，返回目标与映射地址；不是 Binding
// Response、TID 对不上或没有映射地址的数据报返回 nil。
func claimResponse(targets []*raceTarget, data []byte) (*raceTarget, *net.UDPAddr) {
	resp, err := UnpackSTUN(data)
	if err != nil || resp.Type != STUNBindingResponse {
		return nil, nil
	}
	for _, t := range targets {
		if t.answered || resp.TID != t.tid {
			continue
		}
		m, err := resp.GetMappedAddress()
		if err != nil {
			return nil, nil
		}
		return t, &net.UDPAddr{IP: m.IP, Port: int(m.Port)}
	}
	return nil, nil
}

// raceSummary 列出每个候选的结局，供「全部失败」时定位：解析不了、发不出去、没有应答。
func raceSummary(targets []*raceTarget, unresolved []string) string {
	var parts []string
	for _, s := range unresolved {
		parts = append(parts, s+"=unresolved")
	}
	for _, t := range targets {
		state := "no-reply"
		if t.dead {
			state = "send-failed"
		}
		if len(t.aliases) > 0 {
			state += "(also " + strings.Join(t.aliases, ", ") + ")"
		}
		parts = append(parts, t.server+"="+state)
	}
	if len(parts) == 0 {
		return "no STUN candidates"
	}
	return "no STUN server answered (" + strings.Join(parts, ", ") + ")"
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// NATFromProbes 用竞速里已拿到的映射判定 NAT 类型：映射 IP 等于本机 IP 为 open；两台不同
// IP 的服务器看到的映射端口相同为 cone、不同为 symmetric（沿用此前临时 socket 探测的判据，
// 只是复用主 socket 的结果）。只比较映射 IP 相同的应答；数据不足返回 ""。
func NATFromProbes(localIP net.IP, probes []STUNProbe) string {
	if len(probes) == 0 {
		return NATUnknown
	}
	if localIP != nil && probes[0].Mapped.IP.Equal(localIP) {
		return NATOpen
	}
	first := probes[0]
	for _, p := range probes[1:] {
		// 同一台服务器（同 IP）不能说明映射规则；映射 IP 不同（如一台在内网、看到的是内网
		// 地址，或多出口网络）时端口也不可比。
		if p.ServerIP == first.ServerIP || !p.Mapped.IP.Equal(first.Mapped.IP) {
			continue
		}
		if p.Mapped.Port == first.Mapped.Port {
			return NATCone
		}
		return NATSymmetric
	}
	return NATUnknown
}
