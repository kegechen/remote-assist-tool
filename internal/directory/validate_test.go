package directory_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/directory"
)

func TestValidPrefix(t *testing.T) {
	for _, s := range []string{"A", "Z", "2", "9"} {
		if !directory.ValidPrefix(s) {
			t.Errorf("ValidPrefix(%q) = false", s)
		}
	}
	// I、L、O、0、1 不在协助码字符集里；小写由调用方先转大写。
	for _, s := range []string{"", "I", "L", "O", "0", "1", "a", "AB", "-", "\x00"} {
		if directory.ValidPrefix(s) {
			t.Errorf("ValidPrefix(%q) = true", s)
		}
	}
}

func TestValidateNodeInfoText(t *testing.T) {
	addrs := []string{"relay.example.test:8443"}
	good := []string{
		"东京-1", "relay_b", "Operator.Name", strings.Repeat("x", directory.MaxFieldLen),
		"São-Paulo", "서울-1", "とうきょう", "Zürich(2)", "Ops+Team#1", "★relay", // 预组合字母、韩文、标点、符号照常可用
	}
	for _, s := range good {
		if err := directory.ValidateNodeInfo(s, s, s, addrs, nil); err != nil {
			t.Errorf("ValidateNodeInfo(%q): %v", s, err)
		}
	}
	bad := map[string]string{
		"empty":            "",
		"too long":         strings.Repeat("x", directory.MaxFieldLen+1),
		"osc escape":       "a\x1b]0;title\x07",
		"csi escape":       "a\x1b[2J",
		"c1 control":       "a\u009bb",
		"newline":          "a\nb",
		"ascii space":      "a b",
		"nbsp":             "a\u00a0b",
		"ideographic":      "a\u3000b",
		"zero width":       "a\u200bb",
		"bidi override":    "a\u202eb",
		"bom":              "\ufeffa",
		"invalid utf8":     "a\xffb",
		"replacement rune": "a\ufffdb",
		// \u4e0b\u9762\u8fd9\u4e9b\u4e0d\u5c5e\u4e8e\u63a7\u5236\u3001\u683c\u5f0f\u6216\u7a7a\u767d\u7c7b\uff0c\u5374\u663e\u793a\u6210\u7a7a\u767d\u6216\u53e0\u5728\u524d\u4e00\u4e2a\u5b57\u7b26\u4e0a\uff0c\u540c\u6837\u80fd\u628a\u4e24\u4e2a\u540d\u5b57\u4f2a\u88c5\u6210\u4e00\u6837\u3002
		"hangul filler":           "a\u3164b",
		"hangul choseong filler":  "a\u115fb",
		"hangul jungseong filler": "a\u1160b",
		"halfwidth hangul filler": "a\uffa0b",
		"braille blank":           "a\u2800b",
		"object replacement":      "a\ufffcb",
		"mongolian fvs":           "a\u180bb",
		"variation selector":      "a\ufe0fb",
		"combining acute":         "a\u0301b",
		"combining grapheme join": "a\u034fb",
		"private use":             "a\ue000b",
		"unassigned":              "a\U000e0080b",
		// \u540c\u6837\u5c5e\u4e8e L/N/P/S\u3001\u663e\u793a\u6210\u7a7a\u767d\u7684\uff1a\u57c3\u53ca\u8c61\u5f62\u6587\u5b57\u7a7a\u767d\u3001\u8868\u610f\u534a\u586b\u5145\u3001\u5404\u6587\u5b57\u7684 gap filler\u3001\u7a7a\u97f3\u7b26\u5934\u3002
		"egyptian full blank":   "a\U00013441b",
		"egyptian half blank":   "a\U00013442b",
		"ideographic half fill": "a\u303fb",
		"devanagari gap filler": "a\ua8f9b",
		"newa gap filler":       "a\U0001144eb",
		"dives akuru gap":       "a\U00011945b",
		"bhaiksuki gap filler":  "a\U00011c44b",
		"bhaiksuki gap 2":       "a\U00011c45b",
		"kawi gap filler":       "a\U00011f48b",
		"null notehead":         "a\U0001d159b",
		// \u6570\u5b66\u5b57\u6bcd\u6570\u5b57\u7b26\u53f7\u663e\u793a\u6210\u666e\u901a ASCII\uff0c\u53ef\u4ee5\u628a\u300cu-1\u300d\u4eff\u5192\u6210\u53e6\u4e00\u4e2a\u8282\u70b9\u540d\u3002
		"math monospace":  "\U0001d69e-1",
		"math bold":       "\U0001d400",
		"math last digit": "\U0001d7ff",
	}
	for name, s := range bad {
		if err := directory.ValidateNodeInfo(s, "ops", "", addrs, nil); !errors.Is(err, directory.ErrMalformed) {
			t.Errorf("name %s (%q): err = %v, want ErrMalformed", name, s, err)
		}
	}
	if err := directory.ValidateNodeInfo("n", "ops", "a\x1b", addrs, nil); !errors.Is(err, directory.ErrMalformed) {
		t.Errorf("region with escape accepted: %v", err)
	}
}

