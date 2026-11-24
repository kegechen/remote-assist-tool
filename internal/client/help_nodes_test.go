package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)

// joinRelay 是只会加入的假 relay（TLS，只开 TCP）：加入请求交给 reply 决定怎么回，并应答工具握手（够 MCP connect
// 跑通）。别的消息不理。
type joinRelay struct {
	addr, fp string
	reply    func(req proto.JoinRequest) *proto.JoinResponse
	accepted atomic.Int32
	closed   atomic.Int32 // 客户端关掉（或被 stop 断开）的连接数

	mu        sync.Mutex
	reqs      []proto.JoinRequest
	all       []net.Conn
	ln        net.Listener
	afterJoin *proto.Message // 非 nil 时紧跟在加入响应后面发
}

// sendAfterJoin 让 relay 在每个加入响应之后紧跟着发 m。
func (f *joinRelay) sendAfterJoin(m *proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterJoin = m
}

func startJoinRelay(t *testing.T, reply func(req proto.JoinRequest) *proto.JoinResponse) *joinRelay {
	t.Helper()
	certFile, keyFile := newCertPair(t)
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	f := &joinRelay{addr: ln.Addr().String(), fp: crypto.CertFingerprint(pair.Certificate[0]), reply: reply, ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepted.Add(1)
			go f.serve(c)
		}
	}()
	t.Cleanup(f.stop)
	return f
}

// stop 关掉监听和已有连接。
func (f *joinRelay) stop() {
	f.ln.Close()
	f.dropConns()
}

// dropConns 断开已有连接，照常接受新连接。
func (f *joinRelay) dropConns() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.all {
		c.Close()
	}
}

func (f *joinRelay) serve(c net.Conn) {
	f.mu.Lock()
	f.all = append(f.all, c)
	f.mu.Unlock()
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	for {
		var msg proto.Message
		if err := dec.Decode(&msg); err != nil {
			f.closed.Add(1)
			return
		}
		var out *proto.Message
		switch msg.Type {
		case proto.MsgJoinRequest:
			var req proto.JoinRequest
			_ = json.Unmarshal(msg.Payload, &req)
			f.mu.Lock()
			f.reqs = append(f.reqs, req)
			f.mu.Unlock()
			out, _ = proto.NewMessage(proto.MsgJoinResponse, f.reply(req))
		case proto.MsgToolHello:
			peer := proto.NewHello("")
			out, _ = proto.NewMessage(proto.MsgToolHelloAck, &proto.HelloAck{
				Version: proto.ToolProtocolVersion, Capabilities: peer.Capabilities, NonceB64: peer.NonceB64, Accept: true,
			})
		default:
			continue
		}
		if enc.Encode(out) != nil {
			return
		}
		if msg.Type == proto.MsgJoinRequest {
			f.mu.Lock()
			after := f.afterJoin
			f.mu.Unlock()
			if after != nil && enc.Encode(after) != nil {
				return
			}
		}
	}
}

// requests 返回到现在为止收到的加入请求。help 是收到响应才返回的，所以连接返回之后读到的已经是全部。
func (f *joinRelay) requests() []proto.JoinRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.JoinRequest(nil), f.reqs...)
}

func (f *joinRelay) node() liveNode { return liveNode{addr: f.addr, fp: f.fp} }

// joinOK 让加入成功；dir 非 nil 时随响应下发 *dir（在 relay 收到请求时读）。
func joinOK(dir *[]byte) func(proto.JoinRequest) *proto.JoinResponse {
	return func(proto.JoinRequest) *proto.JoinResponse {
		resp := &proto.JoinResponse{Success: true, SessionID: "ses-test"}
		if dir != nil {
			resp.Directory = *dir
		}
		return resp
	}
}

func joinRejected(msg string) func(proto.JoinRequest) *proto.JoinResponse {
	return func(proto.JoinRequest) *proto.JoinResponse {
		return &proto.JoinResponse{Success: false, Error: msg}
	}
}

func nodeHelpConfig(t *testing.T, defaultAddr string) *Config {
	t.Helper()
	return &Config{
		ServerAddr: defaultAddr, UseTLS: true, InsecureSkip: true, P2PMode: "disabled",
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	}
}

// setHelpRefreshWait 把找不到前缀时为 CF 等的上限改成 d，测试结束时恢复。
func setHelpRefreshWait(t *testing.T, d time.Duration) {
	t.Helper()
	orig := helpRefreshWait
	helpRefreshWait = d
	t.Cleanup(func() { helpRefreshWait = orig })
}

