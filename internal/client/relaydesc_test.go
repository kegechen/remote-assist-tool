package client

import "testing"

// relayDesc 是排查连接问题时的第一手信息（连的哪台 relay、走 TLS 还是明文），
// 三种传输方式都要能一眼看清，别让人再去猜。
func TestRelayDesc(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"明文", Config{ServerAddr: "127.0.0.1:8443"}, "127.0.0.1:8443 (明文)"},
		{"TLS 校验证书", Config{ServerAddr: "relay.example:8443", UseTLS: true}, "relay.example:8443 (TLS)"},
		{"TLS 跳过校验", Config{ServerAddr: "23.95.78.14:8443", UseTLS: true, InsecureSkip: true}, "23.95.78.14:8443 (TLS，跳过证书校验)"},
		// InsecureSkip 只在 TLS 下有意义：明文时不该冒出“跳过证书校验”误导人
		{"明文时忽略 InsecureSkip", Config{ServerAddr: "h:1", InsecureSkip: true}, "h:1 (明文)"},
	}
	for _, c := range cases {
		if got := relayDesc(&c.cfg); got != c.want {
			t.Errorf("%s: relayDesc = %q，想要 %q", c.name, got, c.want)
		}
	}
}
