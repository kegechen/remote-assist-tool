package relay

import "testing"

// same_network 下发给客户端后，客户端会只打私网地址、跳过公网打洞（旧客户端也是），
// 所以只有「同出口 IP」才能算同网。私网 /16 相同不够：企业内网、跨地域办公室普遍复用
// 10.x / 172.16.x 段，私网地址彼此不可达，误判后连公网打洞的机会都没了。
func TestDetectSameNetwork(t *testing.T) {
	cases := []struct {
		name                      string
		shareRemote, helpRemote   string
		sharePrivate, helpPrivate string
		want                      bool
	}{
		{"同出口 IP", "1.2.3.4:5000", "1.2.3.4:6000", "192.168.1.2:1", "192.168.1.3:1", true},
		{"同出口 IP、私网段不同", "1.2.3.4:5000", "1.2.3.4:6000", "192.168.1.2:1", "10.0.0.3:1", true},
		{"不同出口、私网同 /16（跨地域办公网）", "117.48.146.18:15492", "36.7.69.115:15784", "10.11.115.104:59810", "10.11.24.148:49773", false},
		{"不同出口、私网不同段", "1.2.3.4:5000", "5.6.7.8:6000", "192.168.1.2:1", "10.0.0.3:1", false},
		{"缺地址", "", "", "", "", false},
	}
	for _, c := range cases {
		if got := detectSameNetwork(c.shareRemote, c.helpRemote, c.sharePrivate, c.helpPrivate); got != c.want {
			t.Errorf("%s: detectSameNetwork = %v, want %v", c.name, got, c.want)
		}
	}
}
