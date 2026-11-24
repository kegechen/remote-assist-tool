package client

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
)

// 显式模式学节点列表（spec 6.1、6.5）：注册、加入时带上列表位置，收下连上的 relay 转发的列表；这台 relay 的证书是列表里
// 某个节点的、连的又不是它在列表里的地址，就把地址记为该节点的本机专用地址，之后自动模式连它时先试。不取 CF、不选节点。

// wantLocalAddr 断言缓存里节点 prefix 的本机专用地址是 want（空串表示没有）。
func wantLocalAddr(t *testing.T, m *dirManager, prefix, want string) {
	t.Helper()
	v, c, err := m.usable(m.now())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.LocalAddr(v, prefix); got != want {
		t.Fatalf("节点 %s 的本机专用地址 = %q，应为 %q", prefix, got, want)
	}
}

func TestLearnRecordsLocalAddr(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	fp := strings.Repeat("cd", 32)
	const listed = "w.example.test:8443"
	list := e.sign(42, e.nodeEntry("D", deadNode(t), directory.StatusActive, 100),
		e.nodeEntry("W", liveNode{addr: listed, fp: fp}, directory.StatusActive, 100))

	// 证书属于 W、连的不是 W 在列表里的地址：先收下列表，再记为 W 的本机专用地址。
	const local = "192.0.2.10:8443"
	if n := m.Learn(list, local, strings.ToUpper(fp)); n == nil || n.Prefix != "W" {
		t.Fatalf("应认出节点 W: %+v", n)
	}
	e.wantLogs(1, "已获取节点列表（来自 relay "+local+"）")
	e.wantLogs(1, "[relay] 已记住 "+local+" 为节点 node-W（W）的本机专用地址")
	wantLocalAddr(t, m, "W", local)

	// 再连一次同一个地址：已经记着，不再提示。
	if n := m.Learn(nil, local, fp); n == nil || n.Prefix != "W" {
		t.Fatalf("应认出节点 W: %+v", n)
	}
	e.wantLogs(1, "已记住")

	// 连的是 W 在列表里的地址（大小写不同也算）：不记，原来记的也不动。
	if n := m.Learn(nil, "W.Example.Test:8443", fp); n == nil || n.Prefix != "W" {
		t.Fatalf("应认出节点 W: %+v", n)
	}
	e.wantLogs(1, "已记住")
	wantLocalAddr(t, m, "W", local)

	// 换了个本机专用地址：改记新的。
	const other = "198.51.100.20:9443"
	m.Learn(nil, other, fp)
	e.wantLogs(1, "已记住 "+other+" 为节点 node-W（W）")
	wantLocalAddr(t, m, "W", other)

	// 证书不在列表里（自建 relay）：不记。
	if n := m.Learn(nil, "203.0.113.5:8443", strings.Repeat("ef", 32)); n != nil {
		t.Fatalf("证书不在列表里，不该认出节点: %+v", n)
	}
	if n := m.Learn(nil, "203.0.113.5:8443", ""); n != nil {
		t.Fatalf("明文连接没有证书，不该认出节点: %+v", n)
	}
	e.wantLogs(2, "已记住")
	if n := e.hitCount(); n != 0 {
		t.Fatalf("显式模式不取 CF，却请求了 %d 次", n)
	}
}

// 节点不论状态（排空、停用）都记：本机专用地址只是「这台机器怎么连它」，状态随列表更新会变回来。
func TestLearnRecordsAnyStatus(t *testing.T) {
	for _, status := range []string{directory.StatusDraining, directory.StatusDisabled} {
		t.Run(status, func(t *testing.T) {
			e := newDirEnv(t)
			m := e.manager()
			fp := strings.Repeat("cd", 32)
			list := e.sign(42, e.nodeEntry("D", deadNode(t), directory.StatusActive, 100),
				e.nodeEntry("W", liveNode{addr: "w.example.test:8443", fp: fp}, status, 100))
			if n := m.Learn(list, "192.0.2.10:8443", fp); n == nil || n.Prefix != "W" {
				t.Fatalf("应认出节点 W: %+v", n)
			}
			e.wantLogs(1, "[relay] 已记住 192.0.2.10:8443 为节点 node-W（W）的本机专用地址")
			wantLocalAddr(t, m, "W", "192.0.2.10:8443")
		})
	}
}