// testHelpNodes 返回用 m 管理列表、提示收进 e 的 helpNodes。测试结束前等后台刷新退出：它还在读写缓存时删不掉临时目录。
// 用了 e.hold() 的测试要在这之后再 hold，放行（Cleanup 后进先出）才会排在等待之前。
func testHelpNodes(t *testing.T, e *dirEnv, m *dirManager) *helpNodes {
	t.Helper()
	hn := newHelpNodes(m, "", e.logf)
	t.Cleanup(hn.bg.Wait)
	return hn
}

// nodeHelp 返回一个自动模式的 HelpMode：默认地址 defaultAddr，协助码 code。
func nodeHelp(t *testing.T, e *dirEnv, m *dirManager, defaultAddr, code string) *HelpMode {
	t.Helper()
	h := NewHelpModeMCP(nodeHelpConfig(t, defaultAddr), code)
	h.useNodes(testHelpNodes(t, e, m))
	t.Cleanup(h.client.Close)
	return h
}

type joinOutcome struct {
	resp *proto.JoinResponse
	err  error
}

// join5s 跑一次 connectAndJoin，最多等 5 秒。
func join5s(t *testing.T, h *HelpMode) (*proto.JoinResponse, error) {
	t.Helper()
	ch := make(chan joinOutcome, 1)
	go func() {
		resp, err := h.connectAndJoin()
		ch <- joinOutcome{resp, err}
	}()
	r := recv5s(t, "connectAndJoin", ch)
	return r.resp, r.err
}

func wantJoins(t *testing.T, f *joinRelay, name string, n int) {
	t.Helper()
	if got := len(f.requests()); got != n {
		t.Fatalf("%s 收到 %d 个加入请求，应为 %d", name, got, n)
	}
}

func wantUntouched(t *testing.T, f *joinRelay, name string) {
	t.Helper()
	if got := f.accepted.Load(); got != 0 {
		t.Fatalf("%s 不该被连接，却收到 %d 条连接", name, got)
	}
}

// wantAt 断言 h 连在 addr 上；node 表示是不是按列表严格比对证书连上的（节点模式）。
func wantAt(t *testing.T, h *HelpMode, addr string, node bool) {
	t.Helper()
	if got := h.client.serverAddr(); got != addr {
		t.Fatalf("连到了 %s，应为 %s", got, addr)
	}
	if (h.client.node != nil) != node {
		t.Fatalf("节点模式 = %v，应为 %v", h.client.node != nil, node)
	}
}

// 码首位是节点前缀：连这个节点，证书按列表严格比对，加入请求带上列表位置。码按 relay 的规则规范化（去分隔符、转大写）。
func TestHelpRoutesByCodePrefix(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "w234-5678 9abc")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, w.addr, true)
	wantJoins(t, w, "W", 1)
	wantUntouched(t, d, "默认节点 D")
	req := w.requests()[0]
	if req.Code != "w23456789abc" {
		t.Fatalf("加入用的码 = %q", req.Code)
	}
	v, _ := m.Current()
	if req.DirectorySerial == nil || *req.DirectorySerial != 42 || req.DirectoryKeySerial != v.StatusKeySerial || v.StatusKeySerial == 0 {
		t.Fatalf("加入请求的列表位置 = (%v, %d)，应为 (42, %d)", req.DirectorySerial, req.DirectoryKeySerial, v.StatusKeySerial)
	}
	e.wantLogs(1, "协助码属于节点 node-W（W）")
}

// 列表里找不到前缀时同步向 CF 取一次（和后台刷新合并成一个请求），取到了就按新列表连。
func TestHelpUnknownPrefixRefreshesList(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	dEntry, wEntry := e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100)
	e.publish(m, 42, "", dEntry)
	e.serve(e.sign(43, dEntry, wEntry))
	e.setNow(dirtest.Epoch.Add(7 * time.Hour)) // 列表 6 小时没确认过
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, w.addr, true)
	wantUntouched(t, d, "默认节点 D")
	if n := e.hitCount(); n != 1 {
		t.Fatalf("CF 收到 %d 个请求，应为 1（后台刷新与找不到前缀时的同步刷新合并）", n)
	}
}

// 找不到前缀、列表又是新的（不用取）时直接连列表里的默认节点，严格比对证书。
func TestHelpUnknownPrefixUsesDefaultNode(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, deadAddr(t), "X23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, d.addr, true)
	wantJoins(t, d, "D", 1)
	wantUntouched(t, w, "W")
	if n := e.hitCount(); n != 0 {
		t.Fatalf("列表是新的，不该向 CF 取，CF 却收到 %d 个请求", n)
	}
	e.wantLogs(1, "没有这个协助码所属的节点")
}

