package proto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// 工具通道协议版本与协商
//
// v2 相对 v1 的四处变更（都属于认证绑定）：
//  1. AEAD 带 AAD：请求/响应/流帧的密文与外层明文字段绑定，见 aad.go。
//  2. 握手后 args 必须是合法密文，空参数也要封一个 "{}"，不再有"没密文就跳过解密"的口子。
//  3. 响应与流帧同样强制带密文，空 result / 空 data 一律按损坏处理。
//  4. 接收侧按调用 ID 做抗重放滑动窗口。
//
// 版本号同时是 HKDF 的 info（DeriveSessionKey 用它做域分离），所以 v1/v2 的会话密钥本就
// 不同——这意味着版本必须在派生密钥**之前**协商定，不能事后修正。
//
// 已发布版本的分界线：0.0.1~0.0.9 是 v1，1.0.0 起是 v2。
const (
	ToolProtocolVersionV1 = "1"
	ToolProtocolVersionV2 = "2"

	// ToolProtocolVersion 本端支持的最高版本。
	ToolProtocolVersion = ToolProtocolVersionV2

	// DefaultMinProto 默认可接受的最低版本：**最低的「经过认证」的版本**，当前是 v2。
	//
	// 注意这里刻意写成 V2 而不是 ToolProtocolVersion，尽管两者今天恰好相等 —— 含义不同。
	// 要挡在门外的是 v1 这条**没有认证**的通道（无 AAD、无抗重放）：允许自动降级到它，
	// 意味着不可信的 relay 只要从 Hello 里删掉 versions 字段，就能把两个新端悄悄打回去，
	// 而且事后无从察觉。理由针对的是"不认证"，不是"不是最新"。
	//
	// 跟着最高版本走会在下一次升版本时出事：加了 v3 之后 ToolProtocolVersion 变成 "3"，
	// 默认值跟着变，于是每个 v3 端默认拒绝每个 v2 端 —— 而 v2 是一条完整认证的通道，
	// 优雅协商正是这套机制存在的意义。所以新增版本时这个常量应当留在能接受的最低
	// 认证版本上，而不是自动跟涨。
	//
	// 代价是遇到 0.0.x 会连不上。那是个体验问题，由 IncompatibleVersionError 给出的
	// 可操作提示来解决，而不是靠放宽默认值来掩盖。
	DefaultMinProto = ToolProtocolVersionV2
)

// SupportedToolVersions 本端支持的全部版本，**按优先级降序**。协商取双方交集里靠前的
// 那个，顺序即优先级，不要改成升序。
var SupportedToolVersions = []string{ToolProtocolVersionV2, ToolProtocolVersionV1}

// SideShare / SideHelp 两端角色名，用于组织版本不兼容的人话提示。收在这里而不是让
// 各调用点写字面量，免得两边一个写"被协助端"一个写"share 端"。
const (
	SideShare = "被协助端"
	SideHelp  = "协助端"
)

// versionRank 返回版本在 SupportedToolVersions 中的位次，越小越新。不支持的版本返回 -1。
func versionRank(v string) int {
	for i, s := range SupportedToolVersions {
		if s == v {
			return i
		}
	}
	return -1
}

// NormalizeMinProto 校验 --min-proto 的取值，返回可用的最低版本。
func NormalizeMinProto(v string) (string, error) {
	if v == "" {
		return DefaultMinProto, nil
	}
	if versionRank(v) < 0 {
		return "", fmt.Errorf("不支持的协议版本 %q，可选值：%v", v, SupportedToolVersions)
	}
	return v, nil
}

// IncompatibleVersionError 版本协商失败。
//
// --min-proto 只存在于新版本里，旧版（0.0.x）没有这个参数。所以「该在哪一端加」不是一个
// 需要判断的问题：**永远加在新端**，而新端必然就是发现不兼容、发出这条错误的那一端。
// 旧端那侧无论如何也加不上。
//
// 单独立一个类型而不是 fmt.Errorf，是因为这条错误有两个读者，他们在不同的机器前：
//   - Message()：塞进 HelloAck.ErrorMsg 发给对端。0.0.x 的 help 会把它原样打印
//     （"share rejected tool channel: %s"），所以旧客户端不用改代码就能看到提示。
//     读者在另一台机器上，因此文案必须用角色名指路（"在被协助端加"）。
//   - LocalHint()：在本端终端打印。读者就坐在该动手的那台机器前，说"本端"最直接。
//     这一条不能省：拒绝方与报错的显示位置常常不在一起，只发 ErrorMsg 的话，
//     share 端会安安静静地拒掉一个又一个连接，本机上什么都看不到。
type IncompatibleVersionError struct {
	// LocalSide 本端角色，也就是该加 --min-proto=1 的那一端："协助端" 或 "被协助端"。
	LocalSide string
	// PeerSide 对端角色。
	PeerSide string
	// Want 本端要求的最低版本；Got 对端能提供的最高版本（无交集时为空）。
	Want string
	Got  string
}

