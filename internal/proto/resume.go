package proto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"

	"golang.org/x/crypto/hkdf"
)

// P2P 续会话证明
//
// 场景：relay 重启，或某一端到 relay 的连接闪断，而两端之间的 P2P 隧道仍然通着。help 会
// 重新加入 relay（新连接，可能连的是重启后的新 relay 进程），share 随后收到一条
// SessionReady。share 必须判断这次加入的是不是正在和自己跑 P2P 的那个协助端：
//   - 是：保留 P2P 隧道和会话密钥，工具调用不中断；
//   - 不是，或者无法证明：照旧拆掉旧隧道，为新协助端重新协商。
//
// 不能用别的条件代替这份证明：
//   - 会话 ID 相同不够：同一个码的另一个协助端在去抖窗口内加入，拿到的也是同一个会话 ID；
//     relay 重启后会话 ID 还会变。
//   - 「隧道还活着」更不够：旧协助端的隧道活着，恰恰说明换人时必须把它拆掉。
//
// 证明用工具会话密钥计算。密钥由协助码和双方握手 nonce 派生（DeriveSessionKey），只有
// 完成过那次握手的协助端才有；另一个只知道协助码的人算不出来。
//
// relay 只透传：JoinRequest.ResumeProof 原样放进 SessionReady.ResumeProof，不校验，也没法
// 校验。旧 relay 不认识这个字段，会直接丢掉；share 收不到证明，就一律按换人处理，也就是
// 回到旧行为，不会把别人误认成同一个协助端。
//
// 信任边界：relay 本来就看得到协助码和握手 nonce，重放一份证明之类的事并没有超出它原有
// 的能力。这份证明防的是「另一个拿到了协助码的协助端」。

const (
	// resumeKeyInfo 续会话 MAC 密钥的 HKDF info。与工具通道 AEAD 用的密钥做域分离：
	// 不直接拿会话密钥做 HMAC，免得同一把钥匙跨两种用途。
	resumeKeyInfo  = "rat-p2p-resume-v1"
	resumeNonceLen = 16
	// resumeMACLen 截断到 128 bit，理由同 punchMACLen：这是一次性的在线伪造，没有离线
	// 穷举的余地。
	resumeMACLen = 16
	// 方向标签：help 出的证明和 share 回的确认用不同标签，防止把一方的值原样反射给另一方。
	resumeDirProof = "help-proof"
	resumeDirAck   = "share-ack"
	// maxResumeFieldLen relay 透传前的长度上限。合法值是 16 字节 base64（22 个字符），
	// 这里留足余量，只挡畸形和超长的数据。
	maxResumeFieldLen = 64
)

// P2PResumeProof help 重新加入 relay 时附带的续会话证明（JoinRequest / SessionReady 的字段）。
type P2PResumeProof struct {
	Nonce string `json:"nonce"`
	MAC   string `json:"mac"`
}

// P2PResumeAck share 接受续会话后，经 P2P 隧道回给 help 的确认（MsgP2PResumeAck 的载荷）。
// Nonce 原样回显证明里的 nonce，把这份确认绑定到这一次重新加入。
type P2PResumeAck struct {
	Nonce string `json:"nonce"`
	MAC   string `json:"mac"`
}

// WellFormed 只做形状检查，供 relay 决定是否透传。不涉及密码学校验。
func (p *P2PResumeProof) WellFormed() bool {
	return p != nil &&
		p.Nonce != "" && len(p.Nonce) <= maxResumeFieldLen &&
		p.MAC != "" && len(p.MAC) <= maxResumeFieldLen
}

func deriveResumeKey(sessKey [32]byte) []byte {
	hk := hkdf.New(sha256.New, sessKey[:], nil, []byte(resumeKeyInfo))
	key := make([]byte, 32)
	io.ReadFull(hk, key)
	return key
}

// computeResumeMAC 字段用长度前缀拼接，避免字段边界歧义（同 computePunchMAC）。
func computeResumeMAC(sess Session, code, nonce, dir string) string {
	mac := hmac.New(sha256.New, deriveResumeKey(sess.Key))
	for _, field := range []string{code, nonce, dir} {
		n := len(field)
		mac.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		mac.Write([]byte(field))
	}
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil)[:resumeMACLen])
}

// NewP2PResumeProof 用当前工具会话生成一份续会话证明，每次调用 nonce 都不同。
func NewP2PResumeProof(sess Session, code string) (*P2PResumeProof, error) {
	if !sess.Active() || code == "" {
		return nil, errors.New("no active tool session to prove")
	}
	var raw [resumeNonceLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	nonce := base64.RawStdEncoding.EncodeToString(raw[:])
	return &P2PResumeProof{Nonce: nonce, MAC: computeResumeMAC(sess, code, nonce, resumeDirProof)}, nil
}

// VerifyP2PResumeProof share 端校验。会话未激活、码为空或证明缺字段时一律不通过：
// 不通过的后果只是重新协商，放行的后果是把隧道留给一个没证明身份的人。
func VerifyP2PResumeProof(p *P2PResumeProof, sess Session, code string) bool {
	if !p.WellFormed() || !sess.Active() || code == "" {
		return false
	}
	want := computeResumeMAC(sess, code, p.Nonce, resumeDirProof)
	return hmac.Equal([]byte(p.MAC), []byte(want))
}

// NewP2PResumeAck share 端在接受证明后生成确认。
func NewP2PResumeAck(p *P2PResumeProof, sess Session, code string) *P2PResumeAck {
	return &P2PResumeAck{Nonce: p.Nonce, MAC: computeResumeMAC(sess, code, p.Nonce, resumeDirAck)}
}

// VerifyP2PResumeAck help 端校验确认：必须回显本次证明的 nonce，且 MAC 用的是确认方向的标签。
func VerifyP2PResumeAck(a *P2PResumeAck, p *P2PResumeProof, sess Session, code string) bool {
	if a == nil || !p.WellFormed() || a.Nonce != p.Nonce || a.MAC == "" || !sess.Active() || code == "" {
		return false
	}
	want := computeResumeMAC(sess, code, p.Nonce, resumeDirAck)
	return hmac.Equal([]byte(a.MAC), []byte(want))
}