// 停用的节点不连：它的码当作找不到前缀，连默认节点。
func TestHelpDisabledNodeNotRouted(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusDisabled, 100))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, d.addr, true)
	wantUntouched(t, w, "停用的 W")
}

// 没有能用的列表时与没有这个功能时一样：连默认地址，按配置走 TOFU。加入请求照样带列表位置（能力声明）。
func TestHelpWithoutListUsesDefaultAddr(t *testing.T) {
	d := startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t) // CF 没有列表（503）
	m := e.manager()
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, d.addr, false)
	req := d.requests()[0]
	if req.DirectorySerial == nil || *req.DirectorySerial != 0 {
		t.Fatalf("没有缓存时应声明位置 0: %v", req.DirectorySerial)
	}
	if n := e.hitCount(); n != 1 {
		t.Fatalf("CF 收到 %d 个请求，应为 1", n)
	}
	e.wantLogs(0, "没有这个协助码所属的节点") // 没有列表就谈不上找不到
}

// 默认地址上的 relay 随加入响应下发了更新的列表：收进缓存。
func TestHelpStoresListFromRelay(t *testing.T) {
	var list []byte
	d := startJoinRelay(t, joinOK(&list))
	e := newDirEnv(t)
	m := e.manager()
	dEntry := e.nodeEntry("D", d.node(), directory.StatusActive, 100)
	e.publish(m, 42, "", dEntry)
	list = e.sign(43, dEntry)
	h := nodeHelp(t, e, m, d.addr, "D23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.Current(); v == nil || v.Serial != 43 {
		t.Fatalf("relay 下发的列表没收下: %+v", v)
	}
	e.wantLogs(1, "节点列表已更新（来自 relay "+d.addr+"）")
}

// 过渡期回退：按前缀连到的不是默认节点、在那里加入失败时，去默认节点再试一次（节点模式启用前发的码首位是随机的）。
func TestHelpFallsBackToDefaultNode(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinRejected("join failed"))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, deadAddr(t), "W23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, d.addr, true)
	if h.node == nil || h.node.Prefix != "D" {
		t.Fatalf("回退后所在的节点应为默认节点 D（展示、升级链接按它）: %+v", h.node)
	}
	wantJoins(t, w, "W", 1)
	wantJoins(t, d, "D", 1)
	e.wantLogs(1, "在节点 node-W（W）上加入失败", "默认节点")
}

// 列表里的默认节点不能用（这里是停用）时，回退的「默认节点」是默认地址（TOFU），与没有列表时一样。
func TestHelpFallsBackToDefaultAddrWithoutDefaultNode(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinRejected("join failed"))
	e := newDirEnv(t)
	m := e.manager()
	other := startJoinRelay(t, joinOK(nil))
	e.publish(m, 42, "", e.nodeEntry("Q", other.node(), directory.StatusDisabled, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, d.addr, false)
	if h.node != nil {
		t.Fatalf("回退到的是默认地址（TOFU），不在列表节点上: %+v", h.node)
	}
	wantJoins(t, w, "W", 1)
	wantUntouched(t, other, "停用的默认节点")
}

// 连的就是默认节点时没有可回退的地方。
func TestHelpNoFallbackFromDefaultNode(t *testing.T) {
	d, w := startJoinRelay(t, joinRejected("join failed")), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "D23456789ABC")
	_, err := join5s(t, h)
	var ce *relayConnectError
	if err == nil || errors.As(err, &ce) || !errors.Is(err, ErrJoinFailed) {
		t.Fatalf("应为加入失败: %v", err)
	}
	wantJoins(t, d, "D", 1)
	wantUntouched(t, w, "W")
	waitFor(t, "加入失败后关掉连接", func() bool { return d.closed.Load() == 1 })
}

// 没有列表时连的是默认地址，加入失败也没有可回退的地方。
func TestHelpNoFallbackWithoutList(t *testing.T) {
	d := startJoinRelay(t, joinRejected("join failed"))
	e := newDirEnv(t)
	h := nodeHelp(t, e, e.manager(), d.addr, "W23456789ABC")
	if _, err := join5s(t, h); !errors.Is(err, ErrJoinFailed) {
		t.Fatalf("应为加入失败: %v", err)
	}
	if n := d.accepted.Load(); n != 1 {
		t.Fatalf("默认地址被连了 %d 次，应为 1", n)
	}
}

// 找不到前缀而连了默认节点的，加入失败也不再试。
func TestHelpNoFallbackAfterUnknownPrefix(t *testing.T) {
	d := startJoinRelay(t, joinRejected("join failed"))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "X23456789ABC")
	if _, err := join5s(t, h); err == nil {
		t.Fatal("加入失败却返回成功")
	}
	wantJoins(t, d, "D", 1)
	if n := d.accepted.Load(); n != 1 {
		t.Fatalf("默认节点被连了 %d 次，应为 1", n)
	}
}

