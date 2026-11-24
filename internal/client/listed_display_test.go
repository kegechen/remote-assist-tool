package client

import (
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/version"
)

// stderrOf 让 b 的连接提示写进返回的缓冲（默认写 os.Stderr）。不换全局的 os.Stderr：别的用例留下的
// goroutine 可能正在写它，-race 下会报数据竞争。
func stderrOf(b *HelpMCPBootstrap) *syncBuf {
	buf := &syncBuf{}
	b.stderr = buf
	return buf
}

// 协助信息里的地址是给协助端用的（spec 6.7）：经列表里的某个地址连上时照写那个地址；经本机专用地址连上时写列表里的
// 第一个地址——本机专用地址是本机才连得上的内网、Tailscale 地址，协助端连不上。
func TestFormatShareInstructionsListedAddr(t *testing.T) {
	n := directory.Node{NodeCert: directory.NodeCert{Prefix: "W", Official: true,
		TCP: []string{"a.example.test:8443", "b.example.test:8443"}, QUIC: []string{"q.example.test:8443"}}}
	quicOnly := directory.Node{NodeCert: directory.NodeCert{Prefix: "Q", Official: true, QUIC: []string{"q.example.test:8443"}}}
	for _, tc := range []struct {
		name   string
		n      directory.Node
		actual string
		want   string
	}{
		{"列表里的第二个地址", n, "b.example.test:8443", "b.example.test:8443"},
		{"只在 QUIC 里列出的地址", n, "q.example.test:8443", "q.example.test:8443"},
		{"主机名大小写不同", n, "B.Example.Test:8443", "b.example.test:8443"},
		{"本机专用地址", n, "192.0.2.10:8443", "a.example.test:8443"},
		{"只有 QUIC 的节点，本机专用地址", quicOnly, "192.0.2.10:8443", "q.example.test:8443"},
	} {
		cfg := &Config{ServerAddr: tc.actual, UseTLS: true, InsecureSkip: true}
		got := formatShareInstructions("W000000001", "u@h", cfg, &tc.n, time.Time{})
		if want := "中转服务: " + tc.want + " (TLS，证书已按节点列表校验)\n"; !strings.Contains(got, want) {
			t.Errorf("%s: 应含 %q:\n%s", tc.name, want, got)
		}
	}
}