func TestValidAddr(t *testing.T) {
	for _, a := range []string{"relay.example.test:8443", "192.0.2.10:443", "[2001:db8::1]:8443", "h:1", "h:65535"} {
		if err := directory.ValidAddr(a); err != nil {
			t.Errorf("ValidAddr(%q): %v", a, err)
		}
	}
	for _, a := range []string{
		"", "relay.example.test", ":8443", "h:0", "h:65536", "h:08443", "h:-1", "h:+1", "h:port",
		"https://h:8443", "h:8443/path", "user@h:8443", "h :8443", "h:8443\n", "h\t:8443", "ä.example:8443",
		"h:8443?x", "h:8443#x", `h\x:8443`, "2001:db8::1:8443", strings.Repeat("h", 251) + ":8443", // 256 字节，超过 255 上限
	} {
		if err := directory.ValidAddr(a); !errors.Is(err, directory.ErrMalformed) {
			t.Errorf("ValidAddr(%q) = %v, want ErrMalformed", a, err)
		}
	}
}

func TestValidateNodeInfoAddrLists(t *testing.T) {
	if err := directory.ValidateNodeInfo("n", "o", "", nil, nil); !errors.Is(err, directory.ErrMalformed) {
		t.Errorf("no tcp address accepted: %v", err)
	}
	if err := directory.ValidateNodeInfo("n", "o", "", []string{"h:1", "h:1"}, nil); !errors.Is(err, directory.ErrMalformed) {
		t.Errorf("duplicate address accepted: %v", err)
	}
	five := []string{"h:1", "h:2", "h:3", "h:4", "h:5"}
	if err := directory.ValidateNodeInfo("n", "o", "", []string{"h:1"}, five); !errors.Is(err, directory.ErrMalformed) {
		t.Errorf("too many quic addresses accepted: %v", err)
	}
}

func TestValidateNodeCertFingerprints(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	base := func() directory.NodeCert {
		return directory.NodeCert{Serial: 1, Prefix: "A", Name: "n", Operator: "o", TCP: []string{"h:1"}, TLSFPs: []string{fp}, NotBefore: 1, NotAfter: 2}
	}
	c := base()
	if err := directory.ValidateNodeCert(&c); err != nil {
		t.Fatalf("base cert: %v", err)
	}
	mods := map[string]func(*directory.NodeCert){
		"no fingerprint":        func(c *directory.NodeCert) { c.TLSFPs = nil },
		"uppercase fingerprint": func(c *directory.NodeCert) { c.TLSFPs = []string{strings.ToUpper(fp)} },
		"short fingerprint":     func(c *directory.NodeCert) { c.TLSFPs = []string{fp[:62]} },
		"duplicate fingerprint": func(c *directory.NodeCert) { c.TLSFPs = []string{fp, fp} },
		"three fingerprints": func(c *directory.NodeCert) {
			c.TLSFPs = []string{fp, strings.Repeat("cd", 32), strings.Repeat("ef", 32)}
		},
		"zero serial":       func(c *directory.NodeCert) { c.Serial = 0 },
		"bad prefix":        func(c *directory.NodeCert) { c.Prefix = "I" },
		"inverted validity": func(c *directory.NodeCert) { c.NotAfter = c.NotBefore },
	}
	for name, mod := range mods {
		c := base()
		mod(&c)
		if err := directory.ValidateNodeCert(&c); !errors.Is(err, directory.ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}