// 被封禁不回退：封的是本机，去默认节点只会多挨一次。
func TestHelpNoFallbackWhenBlocked(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinRejected(proto.BlockedMessage))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("应报封禁: %v", err)
	}
	wantUntouched(t, d, "默认节点 D")
}

// 「同源已有协助端」不回退：会话就在按前缀找到的节点上，去默认节点只会多记一次失败。
func TestHelpNoFallbackWhenSessionBusy(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinRejected(proto.ErrJoinSessionBusySameAddr))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); !errors.Is(err, ErrSessionBusySameAddr) {
		t.Fatalf("应报同源已有协助端: %v", err)
	}
	wantUntouched(t, d, "默认节点 D")
}

// 连不上按前缀找到的节点不回退（回退只接住加入失败）；报的是连接失败，不是加入失败。
func TestHelpNoFallbackWhenNodeUnreachable(t *testing.T) {
	d := startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", deadNode(t), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	_, err := join5s(t, h)
	var ce *relayConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("应为连接失败: %v", err)
	}
	wantUntouched(t, d, "默认节点 D")
}

// 回退到默认节点也没成功时，报按前缀找到的节点上的加入错误：码的首位说它属于那个节点，默认节点连不上
// 不能说成「连不上 relay」。
func TestHelpFallbackFailureKeepsRoutedJoinError(t *testing.T) {
	w := startJoinRelay(t, joinRejected("W 上加入失败"))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", deadNode(t), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, deadAddr(t), "W23456789ABC")
	_, err := join5s(t, h)
	var ce *relayConnectError
	if err == nil || errors.As(err, &ce) || !errors.Is(err, ErrJoinFailed) {
		t.Fatalf("应报 W 上的加入错误: %v", err)
	}
	e.wantLogs(1, "默认节点上也没能加入")

	// D 回「同源已有协助端」，与 W 不说原因的拒绝（ErrJoinFailed）区分开，看报的是哪一台的。
	d := startJoinRelay(t, joinRejected(proto.ErrJoinSessionBusySameAddr))
	e2 := newDirEnv(t)
	m2 := e2.manager()
	e2.publish(m2, 42, "", e2.nodeEntry("D", d.node(), directory.StatusActive, 100), e2.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h2 := nodeHelp(t, e2, m2, deadAddr(t), "W23456789ABC")
	if _, err := join5s(t, h2); !errors.Is(err, ErrJoinFailed) || errors.Is(err, ErrSessionBusySameAddr) {
		t.Fatalf("应报 W 上的加入错误: %v", err)
	}
	wantJoins(t, d, "D", 1)
}

// --no-auth 的固定码不是任何节点发的：不路由，也不为它等 CF。
func TestHelpNoAuthCodeNotRouted(t *testing.T) {
	d, n := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	prefix := strings.ToUpper(proto.NoAuthCode[:1])
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry(prefix, n.node(), directory.StatusActive, 100))
	e.setNow(dirtest.Epoch.Add(7 * time.Hour))
	h := nodeHelp(t, e, m, d.addr, proto.NoAuthCode)
	setHelpRefreshWait(t, time.Hour)
	e.hold() // 后台刷新挂住：要是为找不到前缀同步等它，5 秒内回不来
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, d.addr, true)
	wantUntouched(t, n, "前缀碰巧相同的节点")
}

// 按前缀找到了节点就不等 CF；列表 6 小时没确认过时在后台刷新（spec 6.2）。
func TestHelpRoutedDoesNotWaitForRefresh(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	e.setNow(dirtest.Epoch.Add(7 * time.Hour))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	setHelpRefreshWait(t, time.Hour)
	e.hold()
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, w.addr, true)
	waitFor(t, "后台刷新列表", func() bool { return e.hitCount() == 1 })
}

// 找不到前缀时为 CF 等有上限：CF 挂住也只等 helpRefreshWait，然后连默认节点。
func TestHelpUnknownPrefixWaitIsBounded(t *testing.T) {
	d := startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100))
	e.setNow(dirtest.Epoch.Add(7 * time.Hour))
	h := nodeHelp(t, e, m, deadAddr(t), "W23456789ABC")
	setHelpRefreshWait(t, 50*time.Millisecond)
	e.hold()
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, d.addr, true)
	e.wantLogs(1, "没有这个协助码所属的节点")
}