// 连的地址不合规（如端口写成 08443，拨号照样连得上）时不记，也不当成缓存读写失败来提示。
func TestLearnSkipsMalformedAddr(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	fp := strings.Repeat("cd", 32)
	list := e.sign(42, e.nodeEntry("D", deadNode(t), directory.StatusActive, 100),
		e.nodeEntry("W", liveNode{addr: "w.example.test:8443", fp: fp}, directory.StatusActive, 100))
	if n := m.Learn(list, "192.0.2.10:08443", fp); n == nil || n.Prefix != "W" {
		t.Fatalf("应认出节点 W: %+v", n)
	}
	e.wantLogs(0, "已记住")
	e.wantLogs(0, "读写节点列表缓存失败")
	wantLocalAddr(t, m, "W", "")
}

// 没有能用的列表时什么都不说：显式模式本来就不用列表，「改用内置默认节点」之类的提示不适用。
func TestLearnQuietWithoutList(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	fp := strings.Repeat("cd", 32)
	if n := m.Learn(nil, "192.0.2.10:8443", fp); n != nil {
		t.Fatalf("没有列表不该认出节点: %+v", n)
	}
	if n := e.logCount(); n != 0 {
		t.Fatalf("没有列表时不该有提示，却有 %d 行", n)
	}
	e.publish(m, 42, "", e.nodeEntry("W", liveNode{addr: "w.example.test:8443", fp: fp}, directory.StatusActive, 100))
	before := e.logCount()
	e.setNow(e.clock().Add(400 * 24 * time.Hour)) // 列表过期
	if n := m.Learn(nil, "192.0.2.10:8443", fp); n != nil {
		t.Fatalf("过期的列表不该认出节点: %+v", n)
	}
	if n := e.logCount(); n != before {
		t.Fatalf("列表过期时显式模式不该提示（「改用内置默认节点」不适用），却多了 %d 行", n-before)
	}
	var nilManager *dirManager
	if n, _, _ := nilManager.AddrNode("192.0.2.10:8443"); nilManager.Learn([]byte("{}"), "192.0.2.10:8443", "ab") != nil || n != nil {
		t.Fatal("没有管理器时什么都不做")
	}
}

