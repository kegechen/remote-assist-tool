package client

import "testing"

// help 端只采纳与输入码仅差大小写的规范码：这是小写 join 能成立的前提
// （会话密钥必须用 relay 实际匹配上的码派生），也是防恶意 relay 偷换码的约束。
func TestAdoptCanonicalCode(t *testing.T) {
	h := &HelpMode{code: "abcd2345efgh"}
	h.adoptCanonicalCode("")
	if h.code != "abcd2345efgh" {
		t.Fatalf("relay 未回规范码（旧 relay）时保持输入码，得到 %q", h.code)
	}
	h.adoptCanonicalCode("ABCD2345EFGH")
	if h.code != "ABCD2345EFGH" {
		t.Fatalf("仅差大小写时应采纳规范码，得到 %q", h.code)
	}
	h.adoptCanonicalCode("ABCD2345EFGH")
	if h.code != "ABCD2345EFGH" {
		t.Fatalf("相同时应保持不变，得到 %q", h.code)
	}
	h2 := &HelpMode{code: "ABCD2345EFGH"}
	h2.adoptCanonicalCode("WXYZ23456789")
	if h2.code != "ABCD2345EFGH" {
		t.Fatalf("差异不止大小写时必须保持输入码（防偷换码），得到 %q", h2.code)
	}
}

// connect 的幂等键对码大小写不敏感：AI 先用小写 connect 成功（help 端已采纳规范
// 码），再拿 share 屏幕上的原样大写码重连，不应被当成新意图拆掉健康会话。存量码
// 集里不存在仅大小写不同的两个码，EqualFold 不会把不同码误判成同一意图。
func TestSameEndpointCodeCaseInsensitive(t *testing.T) {
	a := connectTarget{Code: "abcd2345efgh", Server: "1.2.3.4:8443"}
	b := connectTarget{Code: "ABCD2345EFGH", Server: "1.2.3.4:8443"}
	if !a.sameEndpoint(b) {
		t.Fatal("仅大小写不同的码应视为同一意图")
	}
	c := connectTarget{Code: "WXYZ23456789", Server: "1.2.3.4:8443"}
	if a.sameEndpoint(c) {
		t.Fatal("不同码仍须视为不同意图")
	}
	if (connectTarget{Code: "ABCD2345EFGH", Server: "1.2.3.4:8443"}).sameEndpoint(connectTarget{Code: "ABCD2345EFGH", Server: "5.6.7.8:8443"}) {
		t.Fatal("server 不同仍须视为不同意图")
	}
}