// P2P 的 STUN（也是 UDP 中继）跟着实际连上的节点走，不按默认地址推导；指定了 --stun 时照用。显式模式不变。
func TestHelpSTUNFollowsNode(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	if got, want := h.p2pSTUN(), RelaySTUNAddr(w.addr); got != want {
		t.Fatalf("自动模式的 STUN = %q，应为所连节点的 %q", got, want)
	}
	h.client.config.STUNServer = "192.0.2.1:3478"
	if got := h.p2pSTUN(); got != "192.0.2.1:3478" {
		t.Fatalf("指定了 STUN 却没用它: %q", got)
	}
	if got := NewHelpModeMCP(&Config{ServerAddr: w.addr}, "W23456789ABC").p2pSTUN(); got != "" {
		t.Fatalf("显式模式没指定 STUN 时应为空（与原来一样），得到 %q", got)
	}
}

// captureHelpSTUN 记下 help 建 P2P manager 时传的 STUN 地址。
func captureHelpSTUN(t *testing.T) <-chan string {
	t.Helper()
	ch := make(chan string, 4)
	orig := newHelpP2PManager
	newHelpP2PManager = func(mode p2p.P2PMode, stun, bindIP string) *p2p.P2PManager {
		ch <- stun
		return orig(mode, stun, bindIP)
	}
	t.Cleanup(func() { newHelpP2PManager = orig })
	return ch
}

// 命令行 help 与 MCP 的 P2P 都用跟着节点走的 STUN。--p2p required 加上绑不上的地址让 P2P 马上失败，不用真打洞。
func TestHelpP2PUsesNodeSTUN(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	p2pConfig := func() *Config {
		cfg := nodeHelpConfig(t, d.addr)
		cfg.P2PMode, cfg.BindIP = "required", "192.0.2.1"
		return cfg
	}

	t.Run("命令行", func(t *testing.T) {
		stun := captureHelpSTUN(t)
		h := NewHelpMode(p2pConfig(), "W23456789ABC", "127.0.0.1:0")
		h.useNodes(testHelpNodes(t, e, m))
		t.Cleanup(h.client.Close)
		if err := within5s(t, "Run", h.Run); err == nil || !strings.Contains(err.Error(), "P2P") {
			t.Fatalf("P2P 应失败: %v", err)
		}
		if got := recv5s(t, "建 P2P manager", stun); got != RelaySTUNAddr(w.addr) {
			t.Fatalf("STUN = %q，应为所连节点的 %q", got, RelaySTUNAddr(w.addr))
		}
	})

	t.Run("MCP", func(t *testing.T) {
		stun := captureHelpSTUN(t)
		b := NewHelpMCPBootstrap(p2pConfig())
		b.nodes, b.stunFollowsRelay = testHelpNodes(t, e, m), true // 同 UseNodeList
		if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err == nil || !strings.Contains(err.Error(), "P2P required") {
			t.Fatalf("P2P 应失败: %v", err)
		}
		if got := recv5s(t, "建 P2P manager", stun); got != RelaySTUNAddr(w.addr) {
			t.Fatalf("STUN = %q，应为所连节点的 %q", got, RelaySTUNAddr(w.addr))
		}
	})
}

// 自动模式的进程（没指定 relay）里，cmd/remote 不按默认地址推导 STUN，STUN 由这里跟着实际连上的 relay：MCP connect 带了
// server 的、拿不到节点列表（拿不到家目录）而连默认地址的，也都要有 STUN，不能是空的（空的就没有 STUN 发现和 UDP 中继）。
func TestHelpSTUNFollowsRelayInAutoProcess(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	t.Setenv(directoryEnv, "off") // 不去 CF 取列表
	p2pConfig := func() *Config {
		cfg := nodeHelpConfig(t, d.addr)
		cfg.P2PMode, cfg.BindIP = "required", "192.0.2.1"
		return cfg
	}
	noHome := func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
	}

	t.Run("MCP connect 带 server", func(t *testing.T) {
		// 默认地址用不会去连的 relay.example.test，推出的 STUN 与 w 的不同，才看得出跟的是哪台。
		server := w.addr
		cfg := p2pConfig()
		cfg.ServerAddr = "relay.example.test:8443"
		stun := captureHelpSTUN(t)
		b := NewHelpMCPBootstrap(cfg)
		b.UseNodeList(t.TempDir())
		if b.nodes == nil {
			t.Fatal("应接上节点列表")
		}
		if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+server+`"}`); err == nil || !strings.Contains(err.Error(), "P2P required") {
			t.Fatalf("P2P 应失败: %v", err)
		}
		if got := recv5s(t, "建 P2P manager", stun); got != RelaySTUNAddr(server) {
			t.Fatalf("STUN = %q，应为所连 relay 的 %q", got, RelaySTUNAddr(server))
		}
	})

	t.Run("MCP 拿不到家目录", func(t *testing.T) {
		noHome(t)
		stun := captureHelpSTUN(t)
		b := NewHelpMCPBootstrap(p2pConfig())
		b.UseNodeList("")
		if b.nodes != nil {
			t.Fatal("拿不到家目录时不该有节点列表")
		}
		if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err == nil || !strings.Contains(err.Error(), "P2P required") {
			t.Fatalf("P2P 应失败: %v", err)
		}
		if got := recv5s(t, "建 P2P manager", stun); got != RelaySTUNAddr(d.addr) {
			t.Fatalf("STUN = %q，应为所连默认 relay 的 %q", got, RelaySTUNAddr(d.addr))
		}
	})

	t.Run("命令行拿不到家目录", func(t *testing.T) {
		noHome(t)
		h := NewHelpMode(p2pConfig(), "W23456789ABC", "127.0.0.1:0")
		h.UseNodeList("")
		if h.nodes != nil {
			t.Fatal("拿不到家目录时不该有节点列表")
		}
		if got := h.p2pSTUN(); got != RelaySTUNAddr(d.addr) {
			t.Fatalf("STUN = %q，应为所连默认 relay 的 %q", got, RelaySTUNAddr(d.addr))
		}
	})
}