// 显式进程里缓存的问题照常提示，但不说「按内置默认节点连接」：显式模式连的是指定的地址。
func TestOfflineManagerCacheNote(t *testing.T) {
	e := newDirEnv(t)
	if err := os.WriteFile(e.path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := e.manager()
	m.url, m.explicit = "", true
	m.Learn(nil, "192.0.2.10:8443", strings.Repeat("cd", 32))
	e.wantLogs(1, "节点列表缓存已损坏")
	e.wantLogs(0, "按内置默认节点连接")
}

// AddrNode 找地址属于哪个节点（不论状态）：列表地址（不分大小写，带上声明过的传输）、本机专用地址；
// 没有能用的列表时什么都找不到，也不提示。
func TestAddrNode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	// 还没有缓存（第一次运行的自动模式 MCP 进程连指定的 server）：没有列表，不能崩。
	if n, _, _ := m.AddrNode("d.example.test:8443"); n != nil {
		t.Fatalf("没有缓存时不该找到节点: %+v", n)
	}
	e.publish(m, 42, "", e.nodeEntry("D", liveNode{addr: "d.example.test:8443", fp: strings.Repeat("a1", 32)}, directory.StatusActive, 100),
		e.nodeEntry("W", liveNode{addr: "w.example.test:8443", fp: strings.Repeat("b2", 32)}, directory.StatusDisabled, 0))
	e.setLocalAddr("W", "w-lan.example.test:8443")
	n, tg, listed := m.AddrNode("D.Example.Test:8443")
	if n == nil || n.Prefix != "D" || !listed || tg != (nodeTarget{Addr: "d.example.test:8443", TCP: true}) {
		t.Fatalf("D 的列表地址: %+v %+v %v", n, tg, listed)
	}
	if n, _, listed := m.AddrNode("W-LAN.example.test:8443"); n == nil || n.Prefix != "W" || listed {
		t.Fatalf("W（已停用）的本机专用地址（主机名不分大小写）: %+v %v", n, listed)
	}
	// W 记下的本机专用地址恰好是 D 的列表地址：以列表为准，按 D 严格比对。
	e.setLocalAddr("W", "d.example.test:8443")
	if n, _, listed := m.AddrNode("d.example.test:8443"); n == nil || n.Prefix != "D" || !listed {
		t.Fatalf("与列表地址冲突时应以列表为准: %+v %v", n, listed)
	}
	if n, _, _ := m.AddrNode("203.0.113.5:8443"); n != nil {
		t.Fatalf("不属于任何节点的地址: %+v", n)
	}
	before := e.logCount()
	e.setNow(e.clock().Add(400 * 24 * time.Hour)) // 列表过期
	if n, _, _ := m.AddrNode("d.example.test:8443"); n != nil {
		t.Fatalf("列表过期后不该找到节点: %+v", n)
	}
	if n := e.logCount(); n != before {
		t.Fatalf("AddrNode 不提示列表的状态，却多了 %d 行", n-before)
	}
}

// 显式的 help（命令行写了 --server）：照旧 TOFU 连指定的地址，加入时带上列表位置，收下 relay 转发的列表并记下本机专用地址。
// 展示与升级链接不变（不当作列表节点）。
func TestExplicitHelpLearnsFromRelay(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	w := startJoinRelay(t, joinOK(&list))
	list = e.sign(42, e.nodeEntry("W", liveNode{addr: deadAddr(t), fp: w.fp}, directory.StatusActive, 100))

	h := NewHelpModeMCP(nodeHelpConfig(t, w.addr), "W234-5678-9ABC")
	h.learn = m
	t.Cleanup(h.client.Close)
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, w.addr, false)
	if req := w.requests()[0]; req.DirectorySerial == nil || *req.DirectorySerial != 0 {
		t.Fatalf("显式模式的加入请求也要带列表位置: %v", req.DirectorySerial)
	}
	e.wantLogs(1, "已获取节点列表（来自 relay "+w.addr+"）")
	e.wantLogs(1, "已记住 "+w.addr+" 为节点 node-W（W）的本机专用地址")
	if h.node != nil {
		t.Fatalf("显式进程照旧按 TOFU 展示，不当作列表节点: %+v", h.node)
	}
	if n := e.hitCount(); n != 0 {
		t.Fatalf("显式模式不取 CF，却请求了 %d 次", n)
	}
}

// aliasRelay 给监听在 addr（127.0.0.1 上）的 relay 起个不是回环的名字 <name>.example.test:<端口>，测试期间拨这个地址
// 会连到 addr。TOFU 对回环地址（127.0.0.1、localhost）豁免，用它才看得出指纹表有没有被查、被写。
func aliasRelay(t *testing.T, name, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	alias := net.JoinHostPort(name+".example.test", port)
	orig := dialOneFunc
	t.Cleanup(func() { dialOneFunc = orig })
	dialOneFunc = func(ctx context.Context, a, kind string, cfg *tls.Config) (net.Conn, error) {
		if strings.EqualFold(a, alias) {
			a = addr
		}
		return orig(ctx, a, kind, cfg)
	}
	return alias
}

// activeHelp 返回 MCP 当前的会话（没有时为 nil）。
func activeHelp(b *HelpMCPBootstrap) *HelpMode {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.help
}