func (e *IncompatibleVersionError) Error() string { return e.Message() }

// peerVersionText 把对端版本渲染成人话，无交集时说"未知版本"。
func (e *IncompatibleVersionError) peerVersionText() string {
	if e.Got == "" {
		return "未知版本"
	}
	return "v" + e.Got
}

// Message 给对端看的文案（会经 HelloAck.ErrorMsg 跨进程传递）。
func (e *IncompatibleVersionError) Message() string {
	return fmt.Sprintf(
		"工具协议版本不兼容：%s要求 v%s，%s只支持 %s。请将%s升级到 1.0.0 及以上；"+
			"若暂时无法升级，可在%s加 --min-proto=1 重启以兼容模式连接"+
			"（旧版本没有该参数，只能加在新版本这一端；兼容模式会关闭 AAD 绑定与抗重放，仅限可信网络）",
		e.LocalSide, e.Want, e.PeerSide, e.peerVersionText(), e.PeerSide, e.LocalSide)
}

// LocalHint 给本端终端看的文案。读者就在该动手的机器前，直接说"本端"。
func (e *IncompatibleVersionError) LocalHint() string {
	return fmt.Sprintf(
		"工具协议版本不兼容：对端（%s）只支持 %s，本端要求 v%s，已拒绝本次连接。"+
			"请将%s升级到 1.0.0 及以上；若暂时无法升级，可在本端加 --min-proto=1 重启"+
			"（旧版本没有该参数，只能加在本端；兼容模式会关闭 AAD 绑定与抗重放，仅限可信网络）",
		e.PeerSide, e.peerVersionText(), e.Want, e.PeerSide)
}

// DeriveSessionKey 以协助码 + 两端 nonce + **协商出的版本** 派生 32 字节会话密钥。
//
// version 必须是协商结果而不是本端常量：降级到 v1 时两端都要用 "rat-tool-v1" 做 info，
// 与 0.0.x 保持一致，否则握手放行了密钥却对不上。
func DeriveSessionKey(code, nonceShare, nonceHelp, version string) [32]byte {
	if version == "" {
		version = ToolProtocolVersion
	}
	salt := []byte(nonceShare + "|" + nonceHelp)
	info := []byte("rat-tool-v" + version)
	hk := hkdf.New(sha256.New, []byte(code), salt, info)
	var key [32]byte
	io.ReadFull(hk, key[:])
	return key
}

// NoAuthCode 是 --no-auth 模式使用的固定协助码常量。
// 使用固定 code 省去 code 交换步骤，但 AEAD 会话密钥仍由
// DeriveSessionKey(NoAuthCode, randomNonce, randomNonce, version) 派生。
// 安全语义：
//   - 固定 code 是公开常量，**不防主动连接** —— 任何能访问 relay 地址的设备
//     都能连上并 exec/读写本机，仅限完全可信的私有 LAN。
//   - “防被动窃听” 依赖 TLS 传输层保护握手 nonce：默认自签 TLS 下窃听者拿不到
//     nonce、无法派生密钥；但 --plain 模式 nonce 明文 + code 公开 → 密钥可被派生、
//     加密失效，故 no-auth 不应与 --plain 同用。
//   - 限单 share：共享 relay 下多个 no-auth share 会撞同一固定 code（后注册覆盖
//     前者），故 no-auth 仅适用单 share / standalone 场景。
//
// 必须是 normalizeCode 安全的值（不含 '-'/' '/'_'）：relay 按原值存入
// byCode，而 join 端先 normalizeCode 再查表、且两端各自用本值派生会话密钥。
// 一旦含连字符会同时引发两个 bug：join 端 normalize 后查不到 byCode（报
// invalid code）、share 用原值/help 用 normalize 值派生导致 AEAD 密钥不一致。
const NoAuthCode = "noauth"

// toolCapabilities 两端通告的能力集，share 与 help 共用一份，避免两处各写一列表后漂移。
var toolCapabilities = []string{"exec", "read_file", "write_file", "list_dir", "stat", "glob", "grep", "process_list", "tail_log"}

// ToolCapabilities 返回能力集副本。
func ToolCapabilities() []string {
	out := make([]string, len(toolCapabilities))
	copy(out, toolCapabilities)
	return out
}

// NewNonceB64 生成一个 base64 编码的 16 字节随机 nonce。
func NewNonceB64() string {
	var n [16]byte
	rand.Read(n[:])
	return base64.StdEncoding.EncodeToString(n[:])
}

