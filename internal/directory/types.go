// Package directory 实现多 relay 节点列表的签名格式、验签与本地缓存。
//
// 信任结构（详见 docs/design/specs/2026-11-23-multi-relay-directory-design.md）：
//
//	根密钥（离线，主/备）  签「节点证书」「状态密钥授权」「吊销列表」
//	状态密钥（CF 上）      签整份「节点列表」，只能改状态/权重/策略，加不了节点
//
// 客户端与 relay 内置根公钥，逐层验签后才使用一个节点。本包是信任根，改动须格外谨慎：
// 任何放宽（跳过某一层、接受未知 signer、时间/序号检查退化）都等于让攻击者能把
// 协助码交给自己的 relay。
package directory

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// FormatVersion 是节点列表格式版本。
const FormatVersion = 1

// 签名域前缀：签名作用于 域前缀 || kind || 0x00 || payload，而不是裸 payload。
// 这样为某一种对象（比如节点证书）签的名，绝不可能被当成另一种对象（比如状态密钥授权）重放。
const sigDomain = "remote-assist-directory-v1"

// Kind 是被签名对象的种类，写在信封里并参与签名。
type Kind string

const (
	KindNodeCert    Kind = "node-cert"
	KindStatusKey   Kind = "status-key"
	KindRevocations Kind = "revocations"
	KindDirectory   Kind = "directory"
)

// 签名者名字。根密钥固定两把；状态密钥签整份列表。
const (
	SignerPrimary = "primary"
	SignerBackup  = "backup"
	SignerStatus  = "status"
)

// 策略。
const (
	PolicyPreferDefault = "prefer-default"
	PolicyFastest       = "fastest"
)

// 节点状态。
const (
	StatusActive   = "active"
	StatusDraining = "draining"
	StatusDisabled = "disabled"
)

// 上限：列表由网络传来，必须限死，防资源耗尽与终端注入。
const (
	MaxRawSize      = 256 << 10 // 整份信封
	MaxNodes        = 32
	MaxAddrsPerKind = 4
	MaxTLSFPs       = 2 // 当前 + 下一张，用于无缝换证书
	MaxFieldLen     = 64
	MaxListLifetime = 45 * 24 * time.Hour // 列表自身有效期上限，限制回放窗口
	ClockSkew       = 24 * time.Hour
	// 吊销条目上限：同一把根签过的吊销取并集后也不能超过。客户端要持久保存吊销，不设上限的话，
	// 握有根私钥的人（包括泄露后、被吊销前的主根）分多份列表就能把本地缓存撑爆。
	MaxRevokedNodes = 1024
	MaxRevokedRoots = 4
)

// CodeCharset 与 relay 协助码字符集（internal/relay/code.go）保持一致；节点前缀取自其中。
// 两处一致性由 internal/relay/directory_charset_test.go 保证。
const CodeCharset = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// Envelope 是所有签名对象的统一外壳。签名覆盖 Kind 与 Payload 的原始字节，不需要 JSON 规范化。
type Envelope struct {
	Kind    Kind   `json:"kind"`
	Payload []byte `json:"payload"` // JSON 序列化的对象，JSON 里是 base64
	Sig     []byte `json:"sig"`
	Signer  string `json:"signer"`
}

// NodeCert 是根密钥签发的节点证书：把「前缀 + 地址 + TLS 证书指纹」绑在一起。
type NodeCert struct {
	Serial   uint64   `json:"serial"`
	Prefix   string   `json:"prefix"` // 单个码字符；协助码首位
	Name     string   `json:"name"`
	Operator string   `json:"operator"`
	Official bool     `json:"official"`
	Region   string   `json:"region,omitempty"`
	TCP      []string `json:"tcp"`
	QUIC     []string `json:"quic,omitempty"`
	// TLSFPs 是 relay 证书 DER 的 sha256 小写 hex（同 crypto.CertFingerprint），最多 2 个，
	// 换证书时先签「旧+新」，切换完成后再签「新」。
	TLSFPs    []string `json:"tls_fps"`
	NotBefore int64    `json:"not_before"`
	NotAfter  int64    `json:"not_after"`
}