// 自动模式的 MCP 进程里带 server 的 connect（spec 6.1），证书按地址属于哪个节点来校验。
// 列表地址：只认这个节点的证书，严格比对、不查也不写指纹表；别的节点的证书也拒绝（不然列表里任何一个运营者都能冒充别的节点）。
func TestMCPServerConnectListedAddrStrict(t *testing.T) {
	w, x := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	wAlias, dAlias := aliasRelay(t, "w", w.addr), aliasRelay(t, "d", x.addr) // D 的列表地址上是出示 E 证书的 relay
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", liveNode{addr: dAlias, fp: strings.Repeat("ab", 32)}, directory.StatusActive, 100),
		e.nodeEntry("E", liveNode{addr: deadAddr(t), fp: x.fp}, directory.StatusActive, 100),
		e.nodeEntry("W", liveNode{addr: wAlias, fp: w.fp}, directory.StatusActive, 100))
	// 不限定传输：列表地址只声明了 TCP，就只拨 TCP。拨了 QUIC 的话，假 relay 不开 UDP，要等到超时。
	b := nodeBootstrap(t, e, m, deadAddr(t))
	store := b.cfg.trustStore
	old := strings.Repeat("cd", 32)
	if err := store.Put(wAlias, old); err != nil {
		t.Fatal(err)
	}
	out := stderrOf(b)

	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+wAlias+`"}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Lookup(wAlias); got != old {
		t.Fatalf("按列表严格比对，不该动指纹表：%q 变成了 %q", old, got)
	}
	wantAt(t, activeHelp(b), wAlias, true)
	if want := "MCP: 已连接中转服务 " + wAlias + " (TLS，证书已按节点列表校验)\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("stderr 应含 %q:\n%s", want, out.String())
	}
	if req := w.requests()[0]; req.DirectorySerial == nil {
		t.Fatal("带 server 的 connect 也要带列表位置")
	}

	_, err := mcpConnect(t, b, `{"code":"D234-5678-9ABC","server":"`+dAlias+`"}`)
	if err == nil || !strings.Contains(err.Error(), "与节点列表不符") {
		t.Fatalf("D 的列表地址上出示的是 E 的证书，应拒绝: %v", err)
	}
	if n := len(x.requests()); n != 0 {
		t.Fatalf("证书不符就不该加入，却发了 %d 次加入请求", n)
	}
	if got, ok := store.Lookup(dAlias); ok {
		t.Fatalf("按列表严格比对，不该写指纹表: %q", got)
	}
	e.wantLogs(0, "已记住")
}

// 列表地址只用它声明过的传输：只有 TCP 的地址，--transport quic 连不了（同自动模式），不悄悄改用别的传输。
func TestMCPServerConnectListedAddrTransport(t *testing.T) {
	w := startJoinRelay(t, joinOK(nil))
	wAlias := aliasRelay(t, "w", w.addr)
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("W", liveNode{addr: wAlias, fp: w.fp}, directory.StatusActive, 100))
	b := nodeBootstrap(t, e, m, deadAddr(t))
	b.cfg.Transport = TransportQUIC
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+wAlias+`"}`); err == nil || !strings.Contains(err.Error(), "--transport quic 无法使用") {
		t.Fatalf("列表地址没有 QUIC，应报 --transport quic 无法使用: %v", err)
	}
}

