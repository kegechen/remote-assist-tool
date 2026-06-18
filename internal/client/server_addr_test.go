package client

import "testing"

func TestNormalizeServerAddr(t *testing.T) {
	cases := map[string]string{
		"192.168.1.1":      "192.168.1.1:8443", // 补默认端口
		"192.168.1.1:8443": "192.168.1.1:8443", // 已有端口，原样
		"192.168.1.1:9000": "192.168.1.1:9000", // 自定义端口，原样
		"example.com":      "example.com:8443",
		"example.com:443":  "example.com:443",
		"127.0.0.1":        "127.0.0.1:8443",
		"::1":              "[::1]:8443", // IPv6 无端口 → 加括号补端口
		"[::1]:8443":       "[::1]:8443",
		"":                 "",
	}
	for in, want := range cases {
		if got := NormalizeServerAddr(in); got != want {
			t.Errorf("NormalizeServerAddr(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestIsLANServer(t *testing.T) {
	lan := []string{
		"192.168.137.1", "192.168.137.1:8443", "10.0.0.5", "172.16.0.1:8443",
		"172.31.255.255", "127.0.0.1:8443", "127.0.0.1", "localhost", "localhost:8443", "::1",
	}
	wan := []string{
		"23.95.78.14:8443", "8.8.8.8", "example.com:8443", "1.1.1.1:443",
		"172.32.0.1", // 172.32 不在私网 172.16-31 段
	}
	for _, a := range lan {
		if !IsLANServer(a) {
			t.Errorf("IsLANServer(%q)=false, want true (LAN)", a)
		}
	}
	for _, a := range wan {
		if IsLANServer(a) {
			t.Errorf("IsLANServer(%q)=true, want false (WAN)", a)
		}
	}
}
