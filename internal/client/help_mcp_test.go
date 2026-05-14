package client

import (
	"encoding/json"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

type bridgeRecorder struct{ inbound []*proto.Message }

func (b *bridgeRecorder) HandleInbound(m *proto.Message) { b.inbound = append(b.inbound, m) }

func TestHelpDispatchRoutesToolRespToBridge(t *testing.T) {
	br := &bridgeRecorder{}
	msg, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: 1, OK: true, ResultJSON: json.RawMessage(`{}`)})
	if !dispatchHelpToolMessage(msg, br) {
		t.Fatal("expected dispatch")
	}
	if len(br.inbound) != 1 {
		t.Fatalf("got %d", len(br.inbound))
	}
}

func TestHelpDispatchIgnoresTunnelData(t *testing.T) {
	br := &bridgeRecorder{}
	msg, _ := proto.NewMessage(proto.MsgTunnelData, &proto.TunnelData{Data: []byte("x")})
	if dispatchHelpToolMessage(msg, br) {
		t.Fatal("did not expect dispatch for TunnelData")
	}
}
