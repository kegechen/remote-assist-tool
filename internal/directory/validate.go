package directory

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidPrefix 报告 s 是否是合法的节点前缀：恰好一个协助码字符。
func ValidPrefix(s string) bool {
	return len(s) == 1 && strings.IndexByte(CodeCharset, s[0]) >= 0
}

// validText 校验会显示给用户的文本（名称、运营者、地区）。这些字符串来自签名对象，
// 但内容出自第三方运营者提交的申请，所以仍要挡掉控制字符和不可见字符：
// 否则 "\x1b]0;..." 之类的终端转义序列会经「选节点」日志直接打到用户终端上。
func validText(field, s string, required bool) error {
	if s == "" {
		if required {
			return fmt.Errorf("%w: %s is empty", ErrMalformed, field)
		}
		return nil
	}
	if len(s) > MaxFieldLen || !utf8.ValidString(s) {
		return fmt.Errorf("%w: %s too long or not UTF-8", ErrMalformed, field)
	}
	for _, r := range s {
		if !visibleRune(r) {
			return fmt.Errorf("%w: %s contains control, invisible or space characters", ErrMalformed, field)
		}
	}
	return nil
}

// blankRunes 是 L/N/P/S 类里显示成空白的字符：韩文填充符、盲文空白、对象替换符、埃及象形文字空白、
// 表意半填充、各文字的 gap filler、空音符头。
var blankRunes = []rune{
	0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800, 0xFFFC, utf8.RuneError,
	0x13441, 0x13442, 0x303F, 0xA8F9, 0x1144E, 0x11945, 0x11C44, 0x11C45, 0x11F48, 0x1D159,
}

// mathAlphanumeric 是「数学字母数字符号」区块：粗体、等宽等样式的拉丁字母和数字，显示出来就是普通 ASCII，
// 能把「u-1」仿冒成另一个节点名。
var mathAlphanumeric = unicode.RangeTable{R32: []unicode.Range32{{Lo: 0x1D400, Hi: 0x1D7FF, Stride: 1}}}

// visibleRune 只放行字母、数字、标点、符号（Unicode 的 L/N/P/S 类），再排除其中显示成空白或仿冒 ASCII 的。
// 用白名单而不是黑名单：控制、格式（零宽、双向控制）、空白、组合附加符、变体选择符、私用区、未分配码位
// 都不在这四类里，不必逐个列举。目的是不让人用看不见、叠加或长得一样的字符把两个节点名伪装成一样。
// 代价是需要组合附加符的文字（如泰文、天城文）写不了，节点名用拉丁字母即可。
func visibleRune(r rune) bool {
	return unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S) && !slices.Contains(blankRunes, r) &&
		!unicode.Is(&mathAlphanumeric, r)
}

// ValidAddr 校验 host:port。relay 是裸 TCP/TLS，不接受 URL、路径、空白与控制字符。
func ValidAddr(addr string) error {
	if addr == "" || len(addr) > 255 {
		return fmt.Errorf("%w: address empty or too long", ErrMalformed)
	}
	for _, r := range addr {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%w: address %q has non-printable or non-ASCII characters", ErrMalformed, addr)
		}
	}
	if strings.ContainsAny(addr, "/?#@\\") {
		return fmt.Errorf("%w: address %q must be host:port", ErrMalformed, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("%w: address %q must be host:port", ErrMalformed, addr)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || port != strconv.Itoa(p) {
		return fmt.Errorf("%w: address %q has a bad port", ErrMalformed, addr)
	}
	return nil
}

func validAddrs(field string, addrs []string, required bool) error {
	if len(addrs) == 0 {
		if required {
			return fmt.Errorf("%w: %s must list at least one address", ErrMalformed, field)
		}
		return nil
	}
	if len(addrs) > MaxAddrsPerKind {
		return fmt.Errorf("%w: %s lists more than %d addresses", ErrMalformed, field, MaxAddrsPerKind)
	}
	seen := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		if err := ValidAddr(a); err != nil {
			return err
		}
		if seen[a] {
			return fmt.Errorf("%w: %s lists %q twice", ErrMalformed, field, a)
		}
		seen[a] = true
	}
	return nil
}

// ValidFingerprint 报告 s 是否是 sha256 的小写 hex（64 字符）。
func ValidFingerprint(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidateNodeInfo 校验节点的展示与地址信息，签发端（relay-admin、node-request）与验证端共用。
func ValidateNodeInfo(name, operator, region string, tcp, quic []string) error {
	if err := validText("name", name, true); err != nil {
		return err
	}
	if err := validText("operator", operator, true); err != nil {
		return err
	}
	if err := validText("region", region, false); err != nil {
		return err
	}
	if err := validAddrs("tcp", tcp, true); err != nil {
		return err
	}
	return validAddrs("quic", quic, false)
}

// ValidateNodeCert 校验证书内容本身（不含签名与有效期是否覆盖当前时间）。
func ValidateNodeCert(c *NodeCert) error {
	if c.Serial == 0 {
		return fmt.Errorf("%w: serial must be > 0", ErrMalformed)
	}
	if !ValidPrefix(c.Prefix) {
		return fmt.Errorf("%w: prefix %q is not a single code character", ErrMalformed, c.Prefix)
	}
	if err := ValidateNodeInfo(c.Name, c.Operator, c.Region, c.TCP, c.QUIC); err != nil {
		return err
	}
	if n := len(c.TLSFPs); n == 0 || n > MaxTLSFPs {
		return fmt.Errorf("%w: tls_fps must list 1..%d fingerprints", ErrMalformed, MaxTLSFPs)
	}
	for i, fp := range c.TLSFPs {
		if !ValidFingerprint(fp) {
			return fmt.Errorf("%w: tls fingerprint %q is not 64 lowercase hex", ErrMalformed, fp)
		}
		for _, prev := range c.TLSFPs[:i] {
			if prev == fp {
				return fmt.Errorf("%w: tls fingerprint %q listed twice", ErrMalformed, fp)
			}
		}
	}
	if c.NotAfter <= c.NotBefore {
		return fmt.Errorf("%w: not_after must be after not_before", ErrMalformed)
	}
	return nil
}
