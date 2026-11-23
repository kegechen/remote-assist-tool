// Package dirtest 提供节点列表相关测试的夹具：确定性的根/状态密钥，以及一键生成各种合法/非法列表的辅助函数。
// 只给测试用；放在独立包里是为了让 relay、client 等包的测试也能复用。
package dirtest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

// Epoch 是夹具的固定「现在」，避免测试依赖真实时钟。
var Epoch = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

// Fixture 持有一套根/状态密钥与一份可用的状态密钥授权。
type Fixture struct {
	T          testing.TB
	Primary    ed25519.PrivateKey
	Backup     ed25519.PrivateKey
	Status     ed25519.PrivateKey
	Roots      directory.Roots
	StatusCert directory.Envelope
	Now        time.Time
	nextSerial uint64
}

// SeedKey 由名字确定性地派生一把 ed25519 私钥（测试向量需要可复现）。
func SeedKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("dirtest-key:" + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

// New 创建夹具：状态密钥授权序号 1，有效期覆盖 Epoch 前后一年。
func New(t testing.TB) *Fixture {
	t.Helper()
	f := &Fixture{
		T:          t,
		Primary:    SeedKey("primary"),
		Backup:     SeedKey("backup"),
		Status:     SeedKey("status"),
		Now:        Epoch,
		nextSerial: 100,
	}
	f.Roots = directory.Roots{Keys: map[string]ed25519.PublicKey{
		directory.SignerPrimary: f.Primary.Public().(ed25519.PublicKey),
		directory.SignerBackup:  f.Backup.Public().(ed25519.PublicKey),
	}}
	f.StatusCert = f.IssueStatus(1, f.Status, f.Primary, directory.SignerPrimary)
	return f
}

// IssueStatus 用 root 授权 pub 对应的状态密钥。
func (f *Fixture) IssueStatus(serial uint64, statusPriv, root ed25519.PrivateKey, signer string) directory.Envelope {
	f.T.Helper()
	env, err := directory.IssueStatusKey(root, signer, directory.StatusKeyCert{
		Serial:    serial,
		PubKey:    statusPriv.Public().(ed25519.PublicKey),
		NotBefore: f.Now.Add(-24 * time.Hour).Unix(),
		NotAfter:  f.Now.Add(365 * 24 * time.Hour).Unix(),
	})
	if err != nil {
		f.T.Fatalf("IssueStatus: %v", err)
	}
	return env
}

// Fingerprint 返回由 seed 派生的、格式合法的假 TLS 指纹。
func Fingerprint(seed string) string {
	sum := sha256.Sum256([]byte("dirtest-fp:" + seed))
	const hex = "0123456789abcdef"
	var b strings.Builder
	for _, c := range sum {
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// Cert 返回一张合法的节点证书内容（有效期覆盖 Epoch 前后一年），mod 可改任意字段。
func (f *Fixture) Cert(prefix string, mod func(*directory.NodeCert)) directory.NodeCert {
	f.nextSerial++
	c := directory.NodeCert{
		Serial:    f.nextSerial,
		Prefix:    prefix,
		Name:      "node-" + prefix,
		Operator:  "ops",
		Official:  true,
		Region:    "test",
		TCP:       []string{"relay-" + strings.ToLower(prefix) + ".example.test:8443"},
		QUIC:      []string{"relay-" + strings.ToLower(prefix) + ".example.test:8443"},
		TLSFPs:    []string{Fingerprint(prefix)},
		NotBefore: f.Now.Add(-24 * time.Hour).Unix(),
		NotAfter:  f.Now.Add(365 * 24 * time.Hour).Unix(),
	}
	if mod != nil {
		mod(&c)
	}
	return c
}

// SignCert 用 primary 根签发（绕过 IssueNodeCert 的内容校验，以便构造非法证书）。
func (f *Fixture) SignCert(c directory.NodeCert) directory.Envelope {
	return f.SignCertWith(c, f.Primary, directory.SignerPrimary)
}

// SignCertWith 用指定根签发，不做内容校验。
func (f *Fixture) SignCertWith(c directory.NodeCert, root ed25519.PrivateKey, signer string) directory.Envelope {
	f.T.Helper()
	payload := mustJSON(f.T, c)
	return directory.Sign(root, directory.KindNodeCert, signer, payload)
}

// Entry 生成一项 active、权重 100 的列表项。
func (f *Fixture) Entry(prefix string) directory.NodeEntry {
	return directory.NodeEntry{Cert: f.SignCert(f.Cert(prefix, nil)), Status: directory.StatusActive, Weight: 100}
}

// Body 返回一份合法列表正文：序号 42，有效期 30 天，默认节点是第一项。
func (f *Fixture) Body(nodes ...directory.NodeEntry) directory.Body {
	def := ""
	if len(nodes) > 0 {
		var c directory.NodeCert
		mustUnmarshal(f.T, nodes[0].Cert.Payload, &c)
		def = c.Prefix
	}
	return directory.Body{
		Format:        directory.FormatVersion,
		Serial:        42,
		IssuedAt:      f.Now.Add(-time.Hour).Unix(),
		ExpiresAt:     f.Now.Add(30 * 24 * time.Hour).Unix(),
		Policy:        directory.PolicyPreferDefault,
		DefaultPrefix: def,
		StatusKey:     f.StatusCert,
		Nodes:         nodes,
	}
}

// Sign 用状态密钥签 b。
func (f *Fixture) Sign(b directory.Body) []byte {
	f.T.Helper()
	raw, err := directory.SignDirectory(f.Status, b)
	if err != nil {
		f.T.Fatalf("SignDirectory: %v", err)
	}
	return raw
}

// RootFP 返回夹具根公钥的指纹；name 是 SignerPrimary 或 SignerBackup。
func (f *Fixture) RootFP(name string) string {
	f.T.Helper()
	pub, ok := f.Roots.Keys[name]
	if !ok {
		f.T.Fatalf("RootFP: no root named %q", name)
	}
	return directory.RootFingerprint(pub)
}

// Revocations 用 root 签发吊销列表。roots 里写 SignerPrimary / SignerBackup 时换成夹具对应根公钥的指纹，
// 其它字符串原样写入（用来构造非法输入）。
func (f *Fixture) Revocations(serial uint64, nodes []uint64, roots []string, root ed25519.PrivateKey, signer string) *directory.Envelope {
	f.T.Helper()
	var fps []string
	for _, r := range roots {
		if r == directory.SignerPrimary || r == directory.SignerBackup {
			r = f.RootFP(r)
		}
		fps = append(fps, r)
	}
	return f.SignRevocations(directory.Revocations{Serial: serial, NodeSerials: nodes, Roots: fps}, root, signer)
}

// SignRevocations 用 root 直接签 r（IssuedAt 为 0 时填夹具时间），不走 IssueRevocations 的检查，以便构造非法输入。
func (f *Fixture) SignRevocations(r directory.Revocations, root ed25519.PrivateKey, signer string) *directory.Envelope {
	f.T.Helper()
	if r.IssuedAt == 0 {
		r.IssuedAt = f.Now.Unix()
	}
	env := directory.Sign(root, directory.KindRevocations, signer, mustJSON(f.T, r))
	return &env
}
