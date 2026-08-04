package client

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
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
