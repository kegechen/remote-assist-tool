package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/relay"
)

// P2P 直连跨 relay 断连存活的端到端用例：真实 relay（明文 TCP，本机回环）、真实 ShareMode
// 主循环、真实 HelpMCPBootstrap。只有打洞被替换成直接给两端一对回环上的 UDP 隧道（直连）。
// 两端到 relay 的 TCP 各经一个可切断的转发（cutProxy），用来模拟某一端的网络中断。

// 续会话的时间参数在包内测试里统一调小。放在 init 里而不是在各测试里改了再还原：resumeRelay
// goroutine 在会话拆除后才退出，收尾时还原会与它的读并发，-race 下就是数据竞争。
func init() {
	resumeFirstDelay = 50 * time.Millisecond
	resumeMaxDelay = time.Second
	resumeWindow = 20 * time.Second
	resumeAckTimeout = 3 * time.Second
	resumeProbeTimeout = 2 * time.Second
	// 被拒后的最短间隔要盖得住被协助端重连的退避（测试里没调小，前几次是 1s、2s、4s），
	// 否则 relay 重启后协助端会赶在被协助端回来之前把被拒次数用完。
	resumeRejectedMinDelay = 2 * time.Second
}

// loopbackP2P 代替打洞：share 与 help 各 Start 一个 manager，两边凑齐就各拿到一对回环 UDP
// 隧道的一端。
type loopbackP2P struct {
	t       *testing.T
	mu      sync.Mutex
	waiting map[bool]*loopbackMgr // 按 isShare 分，等另一端
	pairs   int
	raw     map[*p2p.UDPTunnel]*net.UDPConn // 隧道底下的 socket，见 killSilently
}

func newLoopbackP2P(t *testing.T) *loopbackP2P {
	b := &loopbackP2P{t: t, waiting: map[bool]*loopbackMgr{}, raw: map[*p2p.UDPTunnel]*net.UDPConn{}}
	t.Cleanup(func() {
		b.mu.Lock()
		var tunnels []*p2p.UDPTunnel
		for tn := range b.raw {
			tunnels = append(tunnels, tn)
		}
		b.mu.Unlock()
		for _, tn := range tunnels {
			tn.Close()
		}
	})
	return b
}

func (b *loopbackP2P) factory(p2p.P2PMode, string, string) shareP2PManager {
	return &loopbackMgr{b: b, results: make(chan p2p.P2PResult, 1)}
}

func (b *loopbackP2P) pairCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pairs
}

// killSilently 模拟这一端的进程直接退出：底层 socket 没了，隧道来不及给对端发关闭通知。
func (b *loopbackP2P) killSilently(tn *p2p.UDPTunnel) {
	b.mu.Lock()
	c := b.raw[tn]
	b.mu.Unlock()
	c.Close()
}

type loopbackMgr struct {
	b         *loopbackP2P
	isShare   bool
	results   chan p2p.P2PResult
	closeOnce sync.Once
}

func (*loopbackMgr) SetRelayConn(p2p.RelayConn)               {}
func (*loopbackMgr) SetAuthCode(string)                       {}
func (*loopbackMgr) SetSTUNCandidates([]string)               {}
func (*loopbackMgr) HandlePeerAddrReady(*proto.PeerAddrReady) {}

func (m *loopbackMgr) Start(_ string, isShare bool) (<-chan p2p.P2PResult, error) {
	b := m.b
	b.mu.Lock()
	defer b.mu.Unlock()
	m.isShare = isShare
	peer := b.waiting[!isShare]
	if peer == nil {
		b.waiting[isShare] = m
		return m.results, nil
	}
	delete(b.waiting, !isShare)
	// 不能调 t.Fatal：Start 跑在被测代码的 goroutine 里。
	c1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.t.Errorf("建回环隧道: %v", err)
		return nil, err
	}
	c2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		c1.Close()
		b.t.Errorf("建回环隧道: %v", err)
		return nil, err
	}
	mine := p2p.NewUDPTunnel(c1, c2.LocalAddr().(*net.UDPAddr))
	theirs := p2p.NewUDPTunnel(c2, c1.LocalAddr().(*net.UDPAddr))
	b.raw[mine], b.raw[theirs] = c1, c2
	b.pairs++
	m.results <- p2p.P2PResult{Tunnel: mine}
	peer.results <- p2p.P2PResult{Tunnel: theirs}
	return m.results, nil
}

