package client

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/version"
)

// regRelay 是只会注册的假 relay（TLS，只开 TCP）：每条连接上的注册请求交给 reply 决定怎么回，
// 别的消息不理；push 往最近一条连接上发消息（心跳回应、会话就绪）。
type regRelay struct {
	t        *testing.T
	addr, fp string
	reply    func(req proto.RegisterRequest) *proto.Message
	reqs     chan proto.RegisterRequest
	accepted atomic.Int32

	mu   sync.Mutex
	last *json.Encoder
	all  []net.Conn
}

func startRegRelay(t *testing.T, reply func(req proto.RegisterRequest) *proto.Message) *regRelay {
	t.Helper()
	f, _ := startRegRelayAt(t, "127.0.0.1:0", reply)
	return f
}

// startRegRelayAt 同 startRegRelay，但监听 addr（每台用新生成的证书），并返回停掉它的函数：关掉监听和已有连接，
// 之后同一个地址上可以另起一台。
func startRegRelayAt(t *testing.T, addr string, reply func(req proto.RegisterRequest) *proto.Message) (*regRelay, func()) {
	t.Helper()
	certFile, keyFile := newCertPair(t)
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	f := &regRelay{t: t, addr: ln.Addr().String(), fp: crypto.CertFingerprint(pair.Certificate[0]), reply: reply, reqs: make(chan proto.RegisterRequest, 16)}
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
	var once sync.Once
	stop := func() {
		once.Do(func() {
			ln.Close()
			f.mu.Lock()
			for _, c := range f.all {
				c.Close()
			}
			f.mu.Unlock()
		})
	}
	t.Cleanup(stop)
	return f, stop
}

func (f *regRelay) serve(c net.Conn) {
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	f.mu.Lock()
	f.all = append(f.all, c)
	f.last = enc
	f.mu.Unlock()
	for {
		var msg proto.Message
		if err := dec.Decode(&msg); err != nil {
			return
		}
		if msg.Type != proto.MsgRegisterRequest {
			continue
		}
		var req proto.RegisterRequest
		_ = json.Unmarshal(msg.Payload, &req)
		f.reqs <- req
		f.mu.Lock()
		_ = enc.Encode(f.reply(req))
		f.mu.Unlock()
	}
}

func (f *regRelay) push(typ proto.MessageType, payload any) {
	f.t.Helper()
	msg, err := proto.NewMessage(typ, payload)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.t.Fatal("假 relay 还没有连接")
	}
	if err := f.last.Encode(msg); err != nil {
		f.t.Fatal(err)
	}
}

func (f *regRelay) node() liveNode { return liveNode{addr: f.addr, fp: f.fp} }

func regOK(code, token, status string, dir []byte) func(proto.RegisterRequest) *proto.Message {
	return func(proto.RegisterRequest) *proto.Message {
		msg, _ := proto.NewMessage(proto.MsgRegisterResponse, &proto.RegisterResponse{
			Code: code, ReuseToken: token, CodeSticky: true, NodeStatus: status, Directory: dir,
		})
		return msg
	}
}

// reuseOrNew 是会复用码的 relay 的回应：请求带了码就原样给回（相当于令牌验过了），否则发 fresh。
// dir 非 nil 时随响应下发 *dir（在 relay 收到请求时读）。
func reuseOrNew(fresh, token string, dir *[]byte) func(proto.RegisterRequest) *proto.Message {
	return func(req proto.RegisterRequest) *proto.Message {
		code := fresh
		if req.RequestCode != "" {
			code = req.RequestCode
		}
		var d []byte
		if dir != nil {
			d = *dir
		}
		return regOK(code, token, "", d)(req)
	}
}

func regError(code string) func(proto.RegisterRequest) *proto.Message {
	return func(proto.RegisterRequest) *proto.Message {
		msg, _ := proto.NewMessage(proto.MsgError, &proto.ErrorMessage{Code: code, Message: "rejected"})
		return msg
	}
}

// nodeShareEnv 返回一个自动模式的 share（默认地址 defaultAddr）、它的控制台输出和协助码文件路径。
func nodeShareEnv(t *testing.T, e *dirEnv, m *dirManager, defaultAddr string) (*ShareMode, *syncBuf, string) {
	t.Helper()
	stubClipboard(t)
	codeFile := filepath.Join(t.TempDir(), "code.json")
	cfg := &Config{
		ServerAddr: defaultAddr, UseTLS: true, InsecureSkip: true,
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	}
	s := NewShareMode(cfg, "127.0.0.1:22", true, agent.SandboxConfig{}, codeFile, "")
	out := &syncBuf{}
	s.console = newTransferProgress(out, false, nil)
	if m != nil {
		s.nodes = newShareNodes(m, "", defaultAddr, "", e.logf)
	}
	t.Cleanup(s.client.Close)
	return s, out, codeFile
}

