package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/hkdf"
)

// 打洞包认证
//
// 为什么需要：P2PTestPacket 原先唯一的"凭据"是 sessionID，而 sessionID 会被主动喷洒
// 出去——打洞时要往对端公网 IP 的上百个端口发包，生日攻击还会用几十个 socket 同时喷。
// 接收侧只比对 sessionID 就调用 onP2PConnected(来源地址)，于是任何收到过一个杂散打洞包
// 的第三方，回发一份同样的 JSON 就能把自己冒充成对端：随后一条空 tool_req 即可触发
// swapDaemonTo 夺走 daemon 出口，或者发 MsgToolHello 逼对端 RotateKey，让合法端此后
// 每条请求都 decrypt_failed。全程不需要知道协助码。
//
// 修法是让打洞包自带一个以协助码派生的 MAC。协助码本来就是本项目的信任边界
// （"码交给谁就等于把这台机器交给谁"），用它做打洞认证不引入新的信任假设。
//
// 兼容性：旧版本不带 MAC，新版本会拒绝它们的打洞包，P2P 因此谈不成并回落 TCP relay
// （auto 模式下是无感降级，required 模式会失败）。这是批次 3 明确接受的破坏性变更。

// punchMACLen 截断后的 MAC 字节数。HMAC-SHA256 截断到 128 bit 对"在线伪造一个打洞包"
// 这种一次性、无重试价值的攻击绰绰有余，也省下 JSON 里的一半体积。
const punchMACLen = 16

// punchDirShare / punchDirHelp 绑定发送方身份，避免反射：把我发出的包原样打回来，
// 方向对不上，验不过。
const (
	punchDirShare = "share"
	punchDirHelp  = "help"
)

// derivePunchKey 从协助码派生打洞 MAC 密钥。
//
// 与 DeriveSessionKey 用不同的 info 串做域分离：两者都以协助码为密钥材料，混用会让
// 打洞包变成针对工具通道密钥的预言机。
func derivePunchKey(code string) []byte {
	hk := hkdf.New(sha256.New, []byte(code), nil, []byte("rat-p2p-punch-v"+ToolProtocolVersion))
	key := make([]byte, 32)
	io.ReadFull(hk, key)
	return key
}

// punchDir 把"是否 share 端"映射成方向串。
func punchDir(isShare bool) string {
	if isShare {
		return punchDirShare
	}
	return punchDirHelp
}

// computePunchMAC 计算打洞包 MAC。
//
// 三个字段都进 MAC：sessionID 绑定会话（换个会话的包重放不过来）、random 让每个包
// 各不相同、方向串防反射。用长度前缀而不是直接拼接，避免字段边界歧义。
func computePunchMAC(code, sessionID, random string, isShare bool) string {
	mac := hmac.New(sha256.New, derivePunchKey(code))
	for _, field := range []string{sessionID, random, punchDir(isShare)} {
		var lenPrefix [4]byte
		n := len(field)
		lenPrefix[0] = byte(n >> 24)
		lenPrefix[1] = byte(n >> 16)
		lenPrefix[2] = byte(n >> 8)
		lenPrefix[3] = byte(n)
		mac.Write(lenPrefix[:])
		mac.Write([]byte(field))
	}
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil)[:punchMACLen])
}

// SignPunchPacket 填好 pkt 的 MAC 字段。isShare 是**本端**身份。
func SignPunchPacket(pkt *P2PTestPacket, code string, isShare bool) {
	if pkt == nil || code == "" {
		return
	}
	pkt.MAC = computePunchMAC(code, pkt.SessionID, pkt.Random, isShare)
}

// VerifyPunchPacket 校验收到的打洞包。peerIsShare 是**对端**身份：本端是 help 时对端
// 是 share，反之亦然。
//
// code 为空表示本端没有可用的协助码（不该发生，防御性地一律拒绝——静默放行会把这层
// 认证悄悄变回摆设）。
func VerifyPunchPacket(pkt *P2PTestPacket, code string, peerIsShare bool) bool {
	if pkt == nil || code == "" || pkt.MAC == "" {
		return false
	}
	want := computePunchMAC(code, pkt.SessionID, pkt.Random, peerIsShare)
	return hmac.Equal([]byte(pkt.MAC), []byte(want))
}