// 本机专用地址：这个节点的证书直接放行（节点按列表换了证书、指纹表里还是旧的也连得上），不写指纹表；
// 别的证书（哪怕是列表里别的节点的）照旧 TOFU。
func TestMCPServerConnectLocalAddr(t *testing.T) {
	w, x := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	wAlias, xAlias := aliasRelay(t, "w", w.addr), aliasRelay(t, "x", x.addr)
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("W", liveNode{addr: deadAddr(t), fp: w.fp}, directory.StatusActive, 100),
		e.nodeEntry("E", liveNode{addr: deadAddr(t), fp: x.fp}, directory.StatusActive, 100))
	b := nodeBootstrap(t, e, m, deadAddr(t))
	b.cfg.Transport = TransportTCP // TOFU 拒绝后不再去试 QUIC：假 relay 只开 TCP，要等到超时
	store := b.cfg.trustStore
	old := strings.Repeat("cd", 32)
	for _, a := range []string{wAlias, xAlias} {
		if err := store.Put(a, old); err != nil {
			t.Fatal(err)
		}
	}
	out := stderrOf(b)

	e.setLocalAddr("W", wAlias)
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+wAlias+`"}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Lookup(wAlias); got != old {
		t.Fatalf("本机专用地址上节点的证书直接放行，不该动指纹表：%q 变成了 %q", old, got)
	}
	if want := "MCP: 已连接中转服务 " + wAlias + " (TLS，证书已按节点列表校验)\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("stderr 应含 %q:\n%s", want, out.String())
	}

	// W 的本机专用地址上出示的是 E 的证书：不放行，照旧 TOFU（指纹表里记的不是它）。
	e.setLocalAddr("W", xAlias)
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+xAlias+`"}`); err == nil || !strings.Contains(err.Error(), "证书指纹与首次连接时不一致") {
		t.Fatalf("W 的本机专用地址上出示的是 E 的证书，应照旧按 TOFU 拒绝: %v", err)
	}
	e.wantLogs(0, "已记住")
}

// 未知地址（不是列表地址，也没记成本机专用地址）照旧 TOFU，列表里节点的证书也一样：指纹表里记的不是它就拒绝。
// 第一次连（指纹表里没有）时照常记下指纹，连上的是列表节点就记成它的本机专用地址，之后按本机专用地址放行。
// 证书不在列表里的照旧 TOFU、照旧展示，不记本机专用地址。
func TestMCPServerConnectUnknownAddrTOFU(t *testing.T) {
	w, y := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	wAlias, wAlias2, yAlias := aliasRelay(t, "w", w.addr), aliasRelay(t, "w2", w.addr), aliasRelay(t, "y", y.addr)
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("W", liveNode{addr: deadAddr(t), fp: w.fp}, directory.StatusActive, 100))
	b := nodeBootstrap(t, e, m, deadAddr(t))
	b.cfg.Transport = TransportTCP // TOFU 拒绝后不再去试 QUIC：假 relay 只开 TCP，要等到超时
	store := b.cfg.trustStore
	if err := store.Put(wAlias, strings.Repeat("cd", 32)); err != nil {
		t.Fatal(err)
	}
	out := stderrOf(b)

	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+wAlias+`"}`); err == nil || !strings.Contains(err.Error(), "证书指纹与首次连接时不一致") {
		t.Fatalf("未知地址照旧 TOFU，指纹表里记的不是它就拒绝: %v", err)
	}

	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+wAlias2+`"}`); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Lookup(wAlias2); !ok || got != w.fp {
		t.Fatalf("第一次连照常 TOFU，应记下指纹 %s: %q", w.fp, got)
	}
	e.wantLogs(1, "已记住 "+wAlias2+" 为节点 node-W（W）的本机专用地址")
	if want := "MCP: 已连接中转服务 " + wAlias2 + " (TLS，证书已按节点列表校验)\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("stderr 应含 %q:\n%s", want, out.String())
	}

	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+yAlias+`"}`); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Lookup(yAlias); !ok || got != y.fp {
		t.Fatalf("证书不在列表里照旧 TOFU，应记下指纹 %s: %q", y.fp, got)
	}
	if want := "MCP: 已连接中转服务 " + yAlias + " (TLS，跳过证书校验)\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("stderr 应含 %q:\n%s", want, out.String())
	}
	e.wantLogs(1, "已记住")
}

// 带 server 的 connect 也在后台按 RefreshIfStale 的规则刷新列表：列表地址按列表严格比对，列表旧了就连不上，只能靠 CF 更新。
func TestMCPServerConnectRefreshesList(t *testing.T) {
	y := startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	b := nodeBootstrap(t, e, m, deadAddr(t)) // 还没有缓存：谈不上年龄，要取
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+y.addr+`"}`); err != nil {
		t.Fatal(err)
	}
	b.nodes.bg.Wait()
	if n := e.hitCount(); n != 1 {
		t.Fatalf("应在后台从 CF 取一次列表，实际请求了 %d 次", n)
	}
}