// SupportedVersionsDownTo 返回本端愿意接受的版本集（降序），下限为 minProto。
//
// 通告出去的版本集必须与本端真正会接受的一致：若 --min-proto=2 的 share 仍在
// HelloAck.Versions 里写上 "1"，对端就会照着这个"支持列表"提一个必然被
// NegotiateToolVersion 拒掉的版本，白费一个来回。
func SupportedVersionsDownTo(minProto string) []string { return supportedDownTo(minProto) }

// supportedDownTo 返回本端愿意接受的版本集（降序），下限为 minProto。
func supportedDownTo(minProto string) []string {
	if minProto == "" {
		minProto = DefaultMinProto
	}
	var out []string
	for _, v := range SupportedToolVersions {
		if versionRank(v) <= versionRank(minProto) {
			out = append(out, v)
		}
	}
	return out
}

// helloWithAnchor 按指定的兼容锚点造一条 Hello。
func helloWithAnchor(anchor, minProto string) Hello {
	return Hello{
		Version:      anchor,
		Versions:     supportedDownTo(minProto),
		Capabilities: ToolCapabilities(),
		NonceB64:     NewNonceB64(),
	}
}

// NewHello 生成 help 端**第一次**握手用的 Hello，Version 一律填本端最高版本。
//
// Version 字段是给看不懂 Versions 的旧实现看的兼容锚点，而它们判断的方式是**严格相等**，
// 所以一个锚点值只能讨好一代人：
//   - 0.0.x 的 share 要求它等于 "1"；
//   - **已发布的 1.0.0** 同样是严格相等，只不过要求等于 "2"，而且它没有 Versions 字段，
//     看不到我们通告的列表。
//
// 也就是说没有任何单一取值能同时兼容两者。所以这里只负责"填最高版本"——1.0.0 与所有
// 新版都能通过；0.0.x 会回一条 Accept:false，再由 NewFallbackHello 重试一轮。
// 反过来（一上来就填 "1"）会把 1.0.0 挡在门外：一个本意放宽兼容的开关反而打断当前
// 已发布版本，那是最糟的结果。
func NewHello(minProto string) Hello {
	return helloWithAnchor(ToolProtocolVersion, minProto)
}

// NewFallbackHello 生成被拒后的兼容重试 Hello，Version 填 minProto 当锚点。
//
// 只在开了兼容模式（minProto 低于最高版本）时才用得上：0.0.x 的 share 只看这个字段，
// 填 "1" 才过得了它的相等比对。这是 TLS 1.3 legacy_version 的同款手法，区别是我们把它
// 放在第二轮——第一轮要留给"版本更高但同样只认相等"的 1.0.0。
func NewFallbackHello(minProto string) Hello {
	if minProto == "" {
		minProto = DefaultMinProto
	}
	return helloWithAnchor(minProto, minProto)
}

// ShouldRetryWithFallbackAnchor 报告一次被拒的握手是否值得用兼容锚点重试。
//
// 只有开了兼容模式才重试，且只重试一轮：默认配置下被拒就是被拒，不能变着法再试一次，
// 否则"默认不降级"就成了空话。
func ShouldRetryWithFallbackAnchor(ack HelloAck, minProto string) bool {
	if ack.Accept {
		return false
	}
	if minProto == "" {
		minProto = DefaultMinProto
	}
	return minProto != ToolProtocolVersion
}

// peerVersions 从 Hello/HelloAck 里取对端支持的版本集合。
//
// 空 Versions 说明对端是不认识这个字段的旧版本（0.0.x），此时它的 Version 字段就是它
// 支持的唯一版本。
func peerVersions(single string, list []string) []string {
	if len(list) > 0 {
		return list
	}
	if single == "" {
		return nil
	}
	return []string{single}
}

// NegotiateToolVersion 按对端通告的版本集选出双方都支持、且不低于 minProto 的最高版本。
//
// localSide / peerSide 只影响失败文案（"被协助端" / "协助端"）。
func NegotiateToolVersion(single string, list []string, minProto, localSide, peerSide string) (string, error) {
	if minProto == "" {
		minProto = DefaultMinProto
	}
	peer := peerVersions(single, list)
	best := ""
	for _, v := range SupportedToolVersions {
		for _, p := range peer {
			if v == p {
				best = v
				break
			}
		}
		if best != "" {
			break
		}
	}
	// 对端最高版本，仅用于文案。走 peerBestVersion 而不是就地再写一遍循环：它会跳过
	// 本端不认识的版本号，否则对端通告 ["3","1"] 时 peerBest 会被 "3" 占住（rank 为 -1，
	// 后续比较全部失败），提示就变成"对端只支持 v3，请升级对端"——把该升级的那一端
	// 说反了，还掩盖了"本端加 --min-proto=1 其实就能连"这个事实。
	peerBest := peerBestVersion(single, list)
	if best == "" || versionRank(best) > versionRank(minProto) {
		return "", &IncompatibleVersionError{
			LocalSide: localSide,
			PeerSide:  peerSide,
			Want:      minProto,
			Got:       peerBest,
		}
	}
	return best, nil
}

