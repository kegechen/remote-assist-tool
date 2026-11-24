package blocklist

// 启动参数 --blocklist-allow：写死的放行名单（IP 或 CIDR）。
//
// 没有运行时管理入口时，部署者最常见的需求是「别把我们自己的出口 / 公司 NAT 封了」。
// 命中的来源：永远不计分、不被 accept 拒绝（即使名单文件里还留着它的旧封禁），也不能被手动封禁。
// 设备 ID（dev-…）条目等按设备封禁落地后再支持。

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/remote-assist/tool/internal/logger"
)

// MaxStaticAllow 放行条目数上限：accept 热路径对每个连接线性比对，条目数必须有界。
const MaxStaticAllow = 256

// StaticAllowActor 启动时因放行名单而解除封禁，写进封禁记录与审计的操作人。
const StaticAllowActor = "config"

// ErrStaticAllowed 手动封禁的目标完全落在启动参数的放行范围内：放行优先，封禁不会生效，所以明确拒绝而不是静默无效。
var ErrStaticAllowed = errors.New("ip is allowlisted by --blocklist-allow")

// ParseStaticAllow 解析逗号分隔的放行名单：单个 IP（IPv4 / IPv6，v4-mapped 按 IPv4 处理）或 CIDR（主机位清零）。
// 空串返回 nil。空条目、非法写法、/0（等于放行所有人，该关黑名单就用 --blocklist-enabled=false）、带 zone 的地址、
// 设备 ID、超过 MaxStaticAllow 条都报错——启动参数写错要立刻失败，不能带着错误配置悄悄运行。
func ParseStaticAllow(spec string) ([]netip.Prefix, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	items := strings.Split(spec, ",")
	if len(items) > MaxStaticAllow {
		return nil, fmt.Errorf("--blocklist-allow accepts at most %d entries, got %d", MaxStaticAllow, len(items))
	}
	out := make([]netip.Prefix, 0, len(items))
	for _, raw := range items {
		item := strings.TrimSpace(raw)
		if item == "" {
			return nil, errors.New("--blocklist-allow has an empty entry")
		}
		if strings.HasPrefix(strings.ToLower(item), "dev-") {
			return nil, fmt.Errorf("--blocklist-allow entry %q: device IDs are not supported yet", item)
		}
		p, err := parseStaticAllowItem(item)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseStaticAllowItem(item string) (netip.Prefix, error) {
	if strings.Contains(item, "/") {
		p, err := netip.ParsePrefix(item)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("--blocklist-allow entry %q is not a valid CIDR", item)
		}
		if p.Addr().Is4In6() {
			return netip.Prefix{}, fmt.Errorf("--blocklist-allow entry %q: write IPv4 ranges as IPv4 CIDR", item)
		}
		if p.Bits() == 0 {
			return netip.Prefix{}, fmt.Errorf("--blocklist-allow entry %q (/0) would allow every address; use --blocklist-enabled=false to turn the blocklist off", item)
		}
		return p.Masked(), nil
	}
	addr, err := netip.ParseAddr(item)
	if err != nil || addr.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("--blocklist-allow entry %q is not a valid IP address", item)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// staticAllowed 报告 ip 是否落在放行范围内。accept 热路径：没配置时只比一次长度。
func (l *List) staticAllowed(ip string) bool {
	if len(l.cfg.StaticAllow) == 0 {
		return false
	}
	addr, err := parseAddr(ip) // 已去 zone、v4-mapped 已还原为 IPv4
	if err != nil {
		return false
	}
	for _, p := range l.cfg.StaticAllow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// staticCovers 报告黑名单键是否**整个**落在放行范围内。IPv4 键是单个地址；IPv6 键一般是 /64 前缀，
// 只有放行范围不比 /64 更窄时才算覆盖——放行的只是 /64 里的某个地址时，这个 /64 对其他地址仍可封，
// 被放行的那个地址靠 CheckAccept 的优先放行绕过（与自动封禁一致）。
func (l *List) staticCovers(key string) bool {
	if len(l.cfg.StaticAllow) == 0 {
		return false
	}
	if kp, err := netip.ParsePrefix(key); err == nil {
		for _, p := range l.cfg.StaticAllow {
			if p.Bits() <= kp.Bits() && p.Contains(kp.Addr()) {
				return true
			}
		}
		return false
	}
	addr, err := netip.ParseAddr(key)
	if err != nil {
		return false
	}
	for _, p := range l.cfg.StaticAllow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// releaseStaticAllowed 启动时解除名单文件里已有的、完全落在放行范围内的封禁（自动或手动），
// 封禁记录以人工解禁结束、操作人为 StaticAllowActor。放行名单是运维显式写下的意图，
// 优先于文件里残留的旧封禁；否则这些封禁在名单和封禁记录里显示「封禁中」，实际却被 accept 放行，对不上。
func (l *List) releaseStaticAllowed() {
	if len(l.cfg.StaticAllow) == 0 {
		return
	}
	now := l.now()
	var audits []auditRec
	l.mu.Lock()
	var hits []*entry
	for _, e := range l.entries {
		if e.Ban != nil && l.staticCovers(e.IP) {
			hits = append(hits, e)
		}
	}
	for _, e := range hits {
		prev := e.Ban
		l.removeAutoBan(e)
		l.recordBanEnd(e, EndUnban, now, StaticAllowActor)
		e.Ban = nil
		e.hits = nil
		if r := l.memOf(e.IP).get(e.IP); r != nil {
			r.Offenses, r.LastBanEnd = 0, 0
		}
		e.addHistory(historyRec{TS: now, Action: "unban", Actor: StaticAllowActor, Detail: "启动参数 --blocklist-allow 放行，解除原" + reasonText(prev.Reason) + "封禁"})
		audits = append(audits, auditRec{logger.AuditLevelInfo, "ip_unbanned", fmt.Sprintf("IP %s 在 --blocklist-allow 放行范围内，启动时解除原%s封禁", e.IP, reasonText(prev.Reason)), map[string]interface{}{
			"ip": e.IP, "actor": StaticAllowActor, "prev_reason": prev.Reason,
		}})
		l.place(e, now, true)
	}
	l.mu.Unlock()
	l.emit(audits)
}