// relay 在私网/本机时 auto 跳过 P2P（relay 已是 LAN 直连）；required 照做，disabled 本来就不做。
func TestLANSkipsP2P(t *testing.T) {
	for _, tc := range []struct {
		mode   p2p.P2PMode
		server string
		want   bool
	}{
		{p2p.P2PModeAuto, "127.0.0.1:8443", true},
		{p2p.P2PModeAuto, "localhost:8443", true},
		{p2p.P2PModeAuto, "relay.example.test:8443", false},
		{p2p.P2PModeAuto, "192.0.2.1:8443", false},
		{p2p.P2PModeRequired, "127.0.0.1:8443", false},
		{p2p.P2PModeDisabled, "127.0.0.1:8443", false},
	} {
		if got := lanSkipsP2P(tc.mode, tc.server); got != tc.want {
			t.Errorf("lanSkipsP2P(%v, %q) = %v，想要 %v", tc.mode, tc.server, got, tc.want)
		}
	}
}

// 命令行 help 自动模式下 P2P 的私网判断按连上的实际地址：默认地址是公网，但按码连到的节点在本机/私网（例如本机专用地址
// 是内网地址）时，auto 不做 P2P，与 MCP connect 一样。连接前（cmd/remote）只对显式模式判断，那时还不知道会连哪台。
func TestHelpSkipsP2POnLANNode(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	bye, err := proto.NewMessage(proto.MsgError, &proto.ErrorMessage{Message: "bye"})
	if err != nil {
		t.Fatal(err)
	}
	w.sendAfterJoin(bye) // 让 Run 在 relay 隧道上马上结束
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	stun := captureHelpSTUN(t)
	cfg := nodeHelpConfig(t, "relay.example.test:8443") // 默认地址是公网的；码属于 W，不会去连它
	cfg.P2PMode = "auto"
	h := NewHelpMode(cfg, "W23456789ABC", "127.0.0.1:0")
	h.useNodes(testHelpNodes(t, e, m))
	t.Cleanup(h.client.Close)
	if err := within5s(t, "Run", h.Run); err == nil || !strings.Contains(err.Error(), "bye") {
		t.Fatalf("应在 relay 隧道上收到 bye 后结束: %v", err)
	}
	wantJoins(t, w, "W", 1)
	wantUntouched(t, d, "默认节点 D")
	select {
	case s := <-stun:
		t.Fatalf("连到本机的节点时 auto 不该做 P2P，却建了 P2P manager（STUN %q）", s)
	default:
	}
}

// MCP 自动模式同样按连上的实际地址判断：默认地址是公网的，按码连到本机的节点时 auto 不做 P2P。P2P 在 connect 返回后
// 才在后台起，所以等一会儿看它有没有建 P2P manager。
func TestMCPSkipsP2POnLANNode(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	stun := captureHelpSTUN(t)
	b := nodeBootstrap(t, e, m, "relay.example.test:8443") // 码属于 W，不会去连默认地址
	b.cfg.P2PMode = "auto"
	res, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Server != w.addr {
		t.Fatalf("连上的地址 = %s，应为节点 W 的 %s", res.Server, w.addr)
	}
	wantUntouched(t, d, "默认节点 D")
	select {
	case s := <-stun:
		t.Fatalf("连到本机的节点时 auto 不该做 P2P，却建了 P2P manager（STUN %q）", s)
	case <-time.After(time.Second):
	}
}