// 连的地址是别的节点在列表里的地址时以列表为准：不记成本机专用地址，也不当作在列表节点上（返回 nil），
// 警告一次（地址属于 D，出示的却是 W 的证书，可能被劫持）。这个地址上恢复正常后再出现，会再警告。
func TestLearnOtherNodesListedAddr(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	fp, dFP := strings.Repeat("cd", 32), strings.Repeat("ab", 32)
	list := e.sign(42, e.nodeEntry("D", liveNode{addr: "d.example.test:8443", fp: dFP}, directory.StatusActive, 100),
		e.nodeEntry("W", liveNode{addr: "w.example.test:8443", fp: fp}, directory.StatusActive, 100))
	const warn = "警告：D.Example.Test:8443 是节点 node-D（D）在列表里的地址，出示的却是节点 node-W（W）的证书"
	for _, addr := range []string{"D.Example.Test:8443", "d.example.test:8443"} { // 同一个地址（主机名不分大小写）只警告一次
		if n := m.Learn(list, addr, fp); n != nil {
			t.Fatalf("地址是 D 的列表地址、证书是 W 的，不该认作 W: %+v", n)
		}
	}
	e.wantLogs(1, warn)
	e.wantLogs(1, "可能被劫持")
	e.wantLogs(0, "已记住")
	wantLocalAddr(t, m, "W", "")

	if n := m.Learn(nil, "D.Example.Test:8443", dFP); n == nil || n.Prefix != "D" {
		t.Fatalf("D 的列表地址上出示 D 的证书，应认出 D: %+v", n)
	}
	m.Learn(nil, "D.Example.Test:8443", fp)
	e.wantLogs(2, warn)
}

// 还没有能用的列表时连某节点的列表地址，照旧 TOFU。relay 转发来的列表说这个地址属于 D、出示的却是 E 的证书时，
// 不当作在列表节点上（展示照旧 TOFU 的写法，升级链接照旧），警告一次。下一次连接有了列表，就按 D 严格比对。
func TestMCPServerConnectOtherNodesCertBeforeList(t *testing.T) {
	var list []byte
	x := startJoinRelay(t, joinOK(&list))
	dAlias := aliasRelay(t, "d", x.addr)
	e := newDirEnv(t)
	m := e.manager()
	list = e.sign(42, e.nodeEntry("D", liveNode{addr: dAlias, fp: strings.Repeat("ab", 32)}, directory.StatusActive, 100),
		e.nodeEntry("E", liveNode{addr: deadAddr(t), fp: x.fp}, directory.StatusActive, 100))
	b := nodeBootstrap(t, e, m, deadAddr(t)) // 还没有缓存
	b.cfg.Transport = TransportTCP           // 假 relay 只开 TCP
	out := stderrOf(b)

	if _, err := mcpConnect(t, b, `{"code":"D234-5678-9ABC","server":"`+dAlias+`"}`); err != nil {
		t.Fatal(err)
	}
	b.nodes.bg.Wait()
	if h := activeHelp(b); h.node != nil {
		t.Fatalf("地址属于 D、证书是 E 的，不该当作在节点 %s 上", h.node.Prefix)
	}
	if want := "MCP: 已连接中转服务 " + dAlias + " (TLS，跳过证书校验)\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("stderr 应含 %q:\n%s", want, out.String())
	}
	e.wantLogs(1, "警告："+dAlias+" 是节点 node-D（D）在列表里的地址，出示的却是节点 node-E（E）的证书", "可能被劫持")
}