// StatusKeyCert 是根密钥对状态密钥的授权。
type StatusKeyCert struct {
	Serial    uint64 `json:"serial"` // 递增；客户端只接受不低于已见最高值的授权
	PubKey    []byte `json:"pubkey"` // ed25519 公钥，JSON 里是 base64
	NotBefore int64  `json:"not_before"`
	NotAfter  int64  `json:"not_after"`
}

// Revocations 是根密钥签发的吊销列表。客户端按签发它的根分别对见过的吊销取并集并持久保存，
// 所以新列表应当包含此前全部条目（relay-admin 的台账保证这一点）。
type Revocations struct {
	Serial      uint64   `json:"serial"`
	IssuedAt    int64    `json:"issued_at"`
	NodeSerials []uint64 `json:"node_serials,omitempty"`
	// Roots 是被吊销的根公钥指纹（RootFingerprint），不是名字：发版换上新公钥后，旧吊销不会波及新公钥。
	Roots []string `json:"roots,omitempty"`
	// MinStatusSerial 声明授权序号低于它的状态密钥一律作废。吊销主根时必须带上：主根被吊销后，
	// 它授权过的位置作废，此前被取代的旧状态密钥（哪怕由备用根授权）不能借此重新生效。
	MinStatusSerial uint64 `json:"min_status_serial,omitempty"`
}

// NodeEntry 是列表中的一项：根签名的证书 + CF 可改的状态与权重。
type NodeEntry struct {
	Cert   Envelope `json:"cert"`
	Status string   `json:"status"`
	Weight int      `json:"weight"` // 0–100，0 表示不参与自动选择
}

// Body 是状态密钥签名的节点列表正文。
type Body struct {
	Format        int         `json:"format"`
	Serial        uint64      `json:"serial"`
	IssuedAt      int64       `json:"issued_at"`
	ExpiresAt     int64       `json:"expires_at"`
	Policy        string      `json:"policy"`
	DefaultPrefix string      `json:"default_prefix"`
	StatusKey     Envelope    `json:"status_key"`
	Revocations   *Envelope   `json:"revocations,omitempty"`
	Nodes         []NodeEntry `json:"nodes"`
}

// 错误。用 errors.Is 判断；VerifyError 额外带出出错的层，方便写日志。
var (
	ErrNoRoots      = errors.New("directory: no root keys configured")
	ErrMalformed    = errors.New("directory: malformed")
	ErrBadSignature = errors.New("directory: bad signature")
	ErrExpired      = errors.New("directory: expired")
	ErrNotYetValid  = errors.New("directory: not yet valid")
	ErrRollback     = errors.New("directory: serial rolled back")
	ErrRevoked      = errors.New("directory: revoked")
)

// VerifyError 说明验证在哪一层、因为什么失败。Unwrap 返回上面的哨兵错误。
type VerifyError struct {
	Layer string // directory / status_key / revocations / node <prefix>
	Err   error
	Msg   string
}

func (e *VerifyError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("%s: %v", e.Layer, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Layer, e.Err, e.Msg)
}

func (e *VerifyError) Unwrap() error { return e.Err }

// verr 构造 VerifyError。Msg 里常有没验过签的内容（未知字段名、签名者名、解析错误），验签之前就会出现，
// 而它会打到终端、写进 relay 每次刷新的日志：截短并转义，同 Fetch 的错误。
func verr(layer string, err error, format string, args ...any) *VerifyError {
	msg := ""
	if format != "" {
		msg = clipPrintable(fmt.Sprintf(format, args...))
	}
	return &VerifyError{Layer: layer, Err: err, Msg: msg}
}

// signedMessage 是真正被签名/验签的字节串。
func signedMessage(kind Kind, payload []byte) []byte {
	msg := make([]byte, 0, len(sigDomain)+len(kind)+2+len(payload))
	msg = append(msg, sigDomain...)
	msg = append(msg, 0)
	msg = append(msg, kind...)
	msg = append(msg, 0)
	msg = append(msg, payload...)
	return msg
}

