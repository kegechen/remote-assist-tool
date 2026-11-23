package directory_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	ratcrypto "github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/directory"
)

func selfSigned(t *testing.T, key crypto.Signer) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func requestInfo() directory.NodeRequest {
	return directory.NodeRequest{
		Name: "东京-1", Operator: "ops", Region: "jp",
		TCP: []string{"relay.example.test:8443"}, QUIC: []string{"relay.example.test:8443"},
		CreatedAt: 1_800_000_000,
	}
}

func TestNodeRequestRoundTrip(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)

	// relay 实际使用的自签证书（GenerateSelfSignedCert 生成的 PEM 文件）也要能用。
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := ratcrypto.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	relayPair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}

	for name, pair := range map[string]tls.Certificate{
		"ecdsa-p256": selfSigned(t, ecKey),
		"ecdsa-p384": selfSigned(t, ec384),
		"rsa":        selfSigned(t, rsaKey),
		"ed25519":    selfSigned(t, edKey),
		"relay pem":  relayPair,
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := directory.NewNodeRequest(pair, nil, requestInfo())
			if err != nil {
				t.Fatalf("NewNodeRequest: %v", err)
			}
			req, err := directory.VerifyNodeRequest(raw)
			if err != nil {
				t.Fatalf("VerifyNodeRequest: %v", err)
			}
			if req.Fingerprint != ratcrypto.CertFingerprint(pair.Certificate[0]) || req.PrevFingerprint != "" {
				t.Fatalf("fingerprints %s / %q, want crypto.CertFingerprint and no previous", req.Fingerprint, req.PrevFingerprint)
			}
			if req.Name != "东京-1" || req.Operator != "ops" || req.Format != directory.FormatVersion || len(req.TCP) != 1 {
				t.Fatalf("request = %+v", req)
			}
		})
	}
}

func TestNodeRequestRejectsForgeries(t *testing.T) {
	keyA, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyB, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pairA := selfSigned(t, keyA)
	raw, err := directory.NewNodeRequest(pairA, nil, requestInfo())
	if err != nil {
		t.Fatal(err)
	}
	var env directory.SignedNodeRequest
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var req directory.NodeRequest
	_ = json.Unmarshal(env.Request, &req)

	// tamper 改申请内容，但保留 A 原来的签名。
	tamper := func(mod func(*directory.NodeRequest)) []byte {
		r := req
		mod(&r)
		body, _ := json.Marshal(r)
		out, _ := json.Marshal(directory.SignedNodeRequest{Request: body, Sig: env.Sig})
		return out
	}
	unsigned, _ := json.Marshal(directory.SignedNodeRequest{Request: env.Request})

	cases := map[string][]byte{
		"operator changed":  tamper(func(r *directory.NodeRequest) { r.Operator = "someone-else" }),
		"address changed":   tamper(func(r *directory.NodeRequest) { r.TCP = []string{"evil.example.test:443"} }),
		"cert swapped":      tamper(func(r *directory.NodeRequest) { r.CertDER = selfSigned(t, keyB).Certificate[0] }),
		"signature dropped": unsigned,
		"garbage":           []byte("{"),
		"too large":         make([]byte, directory.MaxRequestSize+1),
	}
	// 证书是 A 的、私钥是 B 的：生成端就必须拒绝。
	mismatched := tls.Certificate{Certificate: pairA.Certificate, PrivateKey: keyB}
	if _, err := directory.NewNodeRequest(mismatched, nil, requestInfo()); err == nil {
		t.Error("NewNodeRequest accepted a private key that does not match the certificate")
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := directory.VerifyNodeRequest(raw); err == nil {
				t.Fatal("forged request verified")
			}
		})
	}
}

