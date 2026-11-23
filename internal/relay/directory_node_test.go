package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
	"github.com/remote-assist/tool/internal/proto"
)

// nodeEnv 是节点模式测试的公共环境：测试根密钥、本机 TLS 证书及其指纹、可控时钟，
// 以及一个代替 CF 发布节点列表的 HTTPS 服务。
type nodeEnv struct {
	t        *testing.T
	f        *dirtest.Fixture
	certFile string
	keyFile  string
	fp       string // 本机 TLS 证书指纹
	stateDir string
	ts       *httptest.Server

	mu   sync.Mutex
	list []byte // 当前发布的列表；nil 时返回 503
	now  time.Time
}

func newNodeEnv(t *testing.T) *nodeEnv {
	t.Helper()
	dir := t.TempDir()
	e := &nodeEnv{
		t:        t,
		f:        dirtest.New(t),
		certFile: filepath.Join(dir, "server.crt"),
		keyFile:  filepath.Join(dir, "server.key"),
		stateDir: filepath.Join(dir, "state"),
		now:      dirtest.Epoch,
	}
	if err := crypto.GenerateSelfSignedCert(e.certFile, e.keyFile); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(e.certFile, e.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	e.fp = directory.CertFingerprint(pair.Certificate[0])
	e.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		list := e.list
		e.mu.Unlock()
		if r.URL.Path != "/v1/directory" || list == nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(list)
	}))
	t.Cleanup(e.ts.Close)
	return e
}

func (e *nodeEnv) serve(list []byte) {
	e.mu.Lock()
	e.list = list
	e.mu.Unlock()
}

func (e *nodeEnv) setNow(now time.Time) {
	e.mu.Lock()
	e.now = now
	e.mu.Unlock()
}