func (m *loopbackMgr) Close() {
	m.closeOnce.Do(func() {
		b := m.b
		b.mu.Lock()
		if b.waiting[m.isShare] == m {
			delete(b.waiting, m.isShare)
		}
		b.mu.Unlock()
	})
}

// cutProxy 夹在一端与 relay 之间的 TCP 转发。cut 断开现有连接（两侧都收到 FIN，与网络中断
// 时 relay 读到 EOF 一致）并拒绝新连接，restore 恢复。
type cutProxy struct {
	ln     net.Listener
	target string
	// toRelay 非 nil 时，发往 relay 的每条消息先经它改写（见 asLegacyRelay）。
	toRelay func(*proto.Message)
	mu      sync.Mutex
	cutOff  bool
	conns   map[net.Conn]struct{}
}

func newCutProxy(t *testing.T, target string, toRelay func(*proto.Message)) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target, toRelay: toRelay, conns: map[net.Conn]struct{}{}}
	go p.serve()
	t.Cleanup(func() {
		ln.Close()
		p.cut()
	})
	return p
}

func (p *cutProxy) addr() string { return p.ln.Addr().String() }

func (p *cutProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		off := p.cutOff
		p.mu.Unlock()
		if off {
			c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		if p.cutOff {
			p.mu.Unlock()
			c.Close()
			up.Close()
			continue
		}
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go p.forward(up, c, p.toRelay)
		go p.forward(c, up, nil)
	}
}

