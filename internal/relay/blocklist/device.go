package blocklist

// 按设备封禁（设计 2026-11-23 §3）：relay 另开一个 List，Config.Keyer = DeviceKeyer，键是 DeviceKey 派生的
// dev- 键。阈值、窗口、时长递增、衰减、解禁/白名单全部沿用同一套实现；不同的只有键空间、文件里的 via、
// 临时文件命名、封禁记录的对象类型与审计事件名。
//
// 设备标识是客户端自报的，可以伪造：它只用来把误操作归到具体设备，不是安全边界。防爆破靠的仍是
// IP 名单（relay 对已识别设备的失败另给来源 IP 记兜底分）。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrInvalidDevice 设备名单收到的不是规范的设备键（dev- 加 24 位小写十六进制）。
var ErrInvalidDevice = errors.New("invalid device key")

// Keyer 名单的键空间。只能用本包提供的取值：Config.Keyer 为 nil 是 IP 名单，DeviceKeyer 是设备名单。
type Keyer struct {
	keyOf     func(string) (string, error) // 观察与计分入口：把来源归到键
	normalize func(string) (string, error) // 管理入口、文件加载与写盘自检：校验并规范化键
	target    string                       // 封禁记录里的对象类型
	tmpPrefix string                       // 写盘临时文件前缀；同目录的名单各用各的，打开时只清理自己的残留
	device    bool

	// 审计：IP 名单的事件名、文案、字段名与加入设备名单之前逐字相同（audit_messages_test.go）。
	event string // 事件名前缀
	noun  string // 文案里的对象
	field string // 审计 details 里键的字段名
}

// deviceTmpPrefix 设备名单写盘的临时文件前缀。不能用 IP 名单的 ".blocklist-"：两本名单同目录，
// IP 名单打开时会清理那个命名下的残留，会误删设备名单正在写的临时文件（反之亦然）。
const deviceTmpPrefix = ".bldev-"

var ipKeyer = &Keyer{
	keyOf: KeyOf, normalize: NormalizeKey, target: TargetIP, tmpPrefix: tmpPrefix,
	event: "ip_", noun: "IP", field: "ip",
}

// DeviceKeyer 设备名单的键空间：只接受规范的 dev- 键（见 DeviceKey），IP 与其他字符串一律不认。
var DeviceKeyer = &Keyer{
	keyOf: NormalizeDeviceKey, normalize: NormalizeDeviceKey, target: TargetDevice, tmpPrefix: deviceTmpPrefix, device: true,
	event: "device_", noun: "设备", field: "device",
}

// warnNoun 处置提示里的对象，前后的空格按中英文混排留好：「该 IP 在白名单中」「该设备在白名单中」。
func (k *Keyer) warnNoun() string {
	if k.device {
		return k.noun
	}
	return " " + k.noun + " "
}

// NormalizeDeviceKey 校验设备键：去掉首尾空白后必须是 dev- 加 24 位小写十六进制。
func NormalizeDeviceKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !isDeviceKey(s) {
		return "", ErrInvalidDevice
	}
	return s, nil
}

// DeviceIDSource 设备标识的来源类别。参与派生：同一个字符串当 ClientID 和当身份串得到的设备键不同。
type DeviceIDSource string

const (
	// DeviceByClientID join 里带的 client_id（help 由本机持久 ClientID 与 relay 证书指纹单向派生的设备标识）。
	DeviceByClientID DeviceIDSource = "client_id"
	// DeviceByHost 旧版 help 没有 ClientID：身份串（user@host 系统 架构）与来源键的组合，由调用方拼好。
	DeviceByHost DeviceIDSource = "host"
)

// DeviceKey 由来源类别与标识派生设备键：dev- + hex(HMAC-SHA256(salt, 类别 | 标识)[:12])。salt 是本名单
// 文件自己的（随文件持久化，跨重启稳定），原始标识不落盘。标识为空、类别未知或这不是设备名单时返回空串。
func (l *List) DeviceKey(src DeviceIDSource, id string) string {
	if l == nil || !l.keyer.device || id == "" || (src != DeviceByClientID && src != DeviceByHost) {
		return ""
	}
	h := hmac.New(sha256.New, l.salt)
	_, _ = h.Write([]byte(src))
	_, _ = h.Write([]byte{0}) // 类别里不含 0 字节，「类别 | 标识」的拼接不会有两种切法
	_, _ = h.Write([]byte(id))
	return "dev-" + hex.EncodeToString(h.Sum(nil)[:12])
}

// DeviceSource 一次设备违规的上下文，只用于展示：Via 是来源 IP（记成条目的 via，最近一次为准），
// Version、Host 是客户端自报的版本与身份串（记进 clients，角色 help）。
type DeviceSource struct {
	Via     string
	Version string
	Host    string
}

// RecordDeviceViolation 同 RecordViolation，另把来源与客户端信息记到设备条目上。IP 名单上等同
// RecordViolation：不写 via、不记客户端，blocklist.json 与加入设备名单之前同构。
func (l *List) RecordDeviceViolation(key string, src DeviceSource, kind Kind, detail string) bool {
	if l == nil {
		return false
	}
	if !l.keyer.device {
		return l.RecordViolation(key, kind, detail)
	}
	return l.recordViolation(key, kind, detail, &src)
}

// StaticAllowed 报告 ip 是否落在启动参数 --blocklist-allow 的放行范围内。relay 据此让放行来源上的设备
// 也不计分、不被拒（与 IP 名单的放行语义一致）。
func (l *List) StaticAllowed(ip string) bool {
	return l != nil && l.staticAllowed(ip)
}

// noteDeviceSource 把来源与客户端信息写进设备条目。调用方持有写锁。
func (e *entry) noteDeviceSource(src *DeviceSource, now int64) {
	if via := validVia(src.Via); via != "" {
		e.Via = via
	}
	host, version := cleanField(src.Host), cleanField(src.Version)
	if host != "" || version != "" {
		e.noteClient("help", version, host, "", false, now)
	}
}

// validVia via 只用于展示，必须是单个地址（规范形式）；否则为空。
func validVia(s string) string {
	addr, err := parseAddr(s)
	if err != nil {
		return ""
	}
	return addr.String()
}

// latestHost 最近出现的客户端身份串，封禁记录里作为设备的标签。
func latestHost(clients []clientRec) string {
	best := -1
	for i := range clients {
		if clients[i].Host != "" && (best < 0 || clients[i].LastSeen >= clients[best].LastSeen) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return clients[best].Host
}
