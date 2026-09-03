package crypto

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 证书指纹钉扎（TOFU，trust on first use）
//
// 为什么需要：`--insecure` 默认是 true，而这个默认值改不掉——relay 在没有 `-cert/-key`
// 时自动生成自签证书，SAN 只有 localhost/127.0.0.1，客户端默认连的却是裸 IP。翻转默认
// 值会因「无可信链」+「SAN 不匹配」两个独立原因导致校验必然失败，所有人开箱即坏。
//
// 于是「跳过校验」是常态，中间人因此永远可行且完全无痕：协助码由 relay 明文下发
// （RegisterResponse.Code）、又明文上行（JoinRequest.Code），MITM 拿到 code 就能用
// DeriveSessionKey(code, nonceShare, nonceHelp) 复算出工具通道的 AEAD 密钥——`--insecure`
// 帮助文本里「security then relies on tool-channel AEAD」的兜底论断并不成立。
//
// TOFU 不解决首次连接，但把攻击窗口从「永远」压缩到「只有首次」：首次记下证书指纹，
// 之后指纹变了就拒绝连接。使用成本接近零——不需要 CA，不需要任何配置。
//
// 代价（已知且必须接受）：relay 的 certs/ 在 .gitignore 里，重装或删除该目录会重新生成
// 证书，届时所有老客户端都会拒连并需要 `--trust-new-cert` 重新钉一次。

// KnownHostsFileName 指纹表文件名，放在用户主目录，与 clientIDFileName 同一套约定。
const KnownHostsFileName = ".remote_assist_known_hosts"

// ErrCertChanged 已记录的指纹与本次握手对不上。单独成类型，便于调用方识别并给出提示。
var ErrCertChanged = errors.New("relay certificate fingerprint changed")

// CertFingerprint 返回证书 DER 的 SHA-256，小写 hex。
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// TrustStore 是 known_hosts 风格的指纹表：每行 `<addr> <sha256-hex>`，`#` 开头为注释。
// Path 为空表示「拿不到可写位置」，此时 Lookup 永远返回未记录、Put 是空操作——
// 钉扎降级为不生效，但绝不因此拒绝连接。
type TrustStore struct {
	Path string

	mu sync.Mutex // 串行化同进程内的 read-modify-write
}

// DefaultTrustStore 返回 ~/.remote_assist_known_hosts。拿不到主目录时返回 Path 为空的
// store（钉扎降级为不生效），而不是让连接失败。
func DefaultTrustStore() *TrustStore {
	home, err := os.UserHomeDir()
	if err != nil {
		return &TrustStore{}
	}
	return &TrustStore{Path: filepath.Join(home, KnownHostsFileName)}
}

// Lookup 查 addr 已记录的指纹。第二个返回值表示是否有记录。
func (s *TrustStore) Lookup(addr string) (string, bool) {
	if s == nil || s.Path == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fp, ok := s.readLocked()[addr]
	return fp, ok
}

// Put 记录（或覆盖）addr 的指纹。整表重写，行数很少，不值得为增量写引入复杂度。
func (s *TrustStore) Put(addr, fp string) error {
	if s == nil || s.Path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.readLocked()
	entries[addr] = fp

	addrs := make([]string, 0, len(entries))
	for a := range entries {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs) // 稳定顺序：文件是给人看的，也方便 diff

	var b strings.Builder
	b.WriteString("# remote-assist 已信任的 relay 证书指纹（TOFU）。\n")
	b.WriteString("# 删掉某一行即可让下次连接重新学习该地址的证书。\n")
	for _, a := range addrs {
		fmt.Fprintf(&b, "%s %s\n", a, entries[a])
	}
	if dir := filepath.Dir(s.Path); dir != "" {
		os.MkdirAll(dir, 0700)
	}
	return os.WriteFile(s.Path, []byte(b.String()), 0600)
}

// readLocked 读取指纹表。文件不存在或某行格式不对都按「没有该记录」处理：指纹表是
// 缓存不是配置，为一行手抖而拒绝所有连接不合算。
func (s *TrustStore) readLocked() map[string]string {
	entries := make(map[string]string)
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return entries
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		entries[fields[0]] = strings.ToLower(fields[1])
	}
	return entries
}

// PinResult 一次钉扎校验的结果，供调用方决定要不要打提示行。
type PinResult int

const (
	// PinMatched 指纹与记录一致，静默放行。
	PinMatched PinResult = iota
	// PinLearned 首次见到该地址，已记录。
	PinLearned
	// PinReplaced 指纹变了，但调用方带了 --trust-new-cert，已覆盖记录。
	PinReplaced
)

// pinVerifier 构造 tls.Config.VerifyPeerCertificate 回调。
//
// InsecureSkipVerify=true 时 crypto/tls 仍会调用这个回调并传入 rawCerts（只是 verifiedChains
// 为空），所以它是「跳过 PKI 校验」前提下唯一能插手的检查点。
func pinVerifier(addr string, store *TrustStore, trustNew bool, onResult func(PinResult, string)) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("relay 没有出示任何证书")
		}
		fp := CertFingerprint(rawCerts[0])
		known, ok := store.Lookup(addr)
		switch {
		case !ok:
			if err := store.Put(addr, fp); err != nil {
				return fmt.Errorf("记录 relay 证书指纹失败: %w", err)
			}
			if onResult != nil {
				onResult(PinLearned, fp)
			}
		case known == fp:
			if onResult != nil {
				onResult(PinMatched, fp)
			}
		case trustNew:
			if err := store.Put(addr, fp); err != nil {
				return fmt.Errorf("更新 relay 证书指纹失败: %w", err)
			}
			if onResult != nil {
				onResult(PinReplaced, fp)
			}
		default:
			return fmt.Errorf("%w: %s 的证书指纹与首次连接时不一致\n  已记录: %s\n  本次:   %s\n"+
				"可能是 relay 重装/换证书，也可能是中间人。确认是前者后用 --trust-new-cert 重新信任，"+
				"或删除 %s 里对应的那一行。",
				ErrCertChanged, addr, known, fp, storePathForMsg(store))
		}
		return nil
	}
}

func storePathForMsg(s *TrustStore) string {
	if s == nil || s.Path == "" {
		return "指纹表"
	}
	return s.Path
}
