package gui

import "testing"

func TestSplitCodeServer(t *testing.T) {
	cases := []struct{ code, server, wantCode, wantServer string }{
		{"ABCD-EFGHJKMN", "", "ABCD-EFGHJKMN", ""},
		{"ABCD-EFGHJKMN@127.0.0.1:18443", "", "ABCD-EFGHJKMN", "127.0.0.1:18443"},
		{"  ABCD@10.0.0.2:8443  ", "", "ABCD", "10.0.0.2:8443"},
		{"ABCD@10.0.0.2:8443", "other:1", "ABCD", "other:1"}, // 显式 server 优先
		{"ABCD@", "", "ABCD", ""},
		{"", "", "", ""},
	}
	for _, c := range cases {
		gc, gs := splitCodeServer(c.code, c.server)
		if gc != c.wantCode || gs != c.wantServer {
			t.Errorf("splitCodeServer(%q,%q)=(%q,%q) want (%q,%q)", c.code, c.server, gc, gs, c.wantCode, c.wantServer)
		}
	}
}