// 经本机专用地址连上节点时，协助信息和协助码文件（宿主程序、webui 通道内升级读它，同样是给协助端用的）写列表里的地址。
func TestShareOnLocalAddrPublishesListedAddr(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	d := startRegRelay(t, regOK("D000000001", "tok-d", "", nil))
	// 列表里的公开地址在测试里连不上；本机经本机专用地址（d 实际监听的地址）连上，出示的是列表里的证书。
	listed := deadAddr(t)
	e.publish(m, 42, "", e.nodeEntry("D", liveNode{addr: listed, fp: d.fp}, directory.StatusActive, 100))
	e.setLocalAddr("D", d.addr)

	s, out, codeFile := nodeShareEnv(t, e, m, deadAddr(t))
	reconnect5s(t, s)
	if s.nodes.currentPrefix() != "D" || s.client.config.ServerAddr != d.addr {
		t.Fatalf("前提不成立：应经本机专用地址 %s 连上节点 D，实际节点 %q、地址 %s", d.addr, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	got := out.String()
	if !strings.Contains(got, "中转服务: "+listed+" (TLS，证书已按节点列表校验)") || strings.Contains(got, "中转服务: "+d.addr) {
		t.Fatalf("协助信息里应是列表里的地址 %s、不是本机专用地址 %s:\n%s", listed, d.addr, got)
	}
	if got, _ := readCodeFile(t, codeFile); got.Server != listed {
		t.Fatalf("协助码文件里的地址 = %q，应为列表里的 %s", got.Server, listed)
	}
}

// 显式模式照旧：协助码文件写实际连上的地址。
func TestShareCodeFileExplicitUnchanged(t *testing.T) {
	r := startRegRelay(t, regOK("X000000001", "tok", "", nil))
	s, _, codeFile := nodeShareEnv(t, nil, nil, r.addr)
	reconnect5s(t, s)
	if got, _ := readCodeFile(t, codeFile); got.Server != r.addr {
		t.Fatalf("协助码文件里的地址 = %q，应为 %s", got.Server, r.addr)
	}
}

// MCP connect 连上列表节点时，stderr 上那一行说明证书已按节点列表校验，第三方节点再加一行运营者（spec 6.7）；
// 显式连的（TOFU）照旧。
func TestMCPConnectDescribesListedRelay(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.thirdPartyEntry("W", w.node(), "Example-Ops"))

	t.Run("第三方节点", func(t *testing.T) {
		b := nodeBootstrap(t, e, m, d.addr)
		buf := stderrOf(b)
		if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		want := "MCP: 已连接中转服务 " + w.addr + " (TLS，证书已按节点列表校验)\nMCP: 节点运营者: Example-Ops（非官方节点）\n"
		if !strings.Contains(out, want) {
			t.Fatalf("stderr 应含 %q:\n%s", want, out)
		}
	})

	t.Run("官方节点", func(t *testing.T) {
		b := nodeBootstrap(t, e, m, d.addr)
		buf := stderrOf(b)
		if _, err := mcpConnect(t, b, `{"code":"D234-5678-9ABC"}`); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if want := "MCP: 已连接中转服务 " + d.addr + " (TLS，证书已按节点列表校验)\n"; !strings.Contains(out, want) || strings.Contains(out, "节点运营者") {
			t.Fatalf("stderr 应含 %q、不含运营者:\n%s", want, out)
		}
	})

	t.Run("经本机专用地址", func(t *testing.T) {
		local := startJoinRelay(t, joinOK(nil))
		e := newDirEnv(t)
		m := e.manager()
		listed := deadAddr(t)
		e.publish(m, 42, "", e.nodeEntry("M", liveNode{addr: listed, fp: local.fp}, directory.StatusActive, 100))
		e.setLocalAddr("M", local.addr)
		b := nodeBootstrap(t, e, m, listed)
		buf := stderrOf(b)
		if _, err := mcpConnect(t, b, `{"code":"M234-5678-9ABC"}`); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if want := "MCP: 已连接中转服务 " + local.addr + " (TLS，证书已按节点列表校验)\n"; !strings.Contains(out, want) {
			t.Fatalf("stderr 应含实际连的地址 %q:\n%s", want, out)
		}
	})

	t.Run("显式模式", func(t *testing.T) {
		b := NewHelpMCPBootstrap(nodeHelpConfig(t, d.addr))
		t.Cleanup(func() {
			b.mu.Lock()
			h := b.help
			b.mu.Unlock()
			if h != nil {
				h.client.Close()
			}
		})
		buf := stderrOf(b)
		if _, err := mcpConnect(t, b, `{"code":"D234-5678-9ABC"}`); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if want := "MCP: 已连接中转服务 " + d.addr + " (TLS，跳过证书校验)\n"; !strings.Contains(out, want) {
			t.Fatalf("stderr 应含 %q:\n%s", want, out)
		}
	})
}

// 找节点的结果只在变了时打一行：MCP 每次 connect（含自动重连）都按码重新找，同一个结果不用反复说。
func TestHelpRouteLogsOnlyOnChange(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	hn := testHelpNodes(t, e, m)
	before := e.logCount()
	// --no-auth 的固定码不路由、什么都不打，之后的结果照常打：上一次连的是默认节点，不是 W。
	for _, code := range []string{"W23456789ABC", "W98765432ABC", "X23456789ABC", "X98765432ABC", "D23456789ABC", "W23456789ABC", proto.NoAuthCode, "W23456789ABC"} {
		hn.route(code)
	}
	e.wantLogs(3, "协助码属于节点 node-W（W）")
	e.wantLogs(1, "协助码属于节点 node-D（D）")
	e.wantLogs(1, "没有这个协助码所属的节点")
	if n := e.logCount() - before; n != 5 {
		t.Fatalf("找节点只该打上面 5 行（固定码不打空行），实际 %d 行", n)
	}
}

// 命令行 help 连上后的提示同样按所在节点取升级链接（spec 6.6）：第三方节点不用它下发的，官方节点照用。
func TestHelpPrintJoinedUpdateLinks(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = "v1.0.0" // dev 版本不提示升级
	resp := &proto.JoinResponse{
		Success: true, SessionID: "ses-test", PeerVersion: "v1.0.0",
		UpdateInfo: proto.UpdateInfo{
			LatestVersion: "v99.0.0",
			DownloadURL:   "https://downloads.example.test/remote-{version}-{os}-{arch}{ext}",
			ReleasePage:   "https://downloads.example.test/release",
		},
	}
	for _, official := range []bool{true, false} {
		h := &HelpMode{node: &directory.Node{NodeCert: directory.NodeCert{Prefix: "W", Official: official, Operator: "Example-Ops"}}}
		var out strings.Builder
		h.printJoined(&out, resp)
		got := out.String()
		if !strings.Contains(got, "会话ID: ses-test") || strings.Count(got, "[提示]") != 2 {
			t.Fatalf("official=%v: 应有会话 ID 和本机、被协助端两条升级提示:\n%s", official, got)
		}
		if used := strings.Contains(got, "downloads.example.test"); used != official {
			t.Errorf("official=%v: 用了节点下发的升级链接 = %v:\n%s", official, used, got)
		}
	}
}

// 升级提示（spec 6.6）：help 连上第三方节点时不用它下发的下载链接，本机与被协助端的提示都换成内置的官方地址；
// 官方节点照用它下发的。
func TestHelpUpdateLinksOnNodes(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = "v1.0.0" // dev 版本不提示升级
	newer := func(proto.JoinRequest) *proto.JoinResponse {
		return &proto.JoinResponse{
			Success: true, SessionID: "ses-test", PeerVersion: "v1.0.0",
			UpdateInfo: proto.UpdateInfo{
				LatestVersion: "v99.0.0",
				DownloadURL:   "https://downloads.example.test/remote-{version}-{os}-{arch}{ext}",
				ReleasePage:   "https://downloads.example.test/release",
			},
		}
	}
	for _, official := range []bool{true, false} {
		w := startJoinRelay(t, newer)
		e := newDirEnv(t)
		m := e.manager()
		entry := e.nodeEntry("W", w.node(), directory.StatusActive, 100)
		if !official {
			entry = e.thirdPartyEntry("W", w.node(), "Example-Ops")
		}
		e.publish(m, 42, "", entry)
		b := nodeBootstrap(t, e, m, deadAddr(t))
		res, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`)
		if err != nil {
			t.Fatal(err)
		}
		if res.UpdateNotice == nil || res.PeerUpdateNotice == nil {
			t.Fatalf("official=%v: 应有本机和被协助端的升级提示: %+v / %+v", official, res.UpdateNotice, res.PeerUpdateNotice)
		}
		for _, url := range []string{res.UpdateNotice.URL, res.PeerUpdateNotice.URL} {
			if got := strings.Contains(url, "downloads.example.test"); got != official {
				t.Errorf("official=%v: 升级链接 %q 用了节点下发的 = %v", official, url, got)
			}
		}
	}
}