// Sign 用 priv 为 payload 生成信封。
func Sign(priv ed25519.PrivateKey, kind Kind, signer string, payload []byte) Envelope {
	return Envelope{
		Kind:    kind,
		Payload: append([]byte(nil), payload...),
		Sig:     ed25519.Sign(priv, signedMessage(kind, payload)),
		Signer:  signer,
	}
}

// verifyEnvelope 校验信封 kind 与签名。pub 为 nil 视为验不过。
func verifyEnvelope(env Envelope, want Kind, pub ed25519.PublicKey) bool {
	if env.Kind != want || len(pub) != ed25519.PublicKeySize || len(env.Sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, signedMessage(env.Kind, env.Payload), env.Sig)
}

// signObject 序列化 v 并签名。
func signObject(priv ed25519.PrivateKey, kind Kind, signer string, v any) (Envelope, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return Envelope{}, err
	}
	return Sign(priv, kind, signer, payload), nil
}

// 以下四个是签发入口（relay-admin、Worker 之外的 Go 侧测试夹具都用它们）。

// IssueNodeCert 用根密钥签发节点证书。签发前先按验证端同一套规则校验内容，避免签出一张客户端必然拒收的证书。
func IssueNodeCert(root ed25519.PrivateKey, signer string, c NodeCert) (Envelope, error) {
	if err := ValidateNodeCert(&c); err != nil {
		return Envelope{}, err
	}
	return signObject(root, KindNodeCert, signer, c)
}

// IssueStatusKey 用根密钥授权一把状态公钥。
func IssueStatusKey(root ed25519.PrivateKey, signer string, c StatusKeyCert) (Envelope, error) {
	if len(c.PubKey) != ed25519.PublicKeySize {
		return Envelope{}, fmt.Errorf("%w: status key pubkey must be %d bytes", ErrMalformed, ed25519.PublicKeySize)
	}
	if c.Serial == 0 || c.NotAfter <= c.NotBefore {
		return Envelope{}, fmt.Errorf("%w: status key serial/validity", ErrMalformed)
	}
	return signObject(root, KindStatusKey, signer, c)
}

// IssueRevocations 用根密钥签发吊销列表。
func IssueRevocations(root ed25519.PrivateKey, signer string, r Revocations) (Envelope, error) {
	pub, _ := root.Public().(ed25519.PublicKey)
	if err := checkRevocations(signer, RootFingerprint(pub), r); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return signObject(root, KindRevocations, signer, r)
}

// checkRevocations 校验吊销列表内容，签发端与验证端共用。signerFP 是签发它的根公钥指纹。
//
// 根吊销只能由备用根签，且不能吊销它自己。备用根离线保存、平时不用，是主根泄露后的恢复钥匙；
// 若主根也能吊销根，拿到泄露主根的攻击者就能抢先吊销备用根，把恢复路径堵死。
// 备用根自己泄露时，只能靠发版把它写进内置吊销（Roots.RevokedRoots）。
func checkRevocations(signer, signerFP string, r Revocations) error {
	if r.Serial == 0 {
		return errors.New("serial must be > 0")
	}
	if len(r.NodeSerials) > MaxRevokedNodes || len(r.Roots) > MaxRevokedRoots {
		return fmt.Errorf("more than %d node serials or %d roots", MaxRevokedNodes, MaxRevokedRoots)
	}
	for _, s := range r.NodeSerials {
		if s == 0 {
			return errors.New("node serial 0")
		}
	}
	for _, fp := range r.Roots {
		switch {
		case !ValidFingerprint(fp):
			return fmt.Errorf("revoked root %q is not a key fingerprint", fp)
		case signer != SignerBackup:
			return fmt.Errorf("roots can only be revoked by the backup root, not %q", signer)
		case fp == signerFP:
			return errors.New("a root cannot revoke itself")
		}
	}
	return nil
}

// SignDirectory 用状态密钥签整份列表，返回可直接发布的 JSON。
func SignDirectory(status ed25519.PrivateKey, b Body) ([]byte, error) {
	env, err := signObject(status, KindDirectory, SignerStatus, b)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}