func (p *cutProxy) forward(dst, src net.Conn, rewrite func(*proto.Message)) {
	if rewrite == nil {
		io.Copy(dst, src)
	} else {
		dec, enc := json.NewDecoder(src), json.NewEncoder(dst)
		for {
			var msg proto.Message
			if dec.Decode(&msg) != nil {
				break
			}
			rewrite(&msg)
			if enc.Encode(&msg) != nil {
				break
			}
		}
	}
	dst.Close()
	src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

func (p *cutProxy) cut() {
	p.mu.Lock()
	p.cutOff = true
	conns := p.conns
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

func (p *cutProxy) restore() {
	p.mu.Lock()
	p.cutOff = false
	p.mu.Unlock()
}

// asLegacyRelay 把发往 relay 的消息改成旧版 relay 眼里的样子：它不认识的新字段等于没有。
// 旧 relay 因此不会保留协助端配对，也不会转发续会话证明。
func asLegacyRelay(msg *proto.Message) {
	var field string
	switch msg.Type {
	case proto.MsgRegisterRequest:
		field = "keep_help"
	case proto.MsgJoinRequest:
		field = "resume_proof"
	default:
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(msg.Payload, &fields) != nil {
		return
	}
	delete(fields, field)
	msg.Payload, _ = json.Marshal(fields)
}

// runRelay 在 addr 起明文 relay，返回停止函数。不挂 t.Cleanup：停机顺序由 surviveEnv.close 管。
func runRelay(addr string, cfg relay.Config) (func(), error) {
	cfg.ListenAddr = addr
	cfg.CodeTTL = time.Minute
	cfg.CodeLength = 10
	srv, err := relay.NewServer(&cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- srv.StartWithContextReady(ctx, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		return nil, err
	case <-time.After(5 * time.Second):
		cancel()
		return nil, errors.New("relay 启动超时")
	}
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}, nil
}

type surviveEnv struct {
	t         *testing.T
	relayAddr string
	relayCfg  relay.Config
	relayStop func() // nil 表示 relay 正停着
	legacy    bool   // 两端看到的是旧版 relay（见 asLegacyRelay）
	shareMode string // 被协助端的 --p2p 模式
	p2p       *loopbackP2P
	dir       string // stat 的目标
	share     *ShareMode
	shareVia  *cutProxy
	code      string
	proxies   []*cutProxy
	helps     []*HelpMCPBootstrap
	stopLoop  func()
}

type surviveOpt func(*surviveEnv)

// legacyRelay 让两端连的是一台旧版 relay。
func legacyRelay(e *surviveEnv) { e.legacy = true }

// requiredP2P 被协助端用 --p2p required。
func requiredP2P(e *surviveEnv) { e.shareMode = "required" }

func newSurviveEnv(t *testing.T, opts ...surviveOpt) *surviveEnv {
	t.Helper()
	stubClipboard(t)
	e := &surviveEnv{
		t:         t,
		relayAddr: freePort(t),
		// StateDir：relay 重启后 share 凭复用令牌拿回原码，协助端才能用同一个码重新加入。
		relayCfg:  relay.Config{StateDir: t.TempDir(), DisableSourceIPLimits: true},
		p2p:       newLoopbackP2P(t),
		dir:       t.TempDir(),
		shareMode: "auto",
	}
	for _, o := range opts {
		o(e)
	}
	e.startRelay()
	e.shareVia = e.newProxy()
	codeFile := filepath.Join(t.TempDir(), "code.json")
	e.share = newTestShare(e.shareVia.addr(), codeFile)
	e.share.client.config.P2PMode = e.shareMode
	e.share.newP2PManager = e.p2p.factory
	e.startShareLoop()
	t.Cleanup(e.close)
	e.code = waitForCodeFile(t, codeFile)
	return e
}

func (e *surviveEnv) startRelay() {
	e.t.Helper()
	stop, err := runRelay(e.relayAddr, e.relayCfg)
	if err != nil {
		e.t.Fatalf("relay 启动失败: %v", err)
	}
	e.relayStop = stop
}

func (e *surviveEnv) stopRelay() {
	e.relayStop()
	e.relayStop = nil
}

func (e *surviveEnv) newProxy() *cutProxy { return e.newProxyWith(nil) }

// newProxyWith 同 newProxy，另把发往 relay 的每条消息先交给 observe 看一眼。
func (e *surviveEnv) newProxyWith(observe func(*proto.Message)) *cutProxy {
	rewrite := observe
	if e.legacy {
		rewrite = func(msg *proto.Message) {
			if observe != nil {
				observe(msg)
			}
			asLegacyRelay(msg)
		}
	}
	p := newCutProxy(e.t, e.relayAddr, rewrite)
	e.proxies = append(e.proxies, p)
	return p
}

// startShareLoop 跑 share 的会话主循环（同 Run，只是能在测试收尾时停下）。
func (e *surviveEnv) startShareLoop() {
	s := e.share
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.reconnectWithBackoff()
		rapidFails := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			rapidFails = s.serveRound(rapidFails)
		}
	}()
	// 主循环只在两轮之间看停止信号：关掉 relay 连接把它从等待里叫醒，它重连成功后就会退出。
	// 只关一次再等：重连的退避间隔是整秒，按固定周期反复关会一次次正好撞上重连里的注册，
	// 把退避一路拖到上限。等不到再补关一次兜底（退避封顶 30s，45s 足够一轮重连）。
	e.stopLoop = func() {
		close(stop)
		for i := 0; i < 2; i++ {
			s.client.Close()
			select {
			case <-done:
				s.client.Close() // 主循环可能刚重连上就退出了，别留着连接和心跳
				return
			case <-time.After(45 * time.Second):
			}
		}
		e.t.Error("share 主循环未退出")
	}
}

// close 停下两端和 relay。share 主循环在 relay 不在时卡在无限重连里出不来，所以 relay 停着
// 的话先拉起来，等两端停了再停它。
func (e *surviveEnv) close() {
	for _, p := range e.proxies {
		p.restore()
	}
	for _, b := range e.helps {
		if l := activeLink(b); l != nil {
			l.teardown(errors.New("test done"))
		}
	}
	if e.relayStop == nil {
		if stop, err := runRelay(e.relayAddr, e.relayCfg); err != nil {
			e.t.Errorf("收尾时拉起 relay 失败: %v", err)
		} else {
			e.relayStop = stop
		}
	}
	e.stopLoop()
	if e.relayStop != nil {
		e.relayStop()
	}
}

func waitForCodeFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			var v codeFileJSON
			if json.Unmarshal(raw, &v) == nil && v.Code != "" {
				return v.Code
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("share 10 秒内没有注册成功")
	return ""
}

func (e *surviveEnv) connectArgs() json.RawMessage {
	args, _ := json.Marshal(map[string]string{"code": e.code})
	return args
}

// tryConnectHelp 起一个协助端经 via 连 relay 并 connect。P2P 走 auto 模式（绕开「私网 relay
// 跳过 P2P」）。
func (e *surviveEnv) tryConnectHelp(via *cutProxy) (*HelpMCPBootstrap, error) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: via.addr(), Transport: "tcp", P2PMode: "auto"})
	b.newP2PManager = e.p2p.factory
	b.lanServer = func(string) bool { return false }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.doConnect(ctx, e.connectArgs()); err != nil {
		return nil, err
	}
	e.helps = append(e.helps, b)
	return b, nil
}

