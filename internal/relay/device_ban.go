package relay

// 按设备封禁（设计 2026-11-23 §3）：join 失败认得出设备就记到设备名单上，封的是那一台设备而不是整个出口；
// 认不出（没开设备名单、没带任何标识、放行来源）照旧记到 IP。
//
// 设备标识是客户端自报的，任何人都能伪造：它只用来把误操作归到具体设备，不是安全边界。防爆破的边界
// 仍是 IP——已识别设备的失败每 deviceFallbackEvery 次给来源 IP 记 1 分，轮换标识的爆破约 100 次封掉来源。

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/relay/blocklist"
)

// maxJoinClientIDLen join 里 client_id 的长度上限（help 发的是 "hd1_" 加 32 位十六进制）。超长的整体
// 当作没带：截断会让只在尾部不同的值落到同一台设备上，也会让只在头部不同的值各算一台。
const maxJoinClientIDLen = 64

// joinDeviceKey 本次 join 的设备键：优先 join 里的 client_id；旧版 help 没有就用来源键加身份串（同一身份串
// 换了出口算另一台设备，同一出口的不同身份串也各算各的）；都没有返回空串，失败照旧按 IP 计分。
//
// 放行来源（--blocklist-allow）上不建设备键：不计分也不拦，与 IP 名单的放行语义一致。设备名单自己判断
// 不了来源是否放行（它的键不是 IP），所以借 IP 名单的判断；设备名单开着时 IP 名单一定开着（见 NewServer）。
func (s *Server) joinDeviceKey(client *ClientConn, clientID string) string {
	if s.deviceBlocklist == nil {
		return ""
	}
	ip := clientIP(client)
	if s.blocklist.StaticAllowed(ip) {
		return ""
	}
	if clientID != "" && len(clientID) <= maxJoinClientIDLen {
		return s.deviceBlocklist.DeviceKey(blocklist.DeviceByClientID, clientID)
	}
	if client.Host == "" {
		return ""
	}
	src, err := blocklist.KeyOf(ip)
	if err != nil {
		return ""
	}
	// 来源键里没有 "|"，放在前面按第一个 "|" 切分无歧义。
	return s.deviceBlocklist.DeviceKey(blocklist.DeviceByHost, src+"|"+client.Host)
}

// rejectBlockedDevice 被封设备的 join：回「访问受限」后断开。不计失败、不计分（拒绝本身已在 CheckAccept 里计数）。
func (s *Server) rejectBlockedDevice(client *ClientConn) (closeConn bool) {
	n := atomic.AddUint64(&s.blockedSampleCtr, 1)
	if sampleHit(n, s.limits.RejectAuditSampleEvery) {
		log.Printf("Join rejected from %s: device %s is blocklisted (blocked_sample_total=%d, sample_every=%d)", clientIP(client), client.deviceKey, n, s.limits.RejectAuditSampleEvery)
	}
	msg, _ := proto.NewMessage(proto.MsgJoinResponse, &proto.JoinResponse{Success: false, Error: proto.BlockedMessage})
	sendMsg(client, msg)
	return true
}

// flagJoinFailure join 失败计分：有设备键就记到设备上（协助码类失败另累计 IP 兜底），否则照旧记到 IP。
func (s *Server) flagJoinFailure(client *ClientConn, kind blocklist.Kind, detail string) {
	key := client.deviceKey
	if key == "" || s.deviceBlocklist == nil {
		s.flagViolation(client, kind, detail)
		return
	}
	ip := clientIP(client)
	src := blocklist.DeviceSource{Via: ip, Version: client.Version, Host: client.Host}
	if s.deviceBlocklist.RecordDeviceViolation(key, src, kind, detail) {
		log.Printf("Device %s auto-banned by blocklist (via %s, last violation: %s/%s)", key, ip, kind, detail)
	}
	// 兜底只数协助码类失败本身；单连接失败到上限的那 5 分是同一批失败的加重，不重复折算到 IP。
	if kind == blocklist.KindJoinFailed && s.deviceFallback.hit(ip, time.Now()) {
		s.flagViolationIP(ip, blocklist.KindJoinFailed, "device_fallback")
	}
}

const (
	// deviceFallbackEvery 已识别设备的 join 失败每这么多次给来源 IP 记 1 分（IP 名单 20 分封禁 ≈ 100 次）。
	deviceFallbackEvery = 5
	// deviceFallbackIdle 一个来源这么久没有新的失败就丢掉它的零头计数（与 IP 名单默认的 10 分钟计分窗口一致）。
	deviceFallbackIdle = 10 * time.Minute
	// deviceFallbackMaxKeys 最多同时跟踪的来源数；满了随机丢一个，内存有上界。
	deviceFallbackMaxKeys = 16384
)

// deviceFallback 按来源键（IPv4 地址 / IPv6 /64）累计已识别设备的 join 失败，只在内存里，重启清零。
// 不按连接计：轮换标识的爆破每条连接只试一次也要累计到同一个来源上。
type deviceFallback struct {
	mu sync.Mutex
	m  map[string]fallbackCount
}

type fallbackCount struct {
	n    int
	seen time.Time
}

// hit 给 ip 的来源记一次失败，返回这次是否凑满 deviceFallbackEvery 次（满了清零）。ip 不是合法地址时返回 false。
func (f *deviceFallback) hit(ip string, now time.Time) bool {
	key, err := blocklist.KeyOf(ip)
	if err != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.m[key]
	if ok && now.Sub(c.seen) > deviceFallbackIdle {
		c = fallbackCount{}
	}
	if !ok {
		if f.m == nil {
			f.m = make(map[string]fallbackCount)
		}
		if len(f.m) >= deviceFallbackMaxKeys {
			for k := range f.m { // map 遍历顺序随机：丢一个任意的来源，最多少记它不满 5 次的零头
				delete(f.m, k)
				break
			}
		}
	}
	c.n++
	c.seen = now
	if c.n >= deviceFallbackEvery {
		delete(f.m, key)
		return true
	}
	f.m[key] = c
	return false
}

// prune 丢掉空闲超过 deviceFallbackIdle 的来源（cleanupLoop 定期调用）。
func (f *deviceFallback) prune(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, c := range f.m {
		if now.Sub(c.seen) > deviceFallbackIdle {
			delete(f.m, k)
		}
	}
}