// 换证书申请：新证书签申请，旧证书私钥共签，证明换证书的就是旧证书的持有人。
func TestNodeRequestRenewal(t *testing.T) {
	oldKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, newKey, _ := ed25519.GenerateKey(rand.Reader) // 新旧证书的密钥类型可以不同
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	oldPair, newPair, otherPair := selfSigned(t, oldKey), selfSigned(t, newKey), selfSigned(t, otherKey)

	raw, err := directory.NewNodeRequest(newPair, &oldPair, requestInfo())
	if err != nil {
		t.Fatalf("NewNodeRequest: %v", err)
	}
	req, err := directory.VerifyNodeRequest(raw)
	if err != nil {
		t.Fatalf("VerifyNodeRequest: %v", err)
	}
	if req.Fingerprint != directory.CertFingerprint(newPair.Certificate[0]) || req.PrevFingerprint != directory.CertFingerprint(oldPair.Certificate[0]) {
		t.Fatalf("fingerprints %s / %s", req.Fingerprint, req.PrevFingerprint)
	}

	var env directory.SignedNodeRequest
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	plainRaw, _ := directory.NewNodeRequest(newPair, nil, requestInfo())
	var plain directory.SignedNodeRequest
	_ = json.Unmarshal(plainRaw, &plain)
	// 冒名者掌握新证书私钥（主签名照样有效），但拿不到旧证书私钥。
	// 这里手工签共签，签名格式与域前缀照抄实现，也顺带把格式钉死。
	const mainDomain, prevDomain = "remote-assist-node-request-v1\x00", "remote-assist-node-request-prev-v1\x00"
	ecSign := func(key *ecdsa.PrivateKey, domain string) []byte {
		sum := sha256.Sum256(append([]byte(domain), env.Request...))
		sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	enc := func(e directory.SignedNodeRequest) []byte { b, _ := json.Marshal(e); return b }
	withPrevSig := func(sig []byte) []byte {
		return enc(directory.SignedNodeRequest{Request: env.Request, Sig: env.Sig, PrevSig: sig})
	}
	// 对照：用旧私钥按正确的域手工共签，必须通过——证明下面的反例失败只因为共签本身不对。
	if _, err := directory.VerifyNodeRequest(withPrevSig(ecSign(oldKey, prevDomain))); err != nil {
		t.Fatalf("hand-made valid co-signature rejected: %v", err)
	}

	for name, raw := range map[string][]byte{
		"co-signature dropped":                 withPrevSig(nil),
		"co-signature without previous cert":   enc(directory.SignedNodeRequest{Request: plain.Request, Sig: plain.Sig, PrevSig: env.PrevSig}),
		"main signature reused as co-sig":      withPrevSig(env.Sig),
		"co-signed by a different key":         withPrevSig(ecSign(otherKey, prevDomain)),
		"old key signed under the main domain": withPrevSig(ecSign(oldKey, mainDomain)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := directory.VerifyNodeRequest(raw); err == nil {
				t.Fatal("forged renewal request verified")
			}
		})
	}
	if _, err := directory.NewNodeRequest(newPair, &newPair, requestInfo()); err == nil {
		t.Error("renewal with the same certificate as previous accepted")
	}
	mismatched := tls.Certificate{Certificate: oldPair.Certificate, PrivateKey: otherPair.PrivateKey}
	if _, err := directory.NewNodeRequest(newPair, &mismatched, requestInfo()); err == nil {
		t.Error("co-signing with a key that does not match the previous certificate accepted")
	}
}

func TestNodeRequestValidatesInfo(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pair := selfSigned(t, key)
	bad := requestInfo()
	bad.Name = "evil\x1b[2J"
	if _, err := directory.NewNodeRequest(pair, nil, bad); !errors.Is(err, directory.ErrMalformed) {
		t.Fatalf("NewNodeRequest with escape in name = %v, want ErrMalformed", err)
	}
	bad = requestInfo()
	bad.TCP = nil
	if _, err := directory.NewNodeRequest(pair, nil, bad); !errors.Is(err, directory.ErrMalformed) {
		t.Fatalf("NewNodeRequest without tcp = %v, want ErrMalformed", err)
	}
	if _, err := directory.NewNodeRequest(tls.Certificate{}, nil, requestInfo()); err == nil {
		t.Fatal("NewNodeRequest without certificate succeeded")
	}
}