// connectHelpOnP2P 连上协助端并等工具流量升级到 P2P。
func (e *surviveEnv) connectHelpOnP2P(via *cutProxy) *HelpMCPBootstrap {
	e.t.Helper()
	b, err := e.tryConnectHelp(via)
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	e.waitHelpOnP2P(b)
	return b
}

func (e *surviveEnv) waitHelpOnP2P(b *HelpMCPBootstrap) {
	e.t.Helper()
	waitUntil(e.t, 10*time.Second, "协助端升级到 P2P", func() bool { return onP2P(b) })
	waitUntil(e.t, 5*time.Second, "被协助端在 P2P 上服务工具", func() bool {
		pc, _, _ := e.shareP2P()
		return pc != nil
	})
}

func onP2P(b *HelpMCPBootstrap) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeResult.P2P
}

// shareP2P 被协助端当前的工具隧道连接、协助端标识，以及是否在等协助端续会话。
func (e *surviveEnv) shareP2P() (*P2PConn, string, bool) {
	s := e.share
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	return s.p2pToolConn, s.p2pHelpTag, s.p2pAwaitingResume
}

func (e *surviveEnv) waitShareAwaiting(budget time.Duration, what string, want bool) {
	e.t.Helper()
	waitUntil(e.t, budget, what, func() bool {
		_, _, awaiting := e.shareP2P()
		return awaiting == want
	})
}

// stat 经协助端调一次工具。
func (e *surviveEnv) stat(b *HelpMCPBootstrap) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]string{"path": e.dir})
	_, err := b.CallTool(ctx, "stat", args)
	return err
}

func (e *surviveEnv) mustStat(b *HelpMCPBootstrap, when string) {
	e.t.Helper()
	if err := e.stat(b); err != nil {
		e.t.Fatalf("%s工具调用失败: %v", when, err)
	}
}

func activeLink(b *HelpMCPBootstrap) *helpLink {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.link
}

func activeSessionID(b *HelpMCPBootstrap) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeResult.SessionID
}

// helpRelayConn 协助端当前的 relay 连接；用来判断它有没有重新加入过 relay。
func helpRelayConn(b *HelpMCPBootstrap) net.Conn {
	b.mu.Lock()
	h := b.help
	b.mu.Unlock()
	if h == nil {
		return nil
	}
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	return h.client.conn
}