// 本机专用地址连不上就从缓存删掉（按固定格式提示），改连列表地址；与 share 共用一套拨号处理。
func TestHelpDropsDeadLocalAddr(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	dead := deadAddr(t)
	e.setLocalAddr("W", dead)
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	wantAt(t, h, w.addr, true)
	e.wantLogs(1, "本机专用地址 "+dead+" 连接失败")
}

// 证书与列表不符的节点不加入（不把码交给它），打警告。
func TestHelpCertMismatchDoesNotJoin(t *testing.T) {
	d, w := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e := newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", liveNode{addr: w.addr, fp: strings.Repeat("cd", 32)}, directory.StatusActive, 100))
	h := nodeHelp(t, e, m, d.addr, "W23456789ABC")
	_, err := join5s(t, h)
	var ce *relayConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("应为连接失败: %v", err)
	}
	wantJoins(t, w, "证书不符的 W", 0)
	e.wantLogs(1, "可能被劫持")
}

func TestHelpUseNodeList(t *testing.T) {
	t.Setenv(directoryEnv, "off")
	home := t.TempDir()
	h := NewHelpModeMCP(&Config{ServerAddr: "relay.example.test:8443"}, "W23456789ABC")
	h.UseNodeList(home)
	if h.nodes == nil || h.nodes.dm.path != filepath.Join(home, directoryCacheFile) {
		t.Fatalf("没有按 home 建列表管理器: %+v", h.nodes)
	}
	if h.defaultAddr != "relay.example.test:8443" {
		t.Fatalf("默认地址 = %q", h.defaultAddr)
	}

	b := NewHelpMCPBootstrap(&Config{ServerAddr: "relay.example.test:8443"})
	b.UseNodeList(home)
	if b.nodes == nil || b.nodes.dm.path != filepath.Join(home, directoryCacheFile) {
		t.Fatalf("bootstrap 没有按 home 建列表管理器: %+v", b.nodes)
	}

	// 显式模式（没调 UseNodeList）不路由。
	if NewHelpModeMCP(&Config{}, "W23456789ABC").nodes != nil {
		t.Fatal("显式模式不该有列表")
	}
}

// --- MCP connect ---

// nodeBootstrap 返回一个自动模式的 MCP bootstrap（默认地址 defaultAddr）。
func nodeBootstrap(t *testing.T, e *dirEnv, m *dirManager, defaultAddr string) *HelpMCPBootstrap {
	t.Helper()
	b := NewHelpMCPBootstrap(nodeHelpConfig(t, defaultAddr))
	b.nodes, b.stunFollowsRelay = testHelpNodes(t, e, m), true // 同 UseNodeList
	t.Cleanup(func() {
		b.mu.Lock()
		h := b.help
		b.mu.Unlock()
		if h != nil {
			h.client.Close()
		}
	})
	return b
}