func (e *nodeEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

// selfEntry 是本机（证书指纹 e.fp）的列表项，前缀 prefix、状态 status；mod 可改证书内容。
func (e *nodeEnv) selfEntry(prefix, status string, mod func(*directory.NodeCert)) directory.NodeEntry {
	c := e.f.Cert(prefix, func(c *directory.NodeCert) {
		c.TLSFPs = []string{e.fp}
		if mod != nil {
			mod(c)
		}
	})
	return directory.NodeEntry{Cert: e.f.SignCert(c), Status: status, Weight: 100}
}

// sign 签一份序号为 serial 的列表（第一项是默认节点，有效期到 Epoch+30 天）。
func (e *nodeEnv) sign(serial uint64, nodes ...directory.NodeEntry) []byte {
	b := e.f.Body(nodes...)
	b.Serial = serial
	return e.f.Sign(b)
}

func (e *nodeEnv) config() Config {
	roots := e.f.Roots
	return Config{
		CodeTTL:               time.Minute,
		CodeLength:            12,
		UseTLS:                true,
		TLSCertFile:           e.certFile,
		TLSKeyFile:            e.keyFile,
		StateDir:              e.stateDir,
		DisableSourceIPLimits: true,
		DirectoryURL:          e.ts.URL + "/v1/directory",
		directoryRoots:        &roots,
		directoryNow:          e.clock,
		directoryClient:       e.ts.Client(),
	}
}

// server 建一个节点模式 relay，并同步拉取一次列表（相当于启动后后台的第一次拉取）。
func (e *nodeEnv) server() *Server {
	e.t.Helper()
	cfg := e.config()
	srv, err := NewServer(&cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := srv.refreshDirectory(context.Background()); err != nil {
		e.t.Fatalf("首次拉取列表: %v", err)
	}
	return srv
}

// nodeReg 描述一次 share 注册请求。capable 表示声明 directory_serial（新客户端），
// keySerial、serial 是客户端缓存的列表位置（状态密钥授权序号、列表序号）。
type nodeReg struct {
	clientID    string
	capable     bool
	keySerial   uint64
	serial      uint64
	requestCode string
	token       string
}

type nodeRegResult struct {
	closed bool
	conn   *reuseProtocolConn
	msg    proto.Message
	resp   proto.RegisterResponse     // msg 是注册响应时
	fields map[string]json.RawMessage // 注册响应 payload 的原始字段，用来断言字段有没有出现
	err    proto.ErrorMessage         // msg 是错误时
}

var nodeConnSeq atomic.Int64

// nodeRegister 经 handleMessage（含首帧解析）完成一次注册，返回收到的第一条消息。
func nodeRegister(t *testing.T, srv *Server, r nodeReg) nodeRegResult {
	t.Helper()
	req := proto.RegisterRequest{
		ClientID:           r.clientID,
		Version:            "1.3.0",
		SupportsStickyCode: true,
		RequestCode:        r.requestCode,
		ReuseToken:         r.token,
	}
	if r.capable {
		serial := r.serial
		req.DirectorySerial = &serial
		req.DirectoryKeySerial = r.keySerial
	}
	m, err := proto.NewMessage(proto.MsgRegisterRequest, &req)
	if err != nil {
		t.Fatal(err)
	}
	conn := &reuseProtocolConn{remoteAddr: "203.0.113.7:1"}
	c := &ClientConn{ID: fmt.Sprintf("node-share-%d", nodeConnSeq.Add(1)), Conn: conn}
	res := nodeRegResult{closed: srv.handleMessage(c, m), conn: conn}
	msgs := conn.messages(t)
	if len(msgs) == 0 {
		t.Fatalf("注册 %s 后没收到任何消息", r.clientID)
	}
	res.msg = msgs[0]
	switch res.msg.Type {
	case proto.MsgRegisterResponse:
		if err := proto.DecodePayload(&res.msg, &res.resp); err != nil {
			t.Fatal(err)
		}
		if err := proto.DecodePayload(&res.msg, &res.fields); err != nil {
			t.Fatal(err)
		}
	case proto.MsgError:
		if err := proto.DecodePayload(&res.msg, &res.err); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

// mustRegister 要求注册成功，返回响应。
func mustRegister(t *testing.T, srv *Server, r nodeReg) nodeRegResult {
	t.Helper()
	res := nodeRegister(t, srv, r)
	if res.closed || res.msg.Type != proto.MsgRegisterResponse {
		t.Fatalf("注册 %s 应成功，得 closed=%v %s %+v", r.clientID, res.closed, res.msg.Type, res.err)
	}
	return res
}

// mustReject 要求注册被以 code 拒绝并关闭连接。
func mustReject(t *testing.T, srv *Server, r nodeReg, code string) nodeRegResult {
	t.Helper()
	res := nodeRegister(t, srv, r)
	if !res.closed || res.msg.Type != proto.MsgError || res.err.Code != code {
		t.Fatalf("注册 %s 应被 %s 拒绝并断开，得 closed=%v %s %+v code=%q", r.clientID, code, res.closed, res.msg.Type, res.err, res.resp.Code)
	}
	return res
}

// nodeJoin 经 handleMessage 完成一次加入，要求成功，返回响应与原始字段。capable、keySerial、serial 同 nodeReg。
func nodeJoin(t *testing.T, srv *Server, code string, capable bool, keySerial, serial uint64) (proto.JoinResponse, map[string]json.RawMessage) {
	t.Helper()
	req := proto.JoinRequest{Code: code, Version: "1.3.0", SupportsUpdateNotice: true, SupportsCanonicalCode: true}
	if capable {
		s := serial
		req.DirectorySerial = &s
		req.DirectoryKeySerial = keySerial
	}
	m, err := proto.NewMessage(proto.MsgJoinRequest, &req)
	if err != nil {
		t.Fatal(err)
	}
	conn := &reuseProtocolConn{remoteAddr: "198.51.100.9:2"}
	c := &ClientConn{ID: fmt.Sprintf("node-help-%d", nodeConnSeq.Add(1)), Conn: conn}
	if srv.handleMessage(c, m) {
		t.Fatalf("加入 %s 被要求断开", code)
	}
	msgs := conn.messages(t)
	if len(msgs) == 0 || msgs[0].Type != proto.MsgJoinResponse {
		t.Fatalf("加入 %s 没收到加入响应: %v", code, msgs)
	}
	var resp proto.JoinResponse
	var fields map[string]json.RawMessage
	if err := proto.DecodePayload(&msgs[0], &resp); err != nil {
		t.Fatal(err)
	}
	if err := proto.DecodePayload(&msgs[0], &fields); err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Fatalf("加入 %s 失败: %q", code, resp.Error)
	}
	return resp, fields
}

func assertCodeFormat(t *testing.T, code string, prefix byte) {
	t.Helper()
	if len(code) != 12 || code[0] != prefix {
		t.Fatalf("码 %q 应为 12 位且首位是本节点前缀 %c", code, prefix)
	}
	for _, r := range code {
		if !strings.ContainsRune(charset, r) {
			t.Fatalf("码 %q 含字符集外的字符 %q", code, r)
		}
	}
}

// 节点模式下发的码首位是本节点前缀、其余 11 位随机；新旧客户端都一样。
// node_status 只发给声明了能力的 share。
func TestNodeModeIssuesPrefixedCodes(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	srv := e.server()
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		r := mustRegister(t, srv, nodeReg{clientID: fmt.Sprintf("cid-%d", i)})
		assertCodeFormat(t, r.resp.Code, 'K')
		if _, ok := r.fields["node_status"]; ok {
			t.Fatalf("旧客户端的响应不应带 node_status: %s", r.msg.Payload)
		}
		seen[r.resp.Code[1:]] = true
	}
	if len(seen) < 20 {
		t.Fatalf("后 11 位应随机，20 次注册只得到 %d 个不同的码", len(seen))
	}
	r := mustRegister(t, srv, nodeReg{clientID: "cid-new", capable: true})
	assertCodeFormat(t, r.resp.Code, 'K')
	if r.resp.NodeStatus != directory.StatusActive {
		t.Fatalf("新客户端的响应应带 node_status=active，得 %q", r.resp.NodeStatus)
	}
}

// 本节点升级前发的码首位是随机的：凭令牌申请复用时，首位不是本节点前缀就换新码（help 会按首位把它路由到别的节点）。
func TestNodeModeReissuesForeignPrefixCode(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	srv := e.server()
	now := time.Now()

	foreign := "BCDEFGHJKMNP"
	r := mustRegister(t, srv, nodeReg{clientID: "cid-1", capable: true, requestCode: foreign, token: srv.reuse.issue(foreign, "cid-1", now)})
	if r.resp.Code == foreign {
		t.Fatalf("外前缀的码 %s 不应被复用", foreign)
	}
	assertCodeFormat(t, r.resp.Code, 'K')

	// 对照：本节点前缀的码凭令牌照常复用。
	own := "KCDEFGHJKMNP"
	r = mustRegister(t, srv, nodeReg{clientID: "cid-2", capable: true, requestCode: own, token: srv.reuse.issue(own, "cid-2", now)})
	if r.resp.Code != own {
		t.Fatalf("本节点前缀的码应凭令牌复用：请求 %s，得 %s", own, r.resp.Code)
	}
}

// 只有声明了能力、且列表比 relay 的旧的客户端才在注册/加入响应里收到列表。
// 新旧按（状态密钥授权序号, 列表序号）比较：换状态密钥后列表序号从头算，旧密钥下序号再高也算旧。
func TestNodeModeForwardsDirectoryOnlyToOlderCapableClients(t *testing.T) {
	e := newNodeEnv(t)
	status2 := dirtest.SeedKey("status-2")
	e.f.StatusCert = e.f.IssueStatus(2, status2, e.f.Primary, directory.SignerPrimary)
	e.f.Status = status2
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	srv := e.server()
	cases := []struct {
		name      string
		capable   bool
		keySerial uint64
		serial    uint64
		want      bool
	}{
		{"旧客户端", false, 0, 0, false},
		{"没有缓存", true, 0, 0, true},
		{"同一状态密钥、序号更低", true, 2, 41, true},
		{"序号相同", true, 2, 42, false},
		{"序号更高", true, 2, 43, false},
		{"旧状态密钥、序号更高", true, 1, 500, true},
		{"更新的状态密钥", true, 3, 1, false},
	}
	for i, tc := range cases {
		r := mustRegister(t, srv, nodeReg{clientID: fmt.Sprintf("cid-%d", i), capable: tc.capable, keySerial: tc.keySerial, serial: tc.serial})
		join, joinFields := nodeJoin(t, srv, r.resp.Code, tc.capable, tc.keySerial, tc.serial)
		for side, got := range map[string]json.RawMessage{"注册": r.fields["directory"], "加入": joinFields["directory"]} {
			if (got != nil) != tc.want {
				t.Fatalf("%s/%s: 响应带列表 = %v，应为 %v", tc.name, side, got != nil, tc.want)
			}
		}
		if !tc.want {
			continue
		}
		for side, raw := range map[string]json.RawMessage{"注册": r.resp.Directory, "加入": join.Directory} {
			v, err := directory.Verify(raw, e.f.Roots, directory.State{}, dirtest.Epoch)
			if err != nil {
				t.Fatalf("%s/%s: 下发的列表验不过: %v", tc.name, side, err)
			}
			if v.StatusKeySerial != 2 || v.Serial != 42 {
				t.Fatalf("%s/%s: 下发的列表位置 (%d, %d)，应为 (2, 42)", tc.name, side, v.StatusKeySerial, v.Serial)
			}
		}
	}
}

// 防枚举：加入失败的响应对新旧客户端完全一致、不带列表。
// 否则枚举协助码的人能从「响应里多了列表」区分「码存在但被拒」与「码不存在」，或者白拿一份列表。
func TestNodeJoinFailureLooksTheSameForEveryClient(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	srv := e.server()
	live := mustRegister(t, srv, nodeReg{clientID: "cid-live", capable: true}).resp.Code

	failedJoin := func(code string, capable bool) json.RawMessage {
		t.Helper()
		req := proto.JoinRequest{Code: code, Version: "1.3.0", SupportsUpdateNotice: true, SupportsCanonicalCode: true}
		if capable {
			zero := uint64(0)
			req.DirectorySerial = &zero
		}
		m, err := proto.NewMessage(proto.MsgJoinRequest, &req)
		if err != nil {
			t.Fatal(err)
		}
		conn := &reuseProtocolConn{remoteAddr: "198.51.100.9:2"}
		c := &ClientConn{ID: fmt.Sprintf("node-help-%d", nodeConnSeq.Add(1)), Conn: conn}
		srv.handleMessage(c, m)
		msgs := conn.messages(t)
		if len(msgs) != 1 || msgs[0].Type != proto.MsgJoinResponse {
			t.Fatalf("加入 %s 应只收到一条加入响应: %v", code, msgs)
		}
		var resp proto.JoinResponse
		if err := proto.DecodePayload(&msgs[0], &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Success {
			t.Fatalf("加入 %s 应失败", code)
		}
		return msgs[0].Payload
	}

	// 第一次加入成功把活码占住（一个会话只收一个 help），之后再加入它就是「码存在但被拒」。
	nodeJoin(t, srv, live, false, 0, 0)

	unknown := "KZZZZZZZZZZZ"
	var first json.RawMessage
	for _, code := range []string{unknown, live} {
		for _, capable := range []bool{false, true} {
			got := failedJoin(code, capable)
			if strings.Contains(string(got), "directory") {
				t.Fatalf("加入失败的响应不应带列表（code=%s capable=%v）: %s", code, capable, got)
			}
			if first == nil {
				first = got
			} else if string(got) != string(first) {
				t.Fatalf("加入失败的响应应与其他失败完全一致（code=%s capable=%v）:\n got %s\nwant %s", code, capable, got, first)
			}
		}
	}
}

// 排空：拒绝新客户端的新注册（不建会话），接受凭令牌复用原码、按 ClientID 重连；旧客户端照常。
func TestNodeDrainingRejectsOnlyNewCapableRegistrations(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	srv := e.server()
	before := mustRegister(t, srv, nodeReg{clientID: "cid-before", capable: true})

	e.serve(e.sign(43, e.selfEntry("K", directory.StatusDraining, nil)))
	if err := srv.refreshDirectory(context.Background()); err != nil {
		t.Fatal(err)
	}

	sessions := func() int {
		srv.sessions.mu.RLock()
		defer srv.sessions.mu.RUnlock()
		return len(srv.sessions.sessions)
	}
	n := sessions()
	mustReject(t, srv, nodeReg{clientID: "cid-new", capable: true}, proto.ErrCodeNodeDraining)
	// 带了请求码但令牌验不过：同样拒绝，不能借此拿到一个新码。
	mustReject(t, srv, nodeReg{clientID: "cid-forged", capable: true, requestCode: "KABCDEFGHJKM", token: "v1.forged"}, proto.ErrCodeNodeDraining)
	// 令牌有效但码首位不是本节点前缀：本该换新码，排空时不发新码。
	foreign := "BABCDEFGHJKM"
	mustReject(t, srv, nodeReg{clientID: "cid-foreign", capable: true, requestCode: foreign, token: srv.reuse.issue(foreign, "cid-foreign", time.Now())}, proto.ErrCodeNodeDraining)
	if got := sessions(); got != n {
		t.Fatalf("被拒绝的注册不应建会话：会话数 %d → %d", n, got)
	}

	own := "KABCDEFGHJKM"
	r := mustRegister(t, srv, nodeReg{clientID: "cid-tok", capable: true, requestCode: own, token: srv.reuse.issue(own, "cid-tok", time.Now())})
	if r.resp.Code != own || r.resp.NodeStatus != directory.StatusDraining {
		t.Fatalf("凭令牌复用应取回原码并带 node_status=draining，得 code=%s status=%q", r.resp.Code, r.resp.NodeStatus)
	}

	r = mustRegister(t, srv, nodeReg{clientID: "cid-before", capable: true})
	if r.resp.Code != before.resp.Code || r.resp.NodeStatus != directory.StatusDraining {
		t.Fatalf("排空前注册的 share 重连应取回原码并带 node_status=draining，得 code=%s status=%q", r.resp.Code, r.resp.NodeStatus)
	}

	r = mustRegister(t, srv, nodeReg{clientID: "cid-old"})
	assertCodeFormat(t, r.resp.Code, 'K')
	if _, ok := r.fields["node_status"]; ok {
		t.Fatalf("旧客户端的响应不应带 node_status: %s", r.msg.Payload)
	}
}

// 停用：拒绝一切注册（含复用、重连），旧客户端收到中文提示；已建立的会话不受影响。
func TestNodeDisabledRejectsRegistrationsButKeepsSessions(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	srv := e.server()
	before := mustRegister(t, srv, nodeReg{clientID: "cid-before", capable: true})

	e.serve(e.sign(43, e.selfEntry("K", directory.StatusDisabled, nil)))
	if err := srv.refreshDirectory(context.Background()); err != nil {
		t.Fatal(err)
	}

	own := "KABCDEFGHJKM"
	cases := []struct {
		name    string
		reg     nodeReg
		wantMsg string
	}{
		{"新客户端新注册", nodeReg{clientID: "cid-new", capable: true}, ""},
		{"新客户端凭令牌复用", nodeReg{clientID: "cid-tok", capable: true, requestCode: own, token: srv.reuse.issue(own, "cid-tok", time.Now())}, ""},
		{"旧客户端", nodeReg{clientID: "cid-old"}, "该节点已停用，请升级客户端"},
		{"已注册的 share 重连", nodeReg{clientID: "cid-before", capable: true}, ""},
	}
	for _, tc := range cases {
		r := mustReject(t, srv, tc.reg, proto.ErrCodeNodeDisabled)
		if tc.wantMsg != "" && !strings.Contains(r.err.Message, tc.wantMsg) {
			t.Fatalf("%s: 提示应含 %q，得 %q", tc.name, tc.wantMsg, r.err.Message)
		}
	}

	// 重连被拒在换绑之前：原 share 的连接没被关掉，会话仍归它，help 照常能加入。
	if before.conn.isClosed() {
		t.Fatal("停用后被拒的重连不应关掉原 share 的连接")
	}
	nodeJoin(t, srv, before.resp.Code, true, 0, 0)
}

// 列表里没有本机证书指纹时不进入节点模式：码全随机，不执行任何节点的状态，但列表照样转发。
func TestNodeModeNeedsOwnCertificateInList(t *testing.T) {
	e := newNodeEnv(t)
	other := e.f.Entry("K") // 指纹不是本机的
	other.Status = directory.StatusDisabled
	e.serve(e.sign(42, e.f.Entry("A"), other))
	srv := e.server()
	firsts := make(map[byte]bool)
	var last nodeRegResult
	for i := 0; i < 40; i++ {
		last = mustRegister(t, srv, nodeReg{clientID: fmt.Sprintf("cid-%d", i), capable: true})
		if _, ok := last.fields["node_status"]; ok {
			t.Fatalf("不在列表里时不应带 node_status: %s", last.msg.Payload)
		}
		firsts[last.resp.Code[0]] = true
	}
	if len(firsts) < 2 {
		t.Fatalf("不在列表里时码首位应随机，40 次注册首位都是 %v", firsts)
	}
	if last.resp.Directory == nil {
		t.Fatal("列表有效时应转发给序号更低的客户端")
	}
}

// 启动后台循环会自己拉取列表：不用手动调 refreshDirectory 就能进入节点模式。
func TestNodeModeFetchesDirectoryInBackground(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	cfg := e.config()
	cfg.ListenAddr = "127.0.0.1:0"
	srv, err := NewServer(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv.nodeView().self != nil {
		t.Fatal("还没拉取就不应认出自己")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.StartWithContextReady(ctx, nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("relay 退出: %v", err)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for srv.nodeView().self == nil {
		if time.Now().After(deadline) {
			t.Fatal("启动后应在后台拉取列表并进入节点模式")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 列表落盘在 state-dir：重启后不等拉取就进入节点模式；之后 CF 回放更旧的列表会被拒收。
func TestNodeDirectorySurvivesRestart(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(50, e.selfEntry("K", directory.StatusActive, nil)))
	e.server()
	if _, err := os.Stat(filepath.Join(e.stateDir, "directory.json")); err != nil {
		t.Fatalf("列表应写进 state-dir: %v", err)
	}

	e.serve(nil) // CF 暂时不可用
	cfg := e.config()
	srv, err := NewServer(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := mustRegister(t, srv, nodeReg{clientID: "cid-1", capable: true})
	assertCodeFormat(t, r.resp.Code, 'K')
	if r.resp.Directory == nil {
		t.Fatal("重启后应转发落盘的列表")
	}
	if err := srv.refreshDirectory(context.Background()); err == nil {
		t.Fatal("CF 不可用时拉取应报错")
	}
	assertCodeFormat(t, mustRegister(t, srv, nodeReg{clientID: "cid-2"}).resp.Code, 'K')

	// 回放一份序号更低、把本节点标成停用的旧列表：拒收，状态不回退。
	e.serve(e.sign(49, e.selfEntry("K", directory.StatusDisabled, nil)))
	if err := srv.refreshDirectory(context.Background()); !errors.Is(err, directory.ErrRollback) {
		t.Fatalf("旧列表应以 ErrRollback 拒收，得 %v", err)
	}
	assertCodeFormat(t, mustRegister(t, srv, nodeReg{clientID: "cid-3"}).resp.Code, 'K')
}

// 拉取失败时继续用旧副本，直到它过期；过期在使用时判定，不等下一次拉取。本节点证书过期同理。
func TestNodeDirectoryExpiresWithoutRefresh(t *testing.T) {
	t.Run("列表过期", func(t *testing.T) {
		e := newNodeEnv(t)
		e.serve(e.sign(42, e.selfEntry("K", directory.StatusDisabled, nil)))
		srv := e.server()
		mustReject(t, srv, nodeReg{clientID: "cid-1", capable: true}, proto.ErrCodeNodeDisabled)

		e.serve(nil)
		e.setNow(dirtest.Epoch.Add(29 * 24 * time.Hour))
		if err := srv.refreshDirectory(context.Background()); err == nil {
			t.Fatal("CF 不可用时拉取应报错")
		}
		mustReject(t, srv, nodeReg{clientID: "cid-2", capable: true}, proto.ErrCodeNodeDisabled)

		// 列表有效期到 Epoch+30 天，再放宽 ClockSkew。
		e.setNow(dirtest.Epoch.Add(30*24*time.Hour + directory.ClockSkew + time.Minute))
		r := mustRegister(t, srv, nodeReg{clientID: "cid-3", capable: true})
		if _, ok := r.fields["node_status"]; ok {
			t.Fatalf("列表过期后不应带 node_status: %s", r.msg.Payload)
		}
		if r.resp.Directory != nil {
			t.Fatal("过期的列表不应再转发")
		}
	})
	t.Run("本节点证书过期", func(t *testing.T) {
		e := newNodeEnv(t)
		e.serve(e.sign(42, e.selfEntry("K", directory.StatusDisabled, func(c *directory.NodeCert) {
			c.NotAfter = dirtest.Epoch.Add(48 * time.Hour).Unix()
		}), e.f.Entry("A")))
		srv := e.server()
		mustReject(t, srv, nodeReg{clientID: "cid-1", capable: true}, proto.ErrCodeNodeDisabled)

		e.setNow(dirtest.Epoch.Add(48*time.Hour + directory.ClockSkew + time.Minute))
		r := mustRegister(t, srv, nodeReg{clientID: "cid-2", capable: true})
		if _, ok := r.fields["node_status"]; ok {
			t.Fatalf("本节点证书过期后不应带 node_status: %s", r.msg.Payload)
		}
		if r.resp.Directory == nil {
			t.Fatal("列表本身仍有效，应照样转发")
		}
	})
}

// 验不过的列表整份忽略，节点状态不变。
func TestNodeDirectoryIgnoresUnverifiableList(t *testing.T) {
	e := newNodeEnv(t)
	e.serve(e.sign(42, e.selfEntry("K", directory.StatusActive, nil)))
	srv := e.server()

	b := e.f.Body(e.selfEntry("K", directory.StatusDisabled, nil))
	b.Serial = 43
	forged, err := directory.SignDirectory(dirtest.SeedKey("rogue-status"), b)
	if err != nil {
		t.Fatal(err)
	}
	e.serve(forged)
	if err := srv.refreshDirectory(context.Background()); !errors.Is(err, directory.ErrBadSignature) {
		t.Fatalf("未授权状态密钥签的列表应以 ErrBadSignature 拒收，得 %v", err)
	}
	mustRegister(t, srv, nodeReg{clientID: "cid-1", capable: true})

	e.serve(bytes.Repeat([]byte(" "), directory.MaxRawSize+1))
	if err := srv.refreshDirectory(context.Background()); err == nil {
		t.Fatal("超大的响应应报错")
	}
	r := mustRegister(t, srv, nodeReg{clientID: "cid-2", capable: true})
	if r.resp.NodeStatus != directory.StatusActive {
		t.Fatalf("状态应保持 active，得 %q", r.resp.NodeStatus)
	}
}

// 节点模式的配置错误在启动时就报出来，不静默降级。
func TestNewServerRejectsBadDirectoryConfig(t *testing.T) {
	e := newNodeEnv(t)
	empty := directory.Roots{}
	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"没有 state-dir", func(c *Config) { c.StateDir = "" }},
		{"没有 TLS", func(c *Config) { c.UseTLS = false }},
		{"没有证书文件", func(c *Config) { c.TLSCertFile, c.TLSKeyFile = "", "" }},
		{"证书文件读不了", func(c *Config) { c.TLSCertFile = filepath.Join(e.stateDir, "missing.crt") }},
		{"http 地址", func(c *Config) { c.DirectoryURL = "http://relay-list.example.test/v1/directory" }},
		{"缺协议", func(c *Config) { c.DirectoryURL = "relay-list.example.test/v1/directory" }},
		{"缺主机", func(c *Config) { c.DirectoryURL = "https:///v1/directory" }},
		{"no-auth", func(c *Config) { c.NoAuth = true }},
		{"拉取间隔太短", func(c *Config) { c.DirectoryRefresh = 30 * time.Second }},
		{"没有根公钥", func(c *Config) { c.directoryRoots = &empty }},
	}
	for _, tc := range cases {
		cfg := e.config()
		tc.mod(&cfg)
		if _, err := NewServer(&cfg); err == nil {
			t.Errorf("%s: NewServer 应报错", tc.name)
		}
	}

	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.stateDir, "directory.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	if _, err := NewServer(&cfg); err == nil || !strings.Contains(err.Error(), "directory.json") {
		t.Fatalf("缓存文件损坏时应启动失败并指出文件，得 %v", err)
	}
}

// 没配 --directory-url 时，声明了能力的客户端也收不到任何新字段。
func TestDirectoryFieldsAbsentWithoutNodeMode(t *testing.T) {
	srv := newStickyServer(t, Config{DisableSourceIPLimits: true})
	r := mustRegister(t, srv, nodeReg{clientID: "cid-1", capable: true})
	_, fields := nodeJoin(t, srv, r.resp.Code, true, 0, 0)
	for _, f := range []string{"directory", "node_status"} {
		if _, ok := r.fields[f]; ok {
			t.Fatalf("注册响应不应带 %s: %s", f, r.msg.Payload)
		}
		if _, ok := fields[f]; ok {
			t.Fatalf("加入响应不应带 %s", f)
		}
	}
	if err := srv.refreshDirectory(context.Background()); err != nil {
		t.Fatalf("没开节点模式时拉取应是空操作，得 %v", err)
	}
}