// nodeByAddr：本机专用地址恰好是别的节点的列表地址时，以列表为准。
func TestNodeByAddrPrefersListed(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	const addr = "d.example.test:8443"
	e.publish(m, 42, "", e.nodeEntry("W", deadNode(t), directory.StatusActive, 100),
		e.nodeEntry("D", liveNode{addr: addr, fp: strings.Repeat("cd", 32)}, directory.StatusActive, 100))
	e.setLocalAddr("W", addr)
	v, cache, err := m.usable(m.now())
	if err != nil {
		t.Fatal(err)
	}
	if n := nodeByAddr(v, cache, "D.example.test:8443"); n == nil || n.Prefix != "D" {
		t.Fatalf("%s 是 D 的列表地址，应找到 D: %+v", addr, n)
	}
}

// LearnNodeList（显式进程用）建只学列表的管理器：缓存在 home，不从 CF 取，也不读 REMOTE_RELAY_DIRECTORY；不进自动模式。
func TestLearnNodeListIsOffline(t *testing.T) {
	t.Setenv(directoryEnv, "https://directory.example.test/v1/directory")
	home := t.TempDir()
	check := func(name string, m *dirManager) {
		t.Helper()
		if m == nil || m.url != "" || !m.explicit || m.path != filepath.Join(home, directoryCacheFile) {
			t.Fatalf("%s 的管理器应只学列表、缓存在 %s: %+v", name, home, m)
		}
	}
	s, _, _ := nodeShareEnv(t, newDirEnv(t), nil, "127.0.0.1:1")
	s.LearnNodeList(home)
	check("share", s.learn)
	h := NewHelpModeMCP(nodeHelpConfig(t, "127.0.0.1:1"), "W234-5678-9ABC")
	h.LearnNodeList(home)
	check("help", h.learn)
	b := NewHelpMCPBootstrap(nodeHelpConfig(t, "127.0.0.1:1"))
	b.LearnNodeList(home)
	check("MCP", b.learn)
	if s.nodes != nil || h.nodes != nil || b.nodes != nil || s.stunFollowsRelay || h.stunFollowsRelay || b.stunFollowsRelay {
		t.Fatal("显式模式不进自动模式")
	}
}

// 显式的 MCP 进程（命令行写了 --server）：带不带 server 都照旧 TOFU、按原样展示，只学列表。
func TestExplicitMCPProcessStaysTOFU(t *testing.T) {
	var list []byte
	w := startJoinRelay(t, joinOK(&list))
	e := newDirEnv(t)
	m := e.manager()
	list = e.sign(42, e.nodeEntry("W", liveNode{addr: deadAddr(t), fp: w.fp}, directory.StatusActive, 100))
	wAlias := aliasRelay(t, "w", w.addr)
	b := NewHelpMCPBootstrap(nodeHelpConfig(t, wAlias))
	b.learn = m
	t.Cleanup(func() {
		b.mu.Lock()
		h := b.help
		b.mu.Unlock()
		if h != nil {
			h.client.Close()
		}
	})
	out := stderrOf(b)
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.cfg.trustStore.Lookup(wAlias); !ok || got != w.fp {
		t.Fatalf("显式进程照旧 TOFU，应记下指纹 %s: %q", w.fp, got)
	}
	if strings.Contains(out.String(), "证书已按节点列表校验") || !strings.Contains(out.String(), "MCP: 已连接中转服务 "+wAlias) {
		t.Fatalf("显式进程照旧按 TOFU 展示:\n%s", out.String())
	}
	e.wantLogs(1, "已记住 "+wAlias+" 为节点 node-W（W）的本机专用地址")

	// 带 server 也一样：连的虽是 W 的本机专用地址、出示的也是 W 的证书，照样按指纹表核对。
	if err := b.cfg.trustStore.Put(wAlias, strings.Repeat("cd", 32)); err != nil {
		t.Fatal(err)
	}
	b.cfg.Transport = TransportTCP // TOFU 拒绝后不再去试 QUIC：假 relay 只开 TCP，要等到超时
	if _, err := mcpConnect(t, b, `{"code":"W999-5678-9ABC","server":"`+wAlias+`"}`); err == nil || !strings.Contains(err.Error(), "证书指纹与首次连接时不一致") {
		t.Fatalf("显式进程带 server 也照旧 TOFU: %v", err)
	}
}