func mcpConnect(t *testing.T, b *HelpMCPBootstrap, args string) (connectResult, error) {
	t.Helper()
	ch := make(chan joinOutcome, 1)
	var raw json.RawMessage
	go func() {
		var err error
		raw, err = b.doConnect(context.Background(), json.RawMessage(args))
		ch <- joinOutcome{err: err}
	}()
	if err := recv5s(t, "connect", ch).err; err != nil {
		return connectResult{}, err
	}
	var res connectResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

func twoNodeBootstrap(t *testing.T) (d, w *joinRelay, e *dirEnv, b *HelpMCPBootstrap) {
	t.Helper()
	d, w = startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	e = newDirEnv(t)
	m := e.manager()
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	return d, w, e, nodeBootstrap(t, e, m, d.addr)
}

// connect 没带 server 时按码路由；结果与幂等判断记录路由后实际连上的地址（spec 6.4 第 5 条）。
func TestMCPConnectRoutesAndRecordsServer(t *testing.T) {
	d, w, _, b := twoNodeBootstrap(t)
	res, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Server != w.addr {
		t.Fatalf("结果里的地址 = %s，应为路由后的 %s", res.Server, w.addr)
	}
	b.mu.Lock()
	active := b.activeTarget
	b.mu.Unlock()
	if active.Server != w.addr || !active.Auto {
		t.Fatalf("活动会话 = %+v", active)
	}
	wantUntouched(t, d, "默认节点 D")
}

// 同一个码再 connect 复用健康会话：连上之前不知道会连哪台，按码认（不拿默认地址去比路由后的地址）。
func TestMCPAutoConnectIsIdempotent(t *testing.T) {
	_, w, _, b := twoNodeBootstrap(t)
	for i := range 2 {
		if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
	if n := w.accepted.Load(); n != 1 {
		t.Fatalf("W 被连了 %d 次：重复 connect 拆掉了健康会话", n)
	}
}

// 带上路由后实际连上的地址显式 connect，认得出是同一个会话。
func TestMCPExplicitServerReusesRoutedSession(t *testing.T) {
	_, w, _, b := twoNodeBootstrap(t)
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+w.addr+`"}`); err != nil {
		t.Fatal(err)
	}
	if n := w.accepted.Load(); n != 1 {
		t.Fatalf("W 被连了 %d 次：带实际地址的 connect 没认出同一个会话", n)
	}
}

// 带了 server 的 connect 是显式模式：只连它，不路由（地址是 D 的列表地址，证书按 D 严格比对，spec 6.1）。
// 之后不带 server 的是新意图，按码路由。
func TestMCPExplicitThenAutoConnect(t *testing.T) {
	d, w, _, b := twoNodeBootstrap(t)
	res, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC","server":"`+d.addr+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	onNode := b.help != nil && b.help.client.node != nil
	b.mu.Unlock()
	if res.Server != d.addr || !onNode {
		t.Fatalf("显式 connect 应只连 %s、按 D 严格比对证书，结果 %s（节点模式 %v）", d.addr, res.Server, onNode)
	}
	wantUntouched(t, w, "W")
	if res, err = mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err != nil {
		t.Fatal(err)
	}
	if res.Server != w.addr {
		t.Fatalf("不带 server 的 connect 应按码路由到 %s，结果 %s", w.addr, res.Server)
	}
}

// 自动重连沿用自动模式：重新路由（缓存里的 server 仍是调用方给的空值，不是路由后的地址）。
func TestMCPReconnectStaysAuto(t *testing.T) {
	_, w, _, b := twoNodeBootstrap(t)
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err != nil {
		t.Fatal(err)
	}
	w.dropConns() // 会话断了，自动重连才会真的去连
	if !waitSessionTornDown(t, b, 5*time.Second) {
		t.Fatal("W 断开了会话却没拆")
	}
	if err := within5s(t, "reconnect", func() error { return b.reconnect(context.Background()) }); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	active, last := b.activeTarget, b.lastServer
	b.mu.Unlock()
	if last != "" || !active.Auto || active.Server != w.addr {
		t.Fatalf("重连后 lastServer=%q 活动会话=%+v", last, active)
	}
	if n := w.accepted.Load(); n != 2 {
		t.Fatalf("W 被连了 %d 次，应为 2", n)
	}
}

// 会话断了以后同一个码重试失败，不能清掉自动重连要用的缓存：那是同一个意图。
func TestMCPAutoRetryKeepsCache(t *testing.T) {
	_, w, _, b := twoNodeBootstrap(t)
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err != nil {
		t.Fatal(err)
	}
	w.stop()
	if !waitSessionTornDown(t, b, 5*time.Second) {
		t.Fatal("W 停了会话却没拆")
	}
	if _, err := mcpConnect(t, b, `{"code":"W234-5678-9ABC"}`); err == nil || !strings.Contains(err.Error(), "relay connect failed") {
		t.Fatalf("W 停了应连不上: %v", err)
	}
	b.mu.Lock()
	code := b.lastCode
	b.mu.Unlock()
	if code != "W234-5678-9ABC" {
		t.Fatalf("同一个意图重试失败后缓存被清了: lastCode=%q", code)
	}
}

func TestConnectTargetSameEndpointAuto(t *testing.T) {
	auto := connectTarget{Code: "W23456789ABC", Server: "192.0.2.1:8443", Auto: true}
	cases := []struct {
		name   string
		active connectTarget
		next   connectTarget
		same   bool
	}{
		{"自动对自动，不比地址", auto, connectTarget{Code: "w23456789abc", Auto: true}, true},
		{"自动对自动，码不同", auto, connectTarget{Code: "W23456789ABD", Auto: true}, false},
		{"显式地址等于路由后的地址", auto, connectTarget{Code: "W23456789ABC", Server: "192.0.2.1:8443"}, true},
		{"显式地址不同", auto, connectTarget{Code: "W23456789ABC", Server: "192.0.2.2:8443"}, false},
		{"现有会话是显式的，这次是自动", connectTarget{Code: "W23456789ABC", Server: "192.0.2.1:8443"}, connectTarget{Code: "W23456789ABC", Auto: true}, false},
		{"no_auth 不同", auto, connectTarget{Code: "W23456789ABC", Auto: true, NoAuth: true}, false},
	}
	for _, c := range cases {
		if got := c.active.sameEndpoint(c.next); got != c.same {
			t.Errorf("%s: sameEndpoint = %v，应为 %v", c.name, got, c.same)
		}
	}
}