// shareRelayConn 被协助端当前的 relay 连接；用来判断它有没有断开重连过。
func (e *surviveEnv) shareRelayConn() net.Conn {
	c := e.share.client
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

// waitLogged 等 fl 的状态输出里出现 want。
func waitLogged(t *testing.T, fl *fakeLog, want string) {
	t.Helper()
	waitUntil(t, 5*time.Second, "日志文件里写进「"+want+"」", func() bool { return strings.Contains(fl.text(), want) })
}

func waitUntil(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%v 内没等到：%s", budget, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 场景 a：relay 重启。两端都读到 EOF；P2P 直连照常承载工具调用，relay 回来后协助端带着续会话
// 证明重新加入、被协助端校验通过，沿用原隧道，不重新协商。
func TestP2PSurvivesRelayRestart(t *testing.T) {
	e := newSurviveEnv(t)
	help := e.connectHelpOnP2P(e.newProxy())
	pc0, _, _ := e.shareP2P()
	link0, sid0, pairs0 := activeLink(help), activeSessionID(help), e.p2p.pairCount()
	e.mustStat(help, "初始")

	e.stopRelay()
	for _, p := range e.proxies {
		p.cut()
	}
	waitUntil(t, 5*time.Second, "协助端进入续会话", func() bool { return link0.resuming.Load() })
	e.waitShareAwaiting(5*time.Second, "被协助端保留 P2P 隧道", true)
	e.mustStat(help, "relay 停机期间")

	// 续会话期间同参数再 connect：会话还能用（工具走 P2P），应原样复用。拆掉重连的话，relay
	// 这时还没回来，必然失败，会话也就没了。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := help.doConnect(ctx, e.connectArgs())
	cancel()
	if err != nil {
		t.Fatalf("续会话期间重复 connect 失败: %v", err)
	}
	if activeLink(help) != link0 {
		t.Fatal("续会话期间重复 connect 不应拆掉会话")
	}

	e.startRelay()
	for _, p := range e.proxies {
		p.restore()
	}
	waitUntil(t, 15*time.Second, "协助端续上会话", func() bool {
		return !link0.resuming.Load() && activeSessionID(help) != sid0
	})
	e.mustStat(help, "relay 重启后")

	if activeLink(help) != link0 {
		t.Fatal("协助端会话被拆过")
	}
	if pc, _, awaiting := e.shareP2P(); pc != pc0 || awaiting {
		t.Fatalf("被协助端应沿用原隧道且不再等续会话: 同一隧道=%v awaiting=%v", pc == pc0, awaiting)
	}
	if n := e.p2p.pairCount(); n != pairs0 {
		t.Fatalf("不应重新协商 P2P: %d → %d 次", pairs0, n)
	}
}

// 场景 b：只有被协助端到 relay 的连接断了（relay 在、P2P 通）。协助端收到 PEER_DISCONNECTED 后
// 经 P2P 探活确认对端还在，连接留着；被协助端重连时请 relay 保留配对，协助端不必重新加入。
func TestP2PSurvivesShareRelayFlap(t *testing.T) {
	e := newSurviveEnv(t)
	help := e.connectHelpOnP2P(e.newProxy())
	pc0, _, _ := e.shareP2P()
	link0, conn0, pairs0 := activeLink(help), helpRelayConn(help), e.p2p.pairCount()
	fl := &fakeLog{} // 续会话的提示也写进日志文件（spec 6.8）
	useLogFile(t, fl)

	e.shareVia.cut()
	e.waitShareAwaiting(5*time.Second, "被协助端保留 P2P 隧道", true)
	e.mustStat(help, "被协助端离开 relay 期间")
	waitLogged(t, fl, "被协助端与中转的连接已断开，P2P 直连仍通")

	e.shareVia.restore()
	e.waitShareAwaiting(15*time.Second, "被协助端重新注册并接着用 P2P", false)
	e.mustStat(help, "被协助端回到 relay 后")

	if activeLink(help) != link0 {
		t.Fatal("协助端会话被拆过")
	}
	if helpRelayConn(help) != conn0 {
		t.Fatal("relay 应保留配对，协助端不应被要求重新加入")
	}
	if pc, _, _ := e.shareP2P(); pc != pc0 {
		t.Fatal("被协助端应沿用原隧道")
	}
	if n := e.p2p.pairCount(); n != pairs0 {
		t.Fatalf("不应重新协商 P2P: %d → %d 次", pairs0, n)
	}

	// 保留下来的配对在 relay 上是真的：P2P 断了，工具流量能回落到中转（协助端重新握手，
	// 被协助端把响应出口换回 relay）。
	pc0.Close()
	waitUntil(t, 20*time.Second, "协助端切回中转", func() bool { return !onP2P(help) })
	e.mustStat(help, "P2P 断开回落中转后")
}

// 场景 c：只有协助端到 relay 的连接断了，并且在 relay 的断连去抖（5 秒）内回来。协助端凭续会话
// 证明重新加入，被协助端校验通过后沿用隧道；之后被协助端再闪断，请 relay 保留的是新连接。
func TestP2PSurvivesHelpRelayFlap(t *testing.T) {
	e := newSurviveEnv(t)
	helpVia := e.newProxy()
	help := e.connectHelpOnP2P(helpVia)
	pc0, tag0, _ := e.shareP2P()
	link0, pairs0 := activeLink(help), e.p2p.pairCount()
	fl := &fakeLog{} // 续会话的提示也写进日志文件（spec 6.8）
	useLogFile(t, fl)

	helpVia.cut()
	waitUntil(t, 5*time.Second, "协助端进入续会话", func() bool { return link0.resuming.Load() })
	e.mustStat(help, "协助端离开 relay 期间")

	helpVia.restore()
	waitUntil(t, 10*time.Second, "协助端续上会话", func() bool { return !link0.resuming.Load() })
	e.mustStat(help, "协助端回到 relay 后")
	waitLogged(t, fl, "P2P 直连仍通，正在重新接入中转")
	waitLogged(t, fl, "已重新接入中转（被协助端已确认续会话）")
	assertSameP2P(t, e, help, link0, pc0, pairs0)
	_, tag1, _ := e.shareP2P()
	if tag1 == "" || tag1 == tag0 {
		t.Fatalf("续会话后被协助端应改认协助端的新连接: %q → %q", tag0, tag1)
	}

	conn1 := helpRelayConn(help)
	e.shareVia.cut()
	e.waitShareAwaiting(5*time.Second, "被协助端保留 P2P 隧道", true)
	e.shareVia.restore()
	e.waitShareAwaiting(15*time.Second, "被协助端重新注册", false)
	if helpRelayConn(help) != conn1 {
		t.Fatal("relay 应保留与协助端新连接的配对")
	}
	e.mustStat(help, "被协助端闪断后")
}

// 场景 c'：协助端离开 relay 超过断连去抖，被协助端先收到 PEER_DISCONNECTED。P2P 仍通，它不
// 收尾，等协助端带证明重新加入。
func TestP2PSurvivesHelpRelayOutageBeyondDebounce(t *testing.T) {
	if testing.Short() {
		t.Skip("要等 relay 的 5 秒断连去抖")
	}
	e := newSurviveEnv(t)
	helpVia := e.newProxy()
	help := e.connectHelpOnP2P(helpVia)
	pc0, _, _ := e.shareP2P()
	link0, pairs0 := activeLink(help), e.p2p.pairCount()

	helpVia.cut()
	e.waitShareAwaiting(10*time.Second, "被协助端收到协助端断开并保留隧道", true)
	e.mustStat(help, "协助端离开 relay 期间")

	helpVia.restore()
	waitUntil(t, 10*time.Second, "协助端续上会话", func() bool {
		_, _, awaiting := e.shareP2P()
		return !link0.resuming.Load() && !awaiting
	})
	e.mustStat(help, "协助端回到 relay 后")
	assertSameP2P(t, e, help, link0, pc0, pairs0)
}

// 场景 d：协助端 A 的 relay 连接断了（P2P 还通），5 秒去抖内另一个协助端 B 用同一个码加入。
// B 没有 A 的会话密钥，给不出续会话证明：被协助端必须撤掉 A 的隧道、为 B 重新协商。
func TestP2PRevokedWhenAnotherHelperJoins(t *testing.T) {
	e := newSurviveEnv(t)
	viaA := e.newProxy()
	a := e.connectHelpOnP2P(viaA)
	pcA, tagA, _ := e.shareP2P()
	pairs0 := e.p2p.pairCount()

	viaA.cut()
	var b *HelpMCPBootstrap
	waitUntil(t, 5*time.Second, "B 加入（A 进入去抖后才让位）", func() bool {
		var err error
		b, err = e.tryConnectHelp(e.newProxy())
		return err == nil
	})

	if !waitSessionTornDown(t, a, 5*time.Second) {
		t.Fatal("A 的隧道被撤掉后，A 的会话应结束")
	}
	if !pcA.tunnel.Closed() {
		t.Fatal("A 的隧道应被关闭")
	}
	e.waitHelpOnP2P(b)
	e.mustStat(b, "B ")
	if err := e.stat(a); err == nil {
		t.Fatal("A 不应还能调用工具")
	}
	pcB, tagB, _ := e.shareP2P()
	if pcB == pcA {
		t.Fatal("B 不能沿用 A 的隧道")
	}
	if tagB == "" || tagB == tagA {
		t.Fatalf("被协助端应改认 B: %q → %q", tagA, tagB)
	}
	if n := e.p2p.pairCount(); n != pairs0+1 {
		t.Fatalf("应为 B 重新协商一次 P2P: %d → %d 次", pairs0, n)
	}
}

// 场景 d 的变体：第三方拿到了协助码，还附上一份形状完全合法的续会话证明（用别的会话密钥算的，
// relay 只查形状，会照转）。被协助端校验不过，必须按换人处理：撤掉 A 的隧道，不能当成 A 续会话。
func TestP2PRevokedOnForgedResumeProof(t *testing.T) {
	e := newSurviveEnv(t)
	viaA := e.newProxy()
	a := e.connectHelpOnP2P(viaA)
	pcA, tagA, _ := e.shareP2P()
	linkA := activeLink(a)

	viaA.cut()
	waitUntil(t, 5*time.Second, "A 进入续会话", func() bool { return linkA.resuming.Load() })

	var other proto.Session
	for i := range other.Key {
		other.Key[i] = 0x5a
	}
	other.Version = proto.ToolProtocolVersion
	forged, err := proto.NewP2PResumeProof(other, e.code)
	if err != nil {
		t.Fatal(err)
	}
	m := NewHelpModeMCP(&Config{ServerAddr: e.newProxy().addr(), Transport: "tcp"}, e.code)
	defer m.client.Close()
	waitUntil(t, 5*time.Second, "第三方加入（A 进入去抖后才让位）", func() bool {
		if m.client.Connect() != nil {
			return false
		}
		if _, err := m.joinWithProof(forged); err != nil {
			m.client.Close()
			return false
		}
		return true
	})

	waitUntil(t, 5*time.Second, "被协助端撤掉 A 的隧道", pcA.tunnel.Closed)
	if !waitSessionTornDown(t, a, 5*time.Second) {
		t.Fatal("A 的隧道被撤掉后，A 的会话应结束")
	}
	pc, tag, awaiting := e.shareP2P()
	if pc == pcA || awaiting {
		t.Fatalf("被协助端不应再为 A 保留隧道: 同一隧道=%v awaiting=%v", pc == pcA, awaiting)
	}
	if tag == tagA {
		t.Fatal("被协助端应改认新加入的协助端")
	}
}

// 被协助端一直不在 relay 上，协助端的 relay 连接又断了：重新加入只会被 relay 拒绝，而每次被拒
// relay 都按来源 IP 计分。协助端必须在有限次被拒后放弃续会话，不能在整个续会话时限里反复撞。
func TestHelpStopsRejoiningAfterRepeatedRejections(t *testing.T) {
	e := newSurviveEnv(t)
	var joins atomic.Int32
	helpVia := e.newProxyWith(func(msg *proto.Message) {
		if msg.Type == proto.MsgJoinRequest {
			joins.Add(1)
		}
	})
	help := e.connectHelpOnP2P(helpVia)
	link0 := activeLink(help)

	e.shareVia.cut()
	e.waitShareAwaiting(5*time.Second, "被协助端保留 P2P 隧道", true)
	e.mustStat(help, "被协助端离开 relay 期间")

	helpVia.cut()
	waitUntil(t, 5*time.Second, "协助端进入续会话", func() bool { return link0.resuming.Load() })
	helpVia.restore()
	if !waitSessionTornDown(t, help, 15*time.Second) {
		t.Fatal("被协助端不在 relay 上，协助端应在有限次被拒后放弃续会话")
	}
	rejoins := int(joins.Load()) - 1 // 去掉最初那次 connect
	if rejoins < 1 || rejoins > resumeMaxRejectedJoins {
		t.Fatalf("续会话期间应有 1..%d 次加入，实际 %d 次", resumeMaxRejectedJoins, rejoins)
	}
}

// --p2p required：被协助端保留着隧道等续会话时隧道断了，这一轮就失败了，必须和其他 P2P 失败
// 一样断开 relay 重来，不能留在中转上接着等。
func TestRequiredModeDropsRelayWhenHeldP2PDies(t *testing.T) {
	e := newSurviveEnv(t, requiredP2P)
	helpVia := e.newProxy()
	help := e.connectHelpOnP2P(helpVia)
	link0 := activeLink(help)
	pc0, _, _ := e.shareP2P()

	// 协助端先离开 relay（续会话进行中），被协助端再闪断：它回来时协助端不在，relay 保留不了
	// 配对，它在新连接上等协助端带证明重新加入。
	helpVia.cut()
	waitUntil(t, 5*time.Second, "协助端进入续会话", func() bool { return link0.resuming.Load() })
	conn0 := e.shareRelayConn()
	e.shareVia.cut()
	e.waitShareAwaiting(5*time.Second, "被协助端保留 P2P 隧道", true)
	e.shareVia.restore()
	var conn1 net.Conn
	waitUntil(t, 15*time.Second, "被协助端重新注册", func() bool {
		conn1 = e.shareRelayConn()
		return conn1 != nil && conn1 != conn0
	})
	if _, _, awaiting := e.shareP2P(); !awaiting {
		t.Fatal("协助端不在 relay 上，被协助端应仍在等续会话")
	}

	pc0.Close()
	waitUntil(t, 5*time.Second, "被协助端断开 relay 重来", func() bool { return e.shareRelayConn() != conn1 })
}

// 被协助端进程直接退出（不会发 P2P 关闭通知）：协助端收到 PEER_DISCONNECTED 后经 P2P 探活
// 没有回应，必须马上拆会话，不能让调用干等隧道 60 秒的超时。
func TestHelpTearsDownWhenPeerExitsSilently(t *testing.T) {
	e := newSurviveEnv(t)
	help := e.connectHelpOnP2P(e.newProxy())
	pc0, _, _ := e.shareP2P()

	e.p2p.killSilently(pc0.tunnel)
	e.shareVia.cut()
	if !waitSessionTornDown(t, help, 8*time.Second) {
		t.Fatal("对端进程已退出、P2P 探活无回应，协助端会话应拆掉")
	}
}

// 兼容：relay 是旧版（不认识 KeepHelp、不转发续会话证明）。P2P 保不住，但要干净地退回旧行为：
// 协助端会话拆掉（不悬着），被协助端撤掉旧隧道、照常服务，重新 connect 后一切如常。
func TestP2PFallsBackCleanlyOnLegacyRelay(t *testing.T) {
	e := newSurviveEnv(t, legacyRelay)
	help := e.connectHelpOnP2P(e.newProxy())
	pc0, _, _ := e.shareP2P()

	e.shareVia.cut()
	e.waitShareAwaiting(5*time.Second, "被协助端保留 P2P 隧道", true)
	e.shareVia.restore()
	if !waitSessionTornDown(t, help, 15*time.Second) {
		t.Fatal("旧 relay 上续不了会话，协助端会话应拆掉而不是悬着")
	}
	waitUntil(t, 5*time.Second, "被协助端撤掉旧隧道", pc0.tunnel.Closed)

	// relay 处理完旧连接断开之前，新的 join 会撞上「已有协助端」，重试到它让位为止。
	var again *HelpMCPBootstrap
	waitUntil(t, 5*time.Second, "重新 connect", func() bool {
		var err error
		again, err = e.tryConnectHelp(e.newProxy())
		return err == nil
	})
	e.waitHelpOnP2P(again)
	e.mustStat(again, "重新 connect 后")
}

func assertSameP2P(t *testing.T, e *surviveEnv, help *HelpMCPBootstrap, link0 *helpLink, pc0 *P2PConn, pairs0 int) {
	t.Helper()
	if activeLink(help) != link0 {
		t.Fatal("协助端会话被拆过")
	}
	if pc, _, _ := e.shareP2P(); pc != pc0 {
		t.Fatal("被协助端应沿用原隧道")
	}
	if n := e.p2p.pairCount(); n != pairs0 {
		t.Fatalf("不应重新协商 P2P: %d → %d 次", pairs0, n)
	}
}