// 显式的 share：照旧只连指定的地址，注册时带上列表位置，收下 relay 转发的列表并记下本机专用地址。
func TestExplicitShareLearnsFromRelay(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	w := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message { return regOK("W000000001", "tok-w", "", list)(req) })
	list = e.sign(42, e.nodeEntry("W", liveNode{addr: deadAddr(t), fp: w.fp}, directory.StatusActive, 100))

	s, _, codeFile := nodeShareEnv(t, e, nil, w.addr)
	s.learn = m
	reconnect5s(t, s)
	if req := recv5s(t, "注册请求", w.reqs); req.DirectorySerial == nil || *req.DirectorySerial != 0 {
		t.Fatalf("显式模式的注册请求也要带列表位置: %v", req.DirectorySerial)
	}
	e.wantLogs(1, "已获取节点列表（来自 relay "+w.addr+"）")
	e.wantLogs(1, "已记住 "+w.addr+" 为节点 node-W（W）的本机专用地址")
	if s.client.node != nil || s.nodes != nil {
		t.Fatal("显式模式不选节点、不进节点模式")
	}
	if got, _ := readCodeFile(t, codeFile); got.Server != w.addr {
		t.Fatalf("显式模式 code-file 照旧写实际地址 %s: %q", w.addr, got.Server)
	}
	if n := e.hitCount(); n != 0 {
		t.Fatalf("显式模式不取 CF，却请求了 %d 次", n)
	}
}

// 显式的 share 连的 relay 在排空：带了列表位置（声明了能力）就被拒。显式模式只连这一台，之后注册不再带位置，
// 像旧客户端那样被接纳（与没有这个功能时一样）。
func TestExplicitShareDrainingStopsDeclaring(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	w := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message {
		if req.DirectorySerial != nil {
			return regError(proto.ErrCodeNodeDraining)(req)
		}
		return regOK("W000000001", "tok-w", "", nil)(req)
	})
	s, out, _ := nodeShareEnv(t, e, nil, w.addr)
	s.learn = m
	reconnect5s(t, s)
	if req := recv5s(t, "第一次注册", w.reqs); req.DirectorySerial == nil {
		t.Fatal("第一次注册应带列表位置")
	}
	if req := recv5s(t, "第二次注册", w.reqs); req.DirectorySerial != nil {
		t.Fatal("被排空拒绝后不该再带列表位置")
	}
	if s.code != "W000000001" {
		t.Fatalf("应在这台 relay 上注册成功: %q", s.code)
	}
	if !strings.Contains(out.String(), "正在排空") {
		t.Fatalf("应说明为什么不带节点列表:\n%s", out.String())
	}
	reconnect5s(t, s)
	if req := recv5s(t, "重连注册", w.reqs); req.DirectorySerial != nil {
		t.Fatal("之后的注册也不再带列表位置")
	}
}

// 停用的节点对新旧客户端都拒绝（spec 第 7 节），不带列表位置也没用：只有排空才不再带，被停用拒绝后照旧带。
func TestExplicitShareDisabledKeepsDeclaring(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var calls atomic.Int32
	w := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message {
		if calls.Add(1) == 1 {
			return regError(proto.ErrCodeNodeDisabled)(req)
		}
		return regOK("W000000001", "tok-w", "", nil)(req)
	})
	s, out, _ := nodeShareEnv(t, e, nil, w.addr)
	s.learn = m
	reconnect5s(t, s)
	if req := recv5s(t, "第一次注册", w.reqs); req.DirectorySerial == nil {
		t.Fatal("第一次注册应带列表位置")
	}
	if req := recv5s(t, "第二次注册", w.reqs); req.DirectorySerial == nil {
		t.Fatal("被停用拒绝后照旧带列表位置")
	}
	if strings.Contains(out.String(), "正在排空") {
		t.Fatalf("停用不是排空:\n%s", out.String())
	}
}
