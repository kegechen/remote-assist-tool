package client

import (
	"encoding/json"
	"testing"

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
