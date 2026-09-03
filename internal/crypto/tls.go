package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// NewTLSConfig 创建安全的TLS配置（服务器端）
func NewTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load cert/key: %w", err)
	}

	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		CipherSuites: []uint16{
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},
	}

	return config, nil
}

// NewTLSClientConfig 创建客户端TLS配置
//
// skipVerify 与 caFile 互斥：crypto/tls 里 InsecureSkipVerify 的优先级高于 RootCAs，
// 两者同时给出时整条证书链与域名校验都会被跳过，caFile 变成毫无作用的装饰品。
// 与其静默吞掉调用方明确表达的校验意图，不如在这里 fail closed。调用方（cmd/remote）
// 已在 flag 层做过调和，这里是最后一道防线。
func NewTLSClientConfig(skipVerify bool, caFile string) (*tls.Config, error) {
	return NewClientTLSConfig(ClientTLSOptions{SkipVerify: skipVerify, CAFile: caFile})
}

// ClientTLSOptions 客户端 TLS 配置项。
type ClientTLSOptions struct {
	// SkipVerify 跳过 PKI 证书链与域名校验（对应 --insecure）。
	SkipVerify bool
	// CAFile 自定义根证书。与 SkipVerify 互斥，见 NewTLSClientConfig 的说明。
	CAFile string

	// PinAddr 非空且 SkipVerify 为真时启用 TOFU 指纹钉扎，用它作为指纹表的键
	// （即用户写的 host:port 原文）。SkipVerify 为假时 PKI 校验已经在做身份认证，
	// 再钉一层只会在证书轮换时平白制造摩擦，故不启用。
	//
	// 回环地址例外，见 isLoopbackAddr。
	PinAddr string
	// TrustNewCert 对应 --trust-new-cert：指纹变了也接受，并覆盖记录。
	TrustNewCert bool
	// TrustStore 指纹表。nil 时用 DefaultTrustStore()（~/.remote_assist_known_hosts）。
	TrustStore *TrustStore
	// OnPin 钉扎结果回调，供调用方打提示行。可为 nil。
	OnPin func(PinResult, string)
}

// NewClientTLSConfig 按 ClientTLSOptions 构造客户端 tls.Config。
func NewClientTLSConfig(o ClientTLSOptions) (*tls.Config, error) {
	if o.SkipVerify && o.CAFile != "" {
		return nil, fmt.Errorf("refusing to build TLS config: --ca %q would be silently ignored because certificate verification is disabled; drop --ca or pass --insecure=false", o.CAFile)
	}

	config := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: o.SkipVerify,
	}

	if o.CAFile != "" {
		caCert, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA cert: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert")
		}
		config.RootCAs = pool
	}

	if o.SkipVerify && o.PinAddr != "" && !isLoopbackAddr(o.PinAddr) {
		store := o.TrustStore
		if store == nil {
			store = DefaultTrustStore()
		}
		config.VerifyPeerCertificate = pinVerifier(o.PinAddr, store, o.TrustNewCert, o.OnPin)
	}

	return config, nil
}

// isLoopbackAddr 判断 host:port 里的 host 是不是本机回环。
//
// 回环连接不出网卡，没有中间人可防，钉扎在这里换不来任何安全性；反过来
// 127.0.0.1:8443 这种键会被本机上每一个 relay 共用（standalone、cmd/relay、各种
// 临时实例各有各的自签证书），钉上去只会不停地误报「指纹变了」。所以直接豁免。
//
// 只认字面量：域名要解析才知道指向哪，而构造 tls.Config 的时候还没连上。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// certRenewMargin 证书剩余有效期低于此值就重新生成，避免"刚好在会话中途过期"。
const certRenewMargin = 7 * 24 * time.Hour

// LoadOrCreateSelfSignedCert 复用 certFile/keyFile 上已有的自签证书；文件缺失、损坏
// 或临近过期时重新生成一张。返回值表示本次是否生成了新证书——新证书意味着指纹改变，
// 调用方通常需要据此提示用户。
//
// 存在的理由是 TOFU：钉扎要求同一 relay 地址的证书跨进程稳定，每次启动换一张新的会让
// 对端第二次连接必然撞上 ErrCertChanged。
func LoadOrCreateSelfSignedCert(certFile, keyFile string) (bool, error) {
	if certValid(certFile, keyFile) {
		return false, nil
	}
	if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
		return false, err
	}
	return true, nil
}

// certValid 现有的证书/私钥对是否可直接复用。任何一步不顺利都返回 false 交给重新生成，
// 不往上报错：这是"能省一次生成"的优化，不是校验。
func certValid(certFile, keyFile string) bool {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false
	}
	now := time.Now()
	return !now.Before(leaf.NotBefore) && now.Add(certRenewMargin).Before(leaf.NotAfter)
}

// GenerateSelfSignedCert 生成自签名证书（开发用）
func GenerateSelfSignedCert(certFile, keyFile string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "remote-assist",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return err
	}
	defer certOut.Close()
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer keyOut.Close()

	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})

	return nil
}
