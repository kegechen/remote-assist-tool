package crypto

import (
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
)

// 严格指纹比对：节点列表（签名内容）里每个 relay 节点都带着它的 TLS 证书指纹，连这些节点时出示的证书
// 必须是其中之一。身份由根签名的列表认定，不靠 CA 链、也不是 TOFU：没有「首次信任」，不往指纹表里写，
// 回环地址也不豁免（回环豁免是因为 TOFU 的键会被本机上不同的 relay 共用，这里没有键）。

// ErrCertNotAllowed 表示 relay 出示的证书指纹不在允许的列表里。
var ErrCertNotAllowed = errors.New("relay certificate fingerprint is not in the allowed list")

// fpVerifier 构造 tls.Config.VerifyPeerCertificate 回调：只看叶子证书（rawCerts[0]）的指纹，
// 在 allowed 里（不分大小写）才放行。不在列表里时先调 onMismatch（可为 nil）再报 ErrCertNotAllowed。
//
// 回调是传输无关的：TCP（crypto/tls）与 QUIC（quic-go）都会调用它，但 QUIC 把握手错误转成文本的
// 传输层错误，errors.Is 认不出 ErrCertNotAllowed——调用方要可靠地知道「是证书不符」就用 onMismatch。
func fpVerifier(allowed []string, onMismatch func(fp string)) func([][]byte, [][]*x509.Certificate) error {
	want := fpSet(allowed)
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("relay 没有出示任何证书")
		}
		fp := CertFingerprint(rawCerts[0])
		if _, ok := want[fp]; ok {
			return nil
		}
		if onMismatch != nil {
			onMismatch(fp)
		}
		short := fp
		if len(short) > 16 {
			short = short[:16]
		}
		return fmt.Errorf("%w（出示的指纹 %s…）", ErrCertNotAllowed, short)
	}
}

// fpSet 把指纹列表转成按小写查找的集合。空串留在里面也无妨：算出来的指纹不会是空串。
func fpSet(fps []string) map[string]struct{} {
	set := make(map[string]struct{}, len(fps))
	for _, a := range fps {
		set[strings.ToLower(a)] = struct{}{}
	}
	return set
}

// listedOrPin 构造 VerifyPeerCertificate 回调：叶子证书的指纹在 listed 里（不分大小写）就放行，否则交给 pin（TOFU）。
// 见 ClientTLSOptions.ListedFPs。
func listedOrPin(listed []string, pin func([][]byte, [][]*x509.Certificate) error) func([][]byte, [][]*x509.Certificate) error {
	want := fpSet(listed)
	return func(rawCerts [][]byte, chains [][]*x509.Certificate) error {
		if len(rawCerts) > 0 {
			if _, ok := want[CertFingerprint(rawCerts[0])]; ok {
				return nil
			}
		}
		return pin(rawCerts, chains)
	}
}
