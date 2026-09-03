package client

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)

type injectRecorder struct{ got []*proto.Message }

func (r *injectRecorder) Inject(m *proto.Message) { r.got = append(r.got, m) }

func TestDispatchToolMessageRoutesToDaemon(t *testing.T) {
	rec := &injectRecorder{}
	req := proto.ToolReq{ID: 1, Tool: "ping", ArgsJSON: json.RawMessage(`{}`)}
	msg, _ := proto.NewMessage(proto.MsgToolReq, &req)
	if !dispatchToolMessage(msg, rec) {
		t.Fatal("expected dispatch to handle MsgToolReq")
	}
	if len(rec.got) != 1 || rec.got[0].Type != proto.MsgToolReq {
		t.Fatalf("got: %+v", rec.got)
	}
}

func TestDispatchNonToolReturnsFalse(t *testing.T) {
	rec := &injectRecorder{}
	msg, _ := proto.NewMessage(proto.MsgTunnelData, &proto.TunnelData{Data: []byte("x")})
	if dispatchToolMessage(msg, rec) {
		t.Fatal("expected dispatch to ignore non-tool msg")
	}
}

type relayProbeTool struct{}

func (relayProbeTool) Name() string { return "relay_probe" }

func (relayProbeTool) Run(context.Context, json.RawMessage, agent.StreamSink) (json.RawMessage, error) {
	return json.RawMessage(`{"via":"relay"}`), nil
}

type sendRecorder struct {
	mu    sync.Mutex
	sends int
}

func (r *sendRecorder) SendMessage(proto.MessageType, interface{}) error {
	r.mu.Lock()
	r.sends++
	r.mu.Unlock()
	return nil
}

func (r *sendRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends
}

type fakeShareP2PManager struct {
	startGate <-chan struct{}
	started   chan struct{}
	results   chan p2p.P2PResult
	closed    chan struct{}
	ready     chan proto.PeerAddrReady
	closeOnce sync.Once
}

func newFakeShareP2PManager(startGate <-chan struct{}) *fakeShareP2PManager {
	return &fakeShareP2PManager{
		startGate: startGate,
		started:   make(chan struct{}),
		results:   make(chan p2p.P2PResult),
		closed:    make(chan struct{}),
		ready:     make(chan proto.PeerAddrReady, 2),
	}
}

func (*fakeShareP2PManager) SetRelayConn(p2p.RelayConn) {}

func (m *fakeShareP2PManager) Start(string, bool) (<-chan p2p.P2PResult, error) {
	close(m.started)
	if m.startGate != nil {
		<-m.startGate
	}
	return m.results, nil
}

func (m *fakeShareP2PManager) HandlePeerAddrReady(ready *proto.PeerAddrReady) {
	m.ready <- *ready
}

func (m *fakeShareP2PManager) Close() {
	m.closeOnce.Do(func() { close(m.closed) })
}

func newShareP2PLifecycleHarness(
	t *testing.T,
	factory func(p2p.P2PMode, string, string) shareP2PManager,
) (*ShareMode, net.Conn, <-chan error) {
	t.Helper()
	shareConn, helpConn := net.Pipe()
	relayClient := &Client{
		config: &Config{P2PMode: "auto"},
		conn:   shareConn,
		enc:    json.NewEncoder(shareConn),
		dec:    json.NewDecoder(shareConn),
	}
	s := &ShareMode{
		client:        relayClient,
		code:          "CODE1234",
		newP2PManager: factory,
	}
	// 这些测试只验证 relay/P2P 编排；避免启动永久 daemon goroutine。
	s.daemonOnce.Do(func() {})
	done := make(chan error, 1)
	go func() { done <- s.waitAndHandleTunnel() }()
	t.Cleanup(func() {
		helpConn.Close()
		shareConn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("share lifecycle goroutine did not stop")
		}
	})
	return s, helpConn, done
}

