package directory

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxRequestSize 是入网申请文件的大小上限。
const MaxRequestSize = 64 << 10

// 申请签名的域前缀，与列表各对象的签名域互不相同：节点的 TLS 私钥签出的东西绝不可能被当成列表里的任何签名对象。
// 新证书的签名与旧证书的共签也各用一个域，两者不能互换。
const (
	requestDomain     = "remote-assist-node-request-v1\x00"
	prevRequestDomain = "remote-assist-node-request-prev-v1\x00"
)

// NodeRequest 是节点运营者用 `relay node-request` 生成的入网申请。不含任何秘密：
// 证书本身是公开的，申请上的签名只证明申请人掌握这张证书的私钥。
type NodeRequest struct {
	Format   int      `json:"format"`
	Name     string   `json:"name"`
	Operator string   `json:"operator"`
	Region   string   `json:"region,omitempty"`
	TCP      []string `json:"tcp"`
	QUIC     []string `json:"quic,omitempty"`
	CertDER  []byte   `json:"cert_der"` // relay 的 TLS 叶子证书
	// PrevCertDER 只出现在换证书申请里：节点此刻还在出示的旧证书。申请必须同时由它的私钥共签，
	// 证明换证书的人就是旧证书的持有人——否则任何人都能照抄公开的节点信息，冒名把自己的证书挂到别人的前缀上。
	PrevCertDER []byte `json:"prev_cert_der,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// SignedNodeRequest 是申请文件的外壳：Request 是 NodeRequest 的 JSON，Sig 由 TLS 私钥签发，
// PrevSig 由旧证书私钥签发（仅换证书申请）。
type SignedNodeRequest struct {
	Request []byte `json:"request"`
	Sig     []byte `json:"sig"`
	PrevSig []byte `json:"prev_sig,omitempty"`
}

// VerifiedRequest 是验过签名的入网申请。
type VerifiedRequest struct {
	NodeRequest
	Fingerprint     string // 申请证书的指纹（同 crypto.CertFingerprint）
	PrevFingerprint string // 换证书申请里旧证书的指纹；普通申请为空
}

// CertFingerprint 返回证书 DER 的 sha256 小写 hex，与 internal/crypto.CertFingerprint 一致。
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// requestSigAlg 按证书公钥类型选签名算法。只接受这三类，与 relay 可能使用的证书一致。
func requestSigAlg(pub any) (x509.SignatureAlgorithm, crypto.Hash, error) {
	switch pub.(type) {
	case *ecdsa.PublicKey:
		return x509.ECDSAWithSHA256, crypto.SHA256, nil
	case *rsa.PublicKey:
		return x509.SHA256WithRSA, crypto.SHA256, nil
	case ed25519.PublicKey:
		return x509.PureEd25519, 0, nil
	default:
		return 0, 0, fmt.Errorf("%w: unsupported certificate key type %T", ErrMalformed, pub)
	}
}

// NewNodeRequest 用 relay 的 TLS 证书与私钥生成签名申请。r 里的 Format、CertDER、PrevCertDER 由本函数填写。
// 换证书时 prev 是节点此刻还在用的旧证书（含私钥），由它共签；普通申请传 nil。
func NewNodeRequest(cert tls.Certificate, prev *tls.Certificate, r NodeRequest) ([]byte, error) {
	if err := ValidateNodeInfo(r.Name, r.Operator, r.Region, r.TCP, r.QUIC); err != nil {
		return nil, err
	}
	r.Format = FormatVersion
	r.CertDER = nil
	r.PrevCertDER = nil
	if len(cert.Certificate) > 0 {
		r.CertDER = cert.Certificate[0]
	}
	if prev != nil && len(prev.Certificate) > 0 {
		r.PrevCertDER = prev.Certificate[0]
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	sig, err := signRequest(cert, requestDomain, payload)
	if err != nil {
		return nil, err
	}
	env := SignedNodeRequest{Request: payload, Sig: sig}
	if prev != nil {
		if env.PrevSig, err = signRequest(*prev, prevRequestDomain, payload); err != nil {
			return nil, fmt.Errorf("previous certificate: %w", err)
		}
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	// 用验证端同一套规则自检一遍，确保生成的申请一定能被 sign-node 接受。
	if _, err := VerifyNodeRequest(out); err != nil {
		return nil, err
	}
	return out, nil
}

// signRequest 用 cert 的私钥对 domain||payload 签名。
func signRequest(cert tls.Certificate, domain string, payload []byte) ([]byte, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("directory: no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("directory: parse certificate: %w", err)
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("directory: private key %T cannot sign", cert.PrivateKey)
	}
	// tls.LoadX509KeyPair 已经校验过配对，但调用方也可能手工拼 tls.Certificate。
	if pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(leaf.PublicKey) {
		return nil, errors.New("directory: private key does not match the certificate")
	}
	_, hash, err := requestSigAlg(leaf.PublicKey)
	if err != nil {
		return nil, err
	}
	msg := append([]byte(domain), payload...)
	digest := msg
	if hash != 0 {
		h := hash.New()
		h.Write(msg)
		digest = h.Sum(nil)
	}
	sig, err := signer.Sign(rand.Reader, digest, hash)
	if err != nil {
		return nil, fmt.Errorf("directory: sign request: %w", err)
	}
	return sig, nil
}

// checkRequestSig 校验 sig 是 der 证书的私钥对 domain||payload 的签名。
func checkRequestSig(der []byte, domain string, payload, sig []byte) error {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("%w: certificate: %v", ErrMalformed, err)
	}
	alg, _, err := requestSigAlg(leaf.PublicKey)
	if err != nil {
		return err
	}
	if err := leaf.CheckSignature(alg, append([]byte(domain), payload...), sig); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	return nil
}

// VerifyNodeRequest 校验申请的签名与内容。
// 它只证明「申请人掌握该证书私钥」（换证书申请还证明掌握旧证书私钥）；证书是否真的由申请里的地址出示，
// 要由 relay-admin 实际连接核对。
func VerifyNodeRequest(raw []byte) (*VerifiedRequest, error) {
	if len(raw) == 0 || len(raw) > MaxRequestSize {
		return nil, fmt.Errorf("%w: request size %d not in 1..%d", ErrMalformed, len(raw), MaxRequestSize)
	}
	var env SignedNodeRequest
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: request: %v", ErrMalformed, err)
	}
	var r NodeRequest
	if err := json.Unmarshal(env.Request, &r); err != nil {
		return nil, fmt.Errorf("%w: request body: %v", ErrMalformed, err)
	}
	if r.Format != FormatVersion {
		return nil, fmt.Errorf("%w: unsupported request format %d", ErrMalformed, r.Format)
	}
	if err := checkRequestSig(r.CertDER, requestDomain, env.Request, env.Sig); err != nil {
		return nil, fmt.Errorf("request not signed by the certificate key: %w", err)
	}
	v := &VerifiedRequest{NodeRequest: r, Fingerprint: CertFingerprint(r.CertDER)}
	switch {
	case len(r.PrevCertDER) > 0:
		if err := checkRequestSig(r.PrevCertDER, prevRequestDomain, env.Request, env.PrevSig); err != nil {
			return nil, fmt.Errorf("request not co-signed by the previous certificate key: %w", err)
		}
		v.PrevFingerprint = CertFingerprint(r.PrevCertDER)
		if v.PrevFingerprint == v.Fingerprint {
			return nil, fmt.Errorf("%w: previous certificate is the same as the new one", ErrMalformed)
		}
	case len(env.PrevSig) > 0:
		return nil, fmt.Errorf("%w: co-signature without a previous certificate", ErrMalformed)
	}
	if err := ValidateNodeInfo(r.Name, r.Operator, r.Region, r.TCP, r.QUIC); err != nil {
		return nil, err
	}
	return v, nil
}
