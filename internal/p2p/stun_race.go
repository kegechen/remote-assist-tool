package p2p

import (
	"context"
	"errors"
	"fmt"
	"net"
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

// maxSTUNCandidates 限制一次竞速的候选数：每个候选都会在 NAT 上开一条映射、发几个包。
const maxSTUNCandidates = 8

// MergeSTUNCandidates 按优先级合并 STUN 候选并去重：本端配置（--stun 或默认的
// relay:3478）→ relay 下发 → 内置兜底。空白项与重复项丢弃，最多 maxSTUNCandidates 个。
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
	for _, s := range advertised {
		add(s)
	}
	for _, s := range fallback {
		add(s)
	}
	return out
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
	addr     *net.UDPAddr
	req      []byte
	tid      [12]byte
	sentAt   time.Time // 首包时刻，重传以它为基准
	resends  int
	answered bool
}

type resolvedTarget struct {
	server string
	addr   *net.UDPAddr
}

// RaceDiscover 在主 socket 上同时向所有候选发 Binding Request，按到达顺序返回应答
// （第一个就是采用的公网地址）。必须用打洞的那个 socket：映射是按 socket 建立的。
//
// 只接受「来源 IP:port 等于某个已发请求的目标、且 TID 对得上」的 Binding Response，
// 其余数据报一律丢弃。调用方须保证此时没有别的 goroutine 在读这个 socket。
func RaceDiscover(conn *net.UDPConn, servers []string, opts RaceOptions) ([]STUNProbe, error) {
	start := time.Now()
	deadline := start.Add(opts.Deadline)

	// 域名并发解析：被污染或超时的域名不能挡住其它候选。
	resolved := make(chan resolvedTarget, len(servers))
	pending := 0
	for _, s := range servers {
		host, port, err := net.SplitHostPort(s)
		if err != nil || host == "" {
			continue
		}
		pending++
		go func(s, host, port string) {
			ctx, cancel := context.WithTimeout(context.Background(), opts.ResolveTimeout)
			defer cancel()
			var addr *net.UDPAddr
			if ips, err := net.DefaultResolver.LookupIPAddr(ctx, host); err == nil {
				for _, ip := range ips {
					if v4 := ip.IP.To4(); v4 != nil {
						if p, err := net.LookupPort("udp", port); err == nil {
							addr = &net.UDPAddr{IP: v4, Port: p}
						}
						break
					}
				}
			}
			resolved <- resolvedTarget{server: s, addr: addr}
		}(s, host, port)
	}

	var targets []*raceTarget
	var probes []STUNProbe
	var firstAt time.Time
	buf := make([]byte, 1500)
	defer conn.SetReadDeadline(time.Time{})

	send := func(t *raceTarget) {
		t.sentAt = time.Now()
		_, _ = conn.WriteToUDP(t.req, t.addr)
	}

	for {
		// 收下已解析完的目标并立即发首包。
	drain:
		for {
			select {
			case r := <-resolved:
				pending--
				if r.addr == nil {
					continue
				}
				req := NewBindingRequest()
				req.AddSoftwareAttribute("remote-assist-tool/1.0")
				t := &raceTarget{server: r.server, addr: r.addr, req: req.Pack(), tid: req.TID}
				targets = append(targets, t)
				send(t)
			default:
				break drain
			}
		}

		now := time.Now()
		allAnswered := pending == 0 && len(probes) == len(targets)
		if (!firstAt.IsZero() && (now.Sub(firstAt) >= opts.Grace || allAnswered)) ||
			now.After(deadline) || (pending == 0 && len(targets) == 0) {
			break
		}

		// 到期的重传：以各自首包时刻（sentAt，重传不改）为基准。
		wake := deadline
		if !firstAt.IsZero() {
			wake = minTime(wake, firstAt.Add(opts.Grace))
		}
		for _, t := range targets {
			if t.answered {
				continue
			}
			if t.resends < len(opts.Retransmit) && !now.Before(t.sentAt.Add(opts.Retransmit[t.resends])) {
				t.resends++
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
			continue // Windows 上 ICMP 端口不可达等瞬时错误：继续等其它候选
		}
		if t := matchTarget(targets, buf[:n], from); t != nil {
			resp, _ := UnpackSTUN(buf[:n])
			mapped, err := resp.GetMappedAddress()
			if err != nil {
				continue
			}
			t.answered = true
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			probes = append(probes, STUNProbe{
				Server:   t.server,
				ServerIP: t.addr.IP.String(),
				Mapped:   &net.UDPAddr{IP: mapped.IP, Port: int(mapped.Port)},
				Elapsed:  time.Since(start),
			})
		}
	}

	if len(probes) == 0 {
		return nil, fmt.Errorf("%w: none of %d STUN servers answered", ErrSTUNTimeout, len(servers))
	}
	return probes, nil
}

// matchTarget 校验应答：来源必须是某个未应答目标的地址，且是 TID 匹配的 Binding Response。
func matchTarget(targets []*raceTarget, data []byte, from *net.UDPAddr) *raceTarget {
	resp, err := UnpackSTUN(data)
	if err != nil || resp.Type != STUNBindingResponse {
		return nil
	}
	for _, t := range targets {
		if t.answered || !t.addr.IP.Equal(from.IP) || t.addr.Port != from.Port {
			continue
		}
		if resp.TID == t.tid {
			return t
		}
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// NATFromProbes 用竞速里已拿到的映射直接判定 NAT 类型，省掉临时 socket 的一轮查询：
// 映射 IP 等于本机 IP 为 open；两台不同 IP 的服务器看到的映射端口相同为 cone、不同为
// symmetric（与 DetectNATType 的判据一致，只是复用主 socket 的结果）。只比较映射 IP
// 相同的应答；数据不足返回 ""。
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