// peerBestVersion 取对端通告集合里最新的那个版本，仅用于文案与拒绝判定。
func peerBestVersion(single string, list []string) string {
	best := ""
	for _, p := range peerVersions(single, list) {
		if versionRank(p) < 0 {
			continue
		}
		if best == "" || versionRank(p) < versionRank(best) {
			best = p
		}
	}
	return best
}

// InterpretHelloAck 发起方（help）解释应答方（share）的 HelloAck。
//
// 这里有一处**必须**做的校验：HelloAck.Version 是应答方单方面选定的版本，发起方不能
// 照单全收。否则一个恶意的（或被中间人改过的）应答只要回 Accept:true + Version:"1"，
// 就能把一个本来要求 v2 的发起方拽进 v1 会话——协商的意义正是在这里被绕过的。所以
// 选定版本同样要过 minProto 这道闸。
func InterpretHelloAck(ack HelloAck, minProto, localSide, peerSide string) (string, error) {
	if minProto == "" {
		minProto = DefaultMinProto
	}
	if !ack.Accept {
		// 对端拒绝了。若它能给的最高版本本来就低于本端底线，那这不是一次普通失败，
		// 而是"该升级或该开兼容模式"——给出可操作提示，而不是把对端那句干巴巴的
		// "unsupported tool protocol version" 原样抛给用户。
		if peerBest := peerBestVersion(ack.Version, ack.Versions); peerBest != "" && versionRank(peerBest) > versionRank(minProto) {
			return "", &IncompatibleVersionError{
				LocalSide: localSide,
				PeerSide:  peerSide,
				Want:      minProto,
				Got:       peerBest,
			}
		}
		return "", fmt.Errorf("对端拒绝工具通道握手：%s", ack.ErrorMsg)
	}
	chosen := ack.Version
	if chosen == "" {
		// 不填版本的应答方只可能是 0.0.x 之前的实现，按最低版本理解。
		chosen = ToolProtocolVersionV1
	}
	if versionRank(chosen) < 0 {
		return "", fmt.Errorf("对端选定了本端不支持的协议版本 %q", chosen)
	}
	if versionRank(chosen) > versionRank(minProto) {
		return "", &IncompatibleVersionError{
			LocalSide: localSide,
			PeerSide:  peerSide,
			Want:      minProto,
			Got:       chosen,
		}
	}
	// 拿对端自己的版本通告反查选定结果：Versions 是它「我支持什么」的证词，若里面存在
	// 一个双方都支持、且比 chosen 更新的版本，那这次降级就没有正当理由。
	//
	// 这堵的是开了兼容模式之后仍然存在的那条降级路径：两个 v2 端都带 --min-proto=1 时，
	// 不可信的 relay 把我们的 Hello 改写成 {"version":"1"} 并删掉 versions，对端就会
	// 「合法地」谈出 v1 —— 它看到的提议里确实只有 v1。单看 chosen 和 minProto 无法
	// 分辨这是对端真的只会 v1，还是我们的提议被人动过手脚；但对端的 Versions 里明明
	// 白白写着它支持 v2，证据就在手里，没有理由丢掉。
	//
	// 真正的 0.0.x 不发 Versions，走不到这里，兼容性不受影响。
	if newer := betterVersionThan(chosen, ack.Versions, minProto); newer != "" {
		return "", fmt.Errorf(
			"协商结果可疑：对端选定 v%s，但它同时通告支持 v%s（本端也支持）。"+
				"双方都能用更高版本却谈成了低版本，提议很可能在传输途中被篡改；已拒绝本次握手",
			chosen, newer)
	}
	return chosen, nil
}

// betterVersionThan 在 peerVersions 里找一个本端也支持、不低于 minProto、且比 chosen
// 更新的版本；没有就返回空串。
func betterVersionThan(chosen string, peerVersions []string, minProto string) string {
	for _, v := range peerVersions {
		if versionRank(v) < 0 {
			continue // 本端不认识，谈不上"更好"
		}
		if versionRank(v) > versionRank(minProto) {
			continue // 低于本端底线
		}
		if versionRank(v) < versionRank(chosen) {
			return v
		}
	}
	return ""
}