func sendShareRelayMessage(t *testing.T, enc *json.Encoder, msgType proto.MessageType, payload interface{}) {
	t.Helper()
	msg, err := proto.NewMessage(msgType, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(msg); err != nil {
		t.Fatal(err)
	}
}

func waitForFakeShareP2PManager(t *testing.T, created <-chan *fakeShareP2PManager) *fakeShareP2PManager {
	t.Helper()
	select {
	case mgr := <-created:
		return mgr
	case <-time.After(2 * time.Second):
		t.Fatal("P2P manager was not created")
		return nil
	}
}

func TestShareRelayHandshakeContinuesWhileP2PStartBlocks(t *testing.T) {
	startGate := make(chan struct{})
	mgr := newFakeShareP2PManager(startGate)
	_, helpConn, _ := newShareP2PLifecycleHarness(t, func(p2p.P2PMode, string, string) shareP2PManager {
		return mgr
	})
	defer close(startGate)

	enc := json.NewEncoder(helpConn)
	sendShareRelayMessage(t, enc, proto.MsgSessionReady, &proto.SessionReady{SessionID: "session-1"})
	select {
	case <-mgr.started:
	case <-time.After(2 * time.Second):
		t.Fatal("P2P manager Start was not invoked")
	}

	hello := proto.NewHello()
	if err := helpConn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	sendShareRelayMessage(t, enc, proto.MsgToolHello, &hello)
	if err := helpConn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := helpConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ackMsg proto.Message
	if err := json.NewDecoder(helpConn).Decode(&ackMsg); err != nil {
		t.Fatalf("P2P Start 阻塞时 relay ToolHello 未得到应答: %v", err)
	}
	if ackMsg.Type != proto.MsgToolHelloAck {
		t.Fatalf("收到 %s，期望 %s", ackMsg.Type, proto.MsgToolHelloAck)
	}
	var ack proto.HelloAck
	if err := proto.DecodePayload(&ackMsg, &ack); err != nil || !ack.Accept {
		t.Fatalf("relay ToolHelloAck 无效: ack=%+v err=%v", ack, err)
	}
}

func TestShareSessionReadyReplacesP2PManager(t *testing.T) {
	created := make(chan *fakeShareP2PManager, 2)
	s, helpConn, _ := newShareP2PLifecycleHarness(t, func(p2p.P2PMode, string, string) shareP2PManager {
		mgr := newFakeShareP2PManager(nil)
		created <- mgr
		return mgr
	})
	enc := json.NewEncoder(helpConn)

	sendShareRelayMessage(t, enc, proto.MsgSessionReady, &proto.SessionReady{SessionID: "session-1"})
	first := waitForFakeShareP2PManager(t, created)
	firstReady := proto.PeerAddrReady{PeerPrivateAddr: "10.0.0.1:1001"}
	sendShareRelayMessage(t, enc, proto.MsgPeerAddrReady, &firstReady)
	select {
	case got := <-first.ready:
		if got.PeerPrivateAddr != firstReady.PeerPrivateAddr {
			t.Fatalf("first manager got peer %q", got.PeerPrivateAddr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first manager did not receive PeerAddrReady")
	}

	// helper 在 relay 的断连去抖窗口内换代时，不会先收到 PEER_DISCONNECTED，
	// 只有新的 SessionReady；它必须足以关闭旧 manager 并启动新一轮协商。
	sendShareRelayMessage(t, enc, proto.MsgSessionReady, &proto.SessionReady{SessionID: "session-1"})
	second := waitForFakeShareP2PManager(t, created)
	if first == second {
		t.Fatal("new helper reused the previous P2P manager")
	}
	select {
	case <-first.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("previous P2P manager was not closed")
	}

	secondReady := proto.PeerAddrReady{PeerPrivateAddr: "10.0.0.2:1002"}
	sendShareRelayMessage(t, enc, proto.MsgPeerAddrReady, &secondReady)
	select {
	case got := <-second.ready:
		if got.PeerPrivateAddr != secondReady.PeerPrivateAddr {
			t.Fatalf("second manager got peer %q", got.PeerPrivateAddr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement manager did not receive PeerAddrReady")
	}
	select {
	case stale := <-first.ready:
		t.Fatalf("PeerAddrReady was delivered to closed manager: %+v", stale)
	default:
	}

	// 保证测试确实经过当前 ShareMode 状态，而不只是 factory 被调用两次。
	s.p2pMu.Lock()
	active := s.p2pMgr
	s.p2pMu.Unlock()
	if active != second {
		t.Fatal("replacement manager is not the active P2P manager")
	}
}

func TestRelayHelloSwapsDaemonFromStaleP2PConn(t *testing.T) {
	shareConn, helpConn := net.Pipe()
	t.Cleanup(func() {
		shareConn.Close()
		helpConn.Close()
	})
	relayClient := &Client{
		conn: shareConn,
		enc:  json.NewEncoder(shareConn),
		dec:  json.NewDecoder(shareConn),
	}

	reg := agent.NewRegistry()
	reg.Register(relayProbeTool{})
	staleP2P := &sendRecorder{}
	daemon := agent.NewDaemon(reg, staleP2P, [32]byte{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go daemon.RunLoop(ctx)

	s := &ShareMode{client: relayClient, code: "CODE1234", daemon: daemon}
	// daemon 已由测试装好，阻止 ensureDaemon 覆盖它并另起永久 goroutine。
	s.daemonOnce.Do(func() {})

	hello := proto.NewHello()
	helloMsg, err := proto.NewMessage(proto.MsgToolHello, &hello)
	if err != nil {
		t.Fatal(err)
	}
	handlerDone := make(chan error, 1)
	go func() { handlerDone <- s.handleRelayToolHello(helloMsg) }()

	decoder := json.NewDecoder(helpConn)
	if err := helpConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ackMsg proto.Message
	if err := decoder.Decode(&ackMsg); err != nil {
		t.Fatalf("read relay hello ack: %v", err)
	}
	if err := <-handlerDone; err != nil {
		t.Fatalf("handle relay hello: %v", err)
	}
	var ack proto.HelloAck
	if err := proto.DecodePayload(&ackMsg, &ack); err != nil {
		t.Fatalf("decode relay hello ack: %v", err)
	}
	key := proto.DeriveSessionKey(s.code, ack.NonceB64, hello.NonceB64)
	args, err := proto.AEADSealJSON(&key, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	reqMsg, err := proto.NewMessage(proto.MsgToolReq, &proto.ToolReq{
		ID:       42,
		Tool:     "relay_probe",
		ArgsJSON: args,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.daemon.Inject(reqMsg)

	var respMsg proto.Message
	if err := decoder.Decode(&respMsg); err != nil {
		t.Fatalf("ToolResp 未从 relay 返回: %v", err)
	}
	var resp proto.ToolResp
	if err := proto.DecodePayload(&respMsg, &resp); err != nil {
		t.Fatalf("decode ToolResp: %v", err)
	}
	if resp.ID != 42 || !resp.OK {
		t.Fatalf("unexpected ToolResp: %+v", resp)
	}
	if got := staleP2P.count(); got != 0 {
		t.Fatalf("ToolResp 仍写向旧 P2P 连接: sends=%d", got)
	}
}