// reconnect5s 跑一轮 reconnectWithBackoff（连上并注册），最多等 5 秒。
func reconnect5s(t *testing.T, s *ShareMode) {
	t.Helper()
	_ = within5s(t, "reconnectWithBackoff", func() error { s.reconnectWithBackoff(); return nil })
}

type waitResult struct {
	id  string
	err error
}

func startWait(s *ShareMode) <-chan waitResult {
	ch := make(chan waitResult, 1)
	go func() {
		ready, err := s.waitSessionReady(nil)
		id := ""
		if ready != nil {
			id = ready.SessionID
		}
		ch <- waitResult{id, err}
	}()
	return ch
}

func notWithin(t *testing.T, what string, ch <-chan waitResult, d time.Duration) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s: waitSessionReady 提前返回 (%q, %v)", what, r.id, r.err)
	case <-time.After(d):
	}
}

// 自动模式的注册请求带上缓存的列表位置（也是能力声明）；显式模式不带，行为与原来一样。
func TestShareRegisterSendsDirectoryPosition(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	w := startRegRelay(t, regOK("W000000001", "tok-w", "", nil))
	e.publish(m, 42, "", e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	v, _ := m.Current()

	s, _, _ := nodeShareEnv(t, e, m, deadAddr(t))
	reconnect5s(t, s)
	req := recv5s(t, "注册请求", w.reqs)
	if req.DirectorySerial == nil || *req.DirectorySerial != 42 || req.DirectoryKeySerial != v.StatusKeySerial {
		t.Fatalf("注册请求的列表位置 = (%v, %d)，应为 (42, %d)", req.DirectorySerial, req.DirectoryKeySerial, v.StatusKeySerial)
	}

	explicit, _, _ := nodeShareEnv(t, e, nil, w.addr)
	reconnect5s(t, explicit)
	if req := recv5s(t, "显式模式的注册请求", w.reqs); req.DirectorySerial != nil {
		t.Fatalf("显式模式不该带 directory_serial: %d", *req.DirectorySerial)
	}
}

// 第一次运行没有列表：连默认地址，relay 随注册响应下发列表，收进缓存；之后断线重连时，默认地址正是列表里的
// 节点，就回到这个节点（严格比对指纹），并照常请求复用原码。
func TestShareAcceptsRelayListThenSticks(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	d := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message { return regOK("D000000001", "tok-d", "", list)(req) })
	list = e.sign(42, e.nodeEntry("D", d.node(), directory.StatusActive, 100))

	s, _, _ := nodeShareEnv(t, e, m, d.addr)
	reconnect5s(t, s)
	<-d.reqs
	if v, _ := m.Current(); v == nil || v.Serial != 42 {
		t.Fatal("relay 下发的列表没有收进缓存")
	}
	e.wantLogs(1, "已获取节点列表（来自 relay "+d.addr+"）")
	if s.nodes.currentPrefix() != "" {
		t.Fatal("第一次连的是默认地址，不该已在节点上")
	}

	reconnect5s(t, s)
	req := recv5s(t, "重连的注册请求", d.reqs)
	if s.nodes.currentPrefix() != "D" || s.client.node == nil {
		t.Fatal("重连应回到默认地址对应的列表节点，并进入节点模式")
	}
	if req.RequestCode != "D000000001" || req.ReuseToken != "tok-d" {
		t.Fatalf("同一节点上应请求复用原码，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
}

// 同上，但默认地址只是节点 D 在列表里的第二个地址：重连按列表顺序先连上 D 的第一个地址（同一台 relay），
// 仍是注册 code 的那个节点，要请求复用原码，不能提示「已改用节点」。
func TestShareUnlistedAddrOtherAliasKeepsCode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	d := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message { return regOK("D000000001", "tok-d", "", list)(req) })
	_, port, _ := net.SplitHostPort(d.addr)
	alias := net.JoinHostPort("localhost", port)
	c := e.f.Cert("D", func(c *directory.NodeCert) {
		c.TCP = []string{alias, d.addr}
		c.QUIC = nil
		c.TLSFPs = []string{d.fp}
	})
	list = e.sign(42, directory.NodeEntry{Cert: e.f.SignCert(c), Status: directory.StatusActive, Weight: 100})

	s, out, _ := nodeShareEnv(t, e, m, d.addr)
	reconnect5s(t, s)
	<-d.reqs
	reconnect5s(t, s)
	req := recv5s(t, "重连的注册请求", d.reqs)
	if s.nodes.currentPrefix() != "D" || s.client.config.ServerAddr != alias {
		t.Fatalf("前提不成立：应按列表顺序从 D 的第一个地址 %s 连上，实际节点 %q、地址 %s", alias, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	if req.RequestCode != "D000000001" || req.ReuseToken != "tok-d" {
		t.Fatalf("同一节点（只是换了列表里的另一个地址）应请求复用原码，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
	if strings.Contains(out.String(), "已改用") {
		t.Fatalf("没换节点，不该提示换节点：\n%s", out.String())
	}
}

// 默认地址上的 relay 不在它下发的列表里：之后选到列表节点 W 是真换了节点，不能把原码和令牌带给 W。
func TestShareUnlistedAddrNotInListDropsCode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	x := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message { return regOK("X000000001", "tok-x", "", list)(req) })
	w := startRegRelay(t, regOK("W000000002", "tok-w", "", nil))
	list = e.sign(42, e.nodeEntry("W", w.node(), directory.StatusActive, 100))

	s, out, _ := nodeShareEnv(t, e, m, x.addr)
	reconnect5s(t, s)
	<-x.reqs
	reconnect5s(t, s)
	req := recv5s(t, "W 上的注册请求", w.reqs)
	if s.nodes.currentPrefix() != "W" {
		t.Fatalf("前提不成立：应选到节点 W，实际 %q", s.nodes.currentPrefix())
	}
	if req.RequestCode != "" || req.ReuseToken != "" {
		t.Fatalf("换了节点不该带原码和令牌，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
	if !strings.Contains(out.String(), "已改用节点 node-W（W），原协助码 X000-0000-01 只在原节点上有效，协助码已变更为 W000-0000-02") {
		t.Fatalf("缺少换节点提示：\n%s", out.String())
	}
}

// 没有列表时在默认地址 X 上注册，X 出示的证书却不是节点 D 的（中间人，或别的 relay），而列表说 X 是 D 的地址。
// 重连时 X 因证书不符被跳过，从 D 的另一个地址连上：X 发的码和令牌不能带给 D。
func TestShareUnlistedAddrWrongCertDropsCode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	d := startRegRelay(t, reuseOrNew("D000000002", "tok-d", nil))
	x := startRegRelay(t, reuseOrNew("D000000001", "tok-x", &list))
	c := e.f.Cert("D", func(c *directory.NodeCert) {
		c.TCP = []string{x.addr, d.addr}
		c.QUIC = nil
		c.TLSFPs = []string{d.fp}
	})
	list = e.sign(42, directory.NodeEntry{Cert: e.f.SignCert(c), Status: directory.StatusActive, Weight: 100})

	s, _, _ := nodeShareEnv(t, e, m, x.addr)
	reconnect5s(t, s)
	<-x.reqs
	reconnect5s(t, s)
	req := recv5s(t, "D 上的注册请求", d.reqs)
	if s.nodes.currentPrefix() != "D" || s.client.config.ServerAddr != d.addr {
		t.Fatalf("前提不成立：应跳过证书不符的 X、从 %s 连上 D，实际节点 %q、地址 %s", d.addr, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	if req.RequestCode != "" || req.ReuseToken != "" {
		t.Fatalf("X 的证书不是 D 的，X 发的码和令牌不能带给 D，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
}

// 节点 D 在列表里有两张证书（换证书的过渡期，两个地址各出示一张）。没有列表时在默认地址 X（旧证书）上注册，
// 之后从出示新证书的地址 Y 连上 D：注册时那张证书是 D 的，仍是同一个节点，要请求复用原码。
func TestShareUnlistedAddrNodeWithTwoCertsKeepsCode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	x := startRegRelay(t, reuseOrNew("D000000001", "tok-d", &list))
	y := startRegRelay(t, reuseOrNew("D000000009", "tok-y", nil))
	c := e.f.Cert("D", func(c *directory.NodeCert) {
		c.TCP = []string{y.addr, x.addr}
		c.QUIC = nil
		c.TLSFPs = []string{y.fp, x.fp}
	})
	list = e.sign(42, directory.NodeEntry{Cert: e.f.SignCert(c), Status: directory.StatusActive, Weight: 100})

	s, out, _ := nodeShareEnv(t, e, m, x.addr)
	reconnect5s(t, s)
	<-x.reqs
	reconnect5s(t, s)
	req := recv5s(t, "Y 上的注册请求", y.reqs)
	if s.nodes.currentPrefix() != "D" || s.client.config.ServerAddr != y.addr {
		t.Fatalf("前提不成立：应按列表顺序从 %s 连上 D，实际节点 %q、地址 %s", y.addr, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	if req.RequestCode != "D000000001" || req.ReuseToken != "tok-d" {
		t.Fatalf("注册时的证书是 D 的，应请求复用原码，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
	if strings.Contains(out.String(), "已改用") {
		t.Fatalf("没换节点，不该提示换节点：\n%s", out.String())
	}
}

// 码在节点 A（地址 P）上注册；之后 A 移出列表，地址 P 换给了另一个节点 B（证书不同）。重选到 B、拨的还是 P：
// 地址相同也是换了节点，A 的码和令牌不能带给 B。
func TestShareSameAddrOtherNodeDropsCode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	a, stopA := startRegRelayAt(t, "127.0.0.1:0", reuseOrNew("A000000001", "tok-a", nil))
	e.publish(m, 42, "", e.nodeEntry("A", a.node(), directory.StatusActive, 100))
	s, out, _ := nodeShareEnv(t, e, m, deadAddr(t))
	reconnect5s(t, s)
	<-a.reqs
	if s.nodes.currentPrefix() != "A" {
		t.Fatalf("前提不成立：应在节点 A 上，实际 %q", s.nodes.currentPrefix())
	}
	s.client.Close()
	stopA()
	b, _ := startRegRelayAt(t, a.addr, reuseOrNew("B000000002", "tok-b", nil))
	e.publish(m, 43, "", e.nodeEntry("B", b.node(), directory.StatusActive, 100))

	reconnect5s(t, s)
	req := recv5s(t, "B 上的注册请求", b.reqs)
	if s.nodes.currentPrefix() != "B" || s.client.config.ServerAddr != a.addr {
		t.Fatalf("前提不成立：应在同一地址 %s 上连到节点 B，实际节点 %q、地址 %s", a.addr, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	if req.RequestCode != "" || req.ReuseToken != "" {
		t.Fatalf("换了节点（地址相同）不该带原码和令牌，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
	if !strings.Contains(out.String(), "已改用节点 node-B（B），原协助码 A000-0000-01 只在原节点上有效，协助码已变更为 B000-0000-02") {
		t.Fatalf("缺少换节点提示：\n%s", out.String())
	}
}

// 在节点 D 上（经列表里 D 的第一个地址）注册；之后列表过期，退回默认地址，它正是 D 的第二个地址：
// 出示的还是 D 的证书，是同一台 relay，要请求复用原码，不提示换节点。
func TestShareFallbackToDefaultSameRelayKeepsCode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	d := startRegRelay(t, reuseOrNew("D000000001", "tok-d", nil))
	_, port, _ := net.SplitHostPort(d.addr)
	alias := net.JoinHostPort("localhost", port)
	c := e.f.Cert("D", func(c *directory.NodeCert) {
		c.TCP = []string{alias, d.addr}
		c.QUIC = nil
		c.TLSFPs = []string{d.fp}
	})
	e.publish(m, 42, "", directory.NodeEntry{Cert: e.f.SignCert(c), Status: directory.StatusActive, Weight: 100})
	s, out, _ := nodeShareEnv(t, e, m, d.addr)
	reconnect5s(t, s)
	<-d.reqs
	if s.nodes.currentPrefix() != "D" || s.client.config.ServerAddr != alias {
		t.Fatalf("前提不成立：应经 %s 在节点 D 上，实际节点 %q、地址 %s", alias, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}

	e.setNow(dirtest.Epoch.Add(32 * 24 * time.Hour)) // 列表过期
	reconnect5s(t, s)
	req := recv5s(t, "回落后的注册请求", d.reqs)
	if s.nodes.currentPrefix() != "" || s.client.config.ServerAddr != d.addr {
		t.Fatalf("前提不成立：列表过期后应回到默认地址 %s，实际节点 %q、地址 %s", d.addr, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	if req.RequestCode != "D000000001" || req.ReuseToken != "tok-d" {
		t.Fatalf("同一台 relay（证书仍是 D 的）应请求复用原码，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
	if strings.Contains(out.String(), "已改用") {
		t.Fatalf("没换 relay，不该提示换节点：\n%s", out.String())
	}
}

// 在节点 D（地址 P，也是默认地址）上注册；之后列表过期退回默认地址 P，P 上却换成了另一张证书的 relay：
// 地址相同也不是 D，不能把在严格比对下拿到的码和令牌交给它。
func TestShareFallbackToDefaultOtherCertDropsCode(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	d, stopD := startRegRelayAt(t, "127.0.0.1:0", reuseOrNew("D000000001", "tok-d", nil))
	e.publish(m, 42, "", e.nodeEntry("D", d.node(), directory.StatusActive, 100))
	s, out, _ := nodeShareEnv(t, e, m, d.addr)
	reconnect5s(t, s)
	<-d.reqs
	if s.nodes.currentPrefix() != "D" {
		t.Fatalf("前提不成立：应在节点 D 上，实际 %q", s.nodes.currentPrefix())
	}
	s.client.Close()
	stopD()
	y, _ := startRegRelayAt(t, d.addr, reuseOrNew("Y000000002", "tok-y", nil))

	e.setNow(dirtest.Epoch.Add(32 * 24 * time.Hour)) // 列表过期
	reconnect5s(t, s)
	req := recv5s(t, "回落后的注册请求", y.reqs)
	if s.nodes.currentPrefix() != "" || s.client.config.ServerAddr != d.addr {
		t.Fatalf("前提不成立：列表过期后应回到默认地址 %s，实际节点 %q、地址 %s", d.addr, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	if req.RequestCode != "" || req.ReuseToken != "" {
		t.Fatalf("证书不是 D 的，不能带 D 的码和令牌，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
	if !strings.Contains(out.String(), "原协助码 D000-0000-01 只在原节点上有效，协助码已变更为 Y000-0000-02") {
		t.Fatalf("缺少换节点提示：\n%s", out.String())
	}
}

// connect 已按列表认出节点 D 之后、register 之前，缓存变得读不了：判断码是不是在这个节点上注册的，
// 不能再去读一次缓存（读不到就会误判成换了节点）。
func TestShareCodeNodeSurvivesUnreadableCache(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	d := startRegRelay(t, reuseOrNew("D000000001", "tok-d", &list))
	_, port, _ := net.SplitHostPort(d.addr)
	alias := net.JoinHostPort("localhost", port)
	c := e.f.Cert("D", func(c *directory.NodeCert) {
		c.TCP = []string{alias, d.addr}
		c.QUIC = nil
		c.TLSFPs = []string{d.fp}
	})
	list = e.sign(42, directory.NodeEntry{Cert: e.f.SignCert(c), Status: directory.StatusActive, Weight: 100})
	s, _, _ := nodeShareEnv(t, e, m, d.addr)
	reconnect5s(t, s) // 没有列表：默认地址上注册，列表随响应下发
	<-d.reqs

	s.client.Close()
	if err := s.connect(); err != nil {
		t.Fatal(err)
	}
	if s.nodes.currentPrefix() != "D" || s.client.config.ServerAddr != alias {
		t.Fatalf("前提不成立：应经 %s 在节点 D 上，实际节点 %q、地址 %s", alias, s.nodes.currentPrefix(), s.client.config.ServerAddr)
	}
	if err := os.WriteFile(e.path, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := within5s(t, "register", s.register); err != nil {
		t.Fatal(err)
	}
	req := recv5s(t, "注册请求", d.reqs)
	if req.RequestCode != "D000000001" || req.ReuseToken != "tok-d" {
		t.Fatalf("仍在注册码的那台 relay 上，应请求复用原码，实际 (%q, %q)", req.RequestCode, req.ReuseToken)
	}
}

// 没有能用的列表（CF 也取不到），默认地址上的 relay 在排空：它拒绝声明了列表能力的新注册、要客户端改选别的节点，
// 可本机无从选起。被拒一次之后要像旧客户端那样注册（不声明能力），被接纳，不能一直拿不到码（spec 6.3）。
func TestShareDrainingDefaultWithoutListRegistersLikeOldClient(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	x := startRegRelay(t, func(req proto.RegisterRequest) *proto.Message {
		if req.DirectorySerial != nil {
			return regError(proto.ErrCodeNodeDraining)(req)
		}
		return regOK("X000000001", "tok-x", "", nil)(req)
	})
	s, _, _ := nodeShareEnv(t, e, m, x.addr)
	reconnect5s(t, s)
	first := recv5s(t, "第一次注册请求", x.reqs)
	second := recv5s(t, "第二次注册请求", x.reqs)
	if first.DirectorySerial == nil {
		t.Fatal("第一次注册应照常声明列表能力（relay 有更新的列表会随响应下发）")
	}
	if second.DirectorySerial != nil {
		t.Fatal("排空的 relay 拒绝过之后，没有列表时不该再声明列表能力")
	}
	if s.code != "X000000001" {
		t.Fatalf("应在默认地址上拿到码，实际 %q", s.code)
	}
	if e.hitCount() == 0 {
		t.Fatal("被拒后应向 CF 取一次列表")
	}
	e.wantLogs(1, "[relay] 中转服务 "+x.addr+" 正在排空，本机还没有节点列表")
}

// 没有能用的列表，默认地址上的 relay 排空或停用、拒绝注册：马上向 CF 取一次列表，取到了就改选列表里的节点，
// 在节点上照常声明列表能力。
func TestShareRejectedDefaultWithoutListFetchesList(t *testing.T) {
	for _, code := range []string{proto.ErrCodeNodeDraining, proto.ErrCodeNodeDisabled} {
		t.Run(code, func(t *testing.T) {
			e := newDirEnv(t)
			m := e.manager()
			x := startRegRelay(t, regError(code))
			w := startRegRelay(t, regOK("W000000001", "tok-w", "", nil))
			e.serve(e.sign(42, e.nodeEntry("W", w.node(), directory.StatusActive, 100)))
			s, _, _ := nodeShareEnv(t, e, m, x.addr)
			reconnect5s(t, s)
			<-x.reqs
			req := recv5s(t, "W 上的注册请求", w.reqs)
			if s.nodes.currentPrefix() != "W" {
				t.Fatalf("取到列表后应改选节点 W，实际 %q", s.nodes.currentPrefix())
			}
			if req.DirectorySerial == nil {
				t.Fatal("在列表节点上注册应声明列表能力")
			}
			want := 0
			if code == proto.ErrCodeNodeDraining {
				want = 1
			}
			e.wantLogs(want, "正在排空，本机还没有节点列表")
		})
	}
}

// 注册响应说当前节点在排空：等协助端时迁到别的节点，在新节点上注册拿新码（不带原码和令牌），
// 提示换码，刷新协助码文件。
func TestShareMigratesWhenRelayReportsDraining(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	w := startRegRelay(t, regOK("W000000001", "tok-w", directory.StatusDraining, nil))
	d := startRegRelay(t, regOK("D000000002", "tok-d", "", nil))
	e.publish(m, 42, "", e.nodeEntry("W", w.node(), directory.StatusActive, 100), e.nodeEntry("D", d.node(), directory.StatusActive, 100))

	s, out, codeFile := nodeShareEnv(t, e, m, deadAddr(t))
	reconnect5s(t, s)
	<-w.reqs
	if !s.relayDraining {
		t.Fatal("注册响应的 node_status=draining 没有记下")
	}

	r := recv5s(t, "waitSessionReady", startWait(s))
	if !errors.Is(r.err, errNodeMigrated) {
		t.Fatalf("waitSessionReady = (%q, %v)，应为 errNodeMigrated", r.id, r.err)
	}
	e.wantLogs(1, "当前节点 node-W（W） 正在排空，已改用节点 node-D（D）")

	reconnect5s(t, s)
	req := recv5s(t, "新节点上的注册请求", d.reqs)
	if req.RequestCode != "" || req.ReuseToken != "" {
		t.Fatalf("换了节点不该带原码和复用令牌: (%q, %q)", req.RequestCode, req.ReuseToken)
	}
	if !strings.Contains(out.String(), "已改用节点 node-D（D），原协助码 W000-0000-01 只在原节点上有效，协助码已变更为 D000-0000-02") {
		t.Fatalf("没有换码提示:\n%s", out.String())
	}
	got, _ := readCodeFile(t, codeFile)
	if got.Code != "D000000002" || got.Server != d.addr {
		t.Fatalf("协助码文件 = %+v，应为新节点上的码和地址", got)
	}
}

// 列表把当前节点标成排空：等协助端时，下一次心跳回应时迁走（不打断读，也不另起定时器）。
func TestShareMigratesOnHeartbeatAfterListUpdate(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	w := startRegRelay(t, regOK("W000000001", "tok-w", "", nil))
	d := startRegRelay(t, regOK("D000000002", "tok-d", "", nil))
	e.publish(m, 42, "", e.nodeEntry("W", w.node(), directory.StatusActive, 100), e.nodeEntry("D", d.node(), directory.StatusActive, 100))

	s, _, _ := nodeShareEnv(t, e, m, deadAddr(t))
	reconnect5s(t, s)
	<-w.reqs
	ch := startWait(s)
	notWithin(t, "节点正常", ch, 200*time.Millisecond)

	e.publish(m, 43, "", e.nodeEntry("W", w.node(), directory.StatusDraining, 100), e.nodeEntry("D", d.node(), directory.StatusActive, 100))
	notWithin(t, "还没收到心跳", ch, 200*time.Millisecond)
	w.push(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()})
	if r := recv5s(t, "waitSessionReady", ch); !errors.Is(r.err, errNodeMigrated) {
		t.Fatalf("waitSessionReady = (%q, %v)，应为 errNodeMigrated", r.id, r.err)
	}
}

// 节点正常时心跳回应不引起迁移，协助端照常连上；不会去拨别的节点。
func TestShareNoMigrationWhenHealthy(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	w := startRegRelay(t, regOK("W000000001", "tok-w", "", nil))
	d := startRegRelay(t, regOK("D000000002", "tok-d", "", nil))
	e.publish(m, 42, "", e.nodeEntry("W", w.node(), directory.StatusActive, 100), e.nodeEntry("D", d.node(), directory.StatusActive, 100))

	s, _, _ := nodeShareEnv(t, e, m, deadAddr(t))
	reconnect5s(t, s)
	<-w.reqs
	ch := startWait(s)
	w.push(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()})
	w.push(proto.MsgSessionReady, &proto.SessionReady{SessionID: "s1"})
	if r := recv5s(t, "waitSessionReady", ch); r.err != nil || r.id != "s1" {
		t.Fatalf("waitSessionReady = (%q, %v)，应为 s1", r.id, r.err)
	}
	if n := d.accepted.Load(); n != 0 {
		t.Fatalf("节点正常时拨了别的节点 %d 次", n)
	}
}

// 第一次运行没有列表，在默认地址（就是节点 D）上注册，列表随注册响应才下发：这时还不在列表节点上，但这条连接
// 出示的是 D 的证书。之后列表把 D 标成排空，等协助端时照样迁走，在新节点上拿新码，不带原码和令牌。
func TestShareUnlistedShareMigratesWhenListMarksNodeDraining(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	d := startRegRelay(t, reuseOrNew("D000000001", "tok-d", &list))
	w := startRegRelay(t, reuseOrNew("W000000002", "tok-w", nil))
	list = e.sign(42, e.nodeEntry("D", d.node(), directory.StatusActive, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))

	s, _, _ := nodeShareEnv(t, e, m, d.addr)
	reconnect5s(t, s)
	<-d.reqs
	if v, _ := m.Current(); v == nil || v.Serial != 42 || s.nodes.currentPrefix() != "" {
		t.Fatal("前提不成立：应已收下随注册响应下发的列表，且还不在列表节点上")
	}
	ch := startWait(s)
	notWithin(t, "节点正常", ch, 200*time.Millisecond)

	e.publish(m, 43, "", e.nodeEntry("D", d.node(), directory.StatusDraining, 100), e.nodeEntry("W", w.node(), directory.StatusActive, 100))
	d.push(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()})
	if r := recv5s(t, "waitSessionReady", ch); !errors.Is(r.err, errNodeMigrated) {
		t.Fatalf("waitSessionReady = (%q, %v)，应为 errNodeMigrated", r.id, r.err)
	}
	e.wantLogs(1, "当前节点 node-D（D） 正在排空，已改用节点 node-W（W）")

	reconnect5s(t, s)
	req := recv5s(t, "W 上的注册请求", w.reqs)
	if req.RequestCode != "" || req.ReuseToken != "" {
		t.Fatalf("换了节点不该带原码和复用令牌: (%q, %q)", req.RequestCode, req.ReuseToken)
	}
}

// 不在列表节点上时按证书认所在节点，不按地址：没有列表时在默认地址 X 上注册，列表说 X 是节点 D 的地址、D 在排空，
// 可 X 出示的证书不是 D 的。认不出所在节点就不迁移，与没有这个功能时一样，不会无故换码。
func TestShareUnlistedShareWithForeignCertDoesNotMigrate(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	var list []byte
	x := startRegRelay(t, reuseOrNew("D000000001", "tok-x", &list))
	other := startRegRelay(t, reuseOrNew("D000000009", "tok-o", nil))
	w := startRegRelay(t, reuseOrNew("W000000002", "tok-w", nil))
	c := e.f.Cert("D", func(c *directory.NodeCert) {
		c.TCP = []string{x.addr}
		c.QUIC = nil
		c.TLSFPs = []string{other.fp}
	})
	list = e.sign(42, directory.NodeEntry{Cert: e.f.SignCert(c), Status: directory.StatusDraining, Weight: 100},
		e.nodeEntry("W", w.node(), directory.StatusActive, 100))

	s, _, _ := nodeShareEnv(t, e, m, x.addr)
	reconnect5s(t, s)
	<-x.reqs
	if v, _ := m.Current(); v == nil || v.Serial != 42 {
		t.Fatal("前提不成立：应已收下随注册响应下发的列表")
	}
	ch := startWait(s)
	x.push(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()})
	x.push(proto.MsgSessionReady, &proto.SessionReady{SessionID: "s1"})
	if r := recv5s(t, "waitSessionReady", ch); r.err != nil || r.id != "s1" {
		t.Fatalf("waitSessionReady = (%q, %v)，应为 s1", r.id, r.err)
	}
	if n := w.accepted.Load(); n != 0 {
		t.Fatalf("认不出所在节点却拨了别的节点 %d 次", n)
	}
}

// UseNodeList 打开自动模式：列表缓存放在给定的家目录下（升级继任者传隔离前的家目录），STUN 跟着节点走。
func TestUseNodeList(t *testing.T) {
	real := t.TempDir()
	s := NewShareMode(&Config{ServerAddr: "relay-d.example.test:8443", UseTLS: true}, "127.0.0.1:22", true, agent.SandboxConfig{}, "", "")
	s.UseNodeList("relay-w.example.test:8443", real)
	if s.nodes == nil {
		t.Fatal("没有打开自动模式")
	}
	if got, want := s.nodes.dm.path, filepath.Join(real, directoryCacheFile); got != want {
		t.Errorf("列表缓存 = %q，想要 %q", got, want)
	}
	if s.nodes.hint != "relay-w.example.test:8443" || s.nodes.defaultAddr != "relay-d.example.test:8443" {
		t.Errorf("hint = %q、默认地址 = %q", s.nodes.hint, s.nodes.defaultAddr)
	}
	if !s.stunFollowsRelay {
		t.Error("自动模式下 STUN 应跟着节点走")
	}
}

// thirdPartyEntry 同 nodeEntry（启用、权重 100），但节点是第三方的（official=false），运营者 operator。
func (e *dirEnv) thirdPartyEntry(prefix string, n liveNode, operator string) directory.NodeEntry {
	c := e.f.Cert(prefix, func(c *directory.NodeCert) {
		c.TCP, c.QUIC, c.TLSFPs = []string{n.addr}, nil, []string{n.fp}
		c.Official, c.Operator = false, operator
	})
	return directory.NodeEntry{Cert: e.f.SignCert(c), Status: directory.StatusActive, Weight: 100}
}

// 协助信息（spec 6.7）：节点模式下传输说明写「证书已按节点列表校验」；第三方节点下面加一行运营者。
// 升级提示（spec 6.6）：第三方节点下发的下载链接不展示，改用内置的；官方节点照用它下发的。
func TestShareInstructionsAndUpdateLinksOnNodes(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = "v1.0.0" // dev 版本不提示升级
	const evil = "https://downloads.example.test/remote-{version}-{os}-{arch}{ext}"
	newer := func(req proto.RegisterRequest) *proto.Message {
		msg, _ := proto.NewMessage(proto.MsgRegisterResponse, &proto.RegisterResponse{
			Code: "W000000001", ReuseToken: "tok", CodeSticky: true,
			UpdateInfo: proto.UpdateInfo{
				LatestVersion: "v99.0.0", DownloadURL: evil, ReleasePage: "https://downloads.example.test/release",
			},
		})
		return msg
	}
	for _, tc := range []struct {
		name     string
		official bool
	}{
		{"官方节点", true},
		{"第三方节点", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDirEnv(t)
			m := e.manager()
			w := startRegRelay(t, newer)
			entry := e.nodeEntry("W", w.node(), directory.StatusActive, 100)
			if !tc.official {
				entry = e.thirdPartyEntry("W", w.node(), "Example-Ops")
			}
			e.publish(m, 42, "", entry)

			s, out, _ := nodeShareEnv(t, e, m, deadAddr(t))
			reconnect5s(t, s)
			got := out.String()
			if !strings.Contains(got, "中转服务: "+w.addr+" (TLS，证书已按节点列表校验)") {
				t.Fatalf("协助信息里没有按节点列表校验的说明:\n%s", got)
			}
			if hasOp := strings.Contains(got, "节点运营者: Example-Ops（非官方节点）"); hasOp == tc.official {
				t.Fatalf("运营者一行出现 = %v，official = %v:\n%s", hasOp, tc.official, got)
			}
			if !strings.Contains(got, "v99.0.0") {
				t.Fatalf("没有升级提示:\n%s", got)
			}
			if hasEvil := strings.Contains(got, "downloads.example.test"); hasEvil != tc.official {
				t.Fatalf("展示节点下发的链接 = %v，official = %v:\n%s", hasEvil, tc.official, got)
			}
		})
	}
}

// 显式模式（没有节点列表）照旧：TLS 跳过证书校验，没有运营者一行。
func TestShareInstructionsExplicitUnchanged(t *testing.T) {
	r := startRegRelay(t, regOK("X000000001", "tok", "", nil))
	s, out, _ := nodeShareEnv(t, nil, nil, r.addr)
	reconnect5s(t, s)
	got := out.String()
	if !strings.Contains(got, "中转服务: "+r.addr+" (TLS，跳过证书校验)") || strings.Contains(got, "节点运营者") {
		t.Fatalf("显式模式的协助信息变了:\n%s", got)
	}
}

// relay 拒绝新注册（排空）：不等连续失败，下一轮就改选别的节点。
func TestShareRegisterRejectedDrainingRepicks(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	w := startRegRelay(t, regError(proto.ErrCodeNodeDraining))
	d := startRegRelay(t, regOK("D000000002", "tok-d", "", nil))
	e.publish(m, 42, "", e.nodeEntry("W", w.node(), directory.StatusActive, 100), e.nodeEntry("D", d.node(), directory.StatusActive, 100))

	s, _, _ := nodeShareEnv(t, e, m, deadAddr(t))
	reconnect5s(t, s)
	<-w.reqs
	<-d.reqs
	if s.nodes.currentPrefix() != "D" || s.code != "D000000002" {
		t.Fatalf("应改到节点 D 注册，实际在 %q，码 %q", s.nodes.currentPrefix(), s.code)
	}
	e.wantLogs(1, "节点 node-W（W） 正在排空，不接受新注册，改选其他节点")
}
