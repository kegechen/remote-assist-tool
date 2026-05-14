package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

type stubConn struct {
	sent chan *proto.Message
}

func (c *stubConn) SendMessage(t proto.MessageType, p interface{}) error {
	msg, _ := proto.NewMessage(t, p)
	c.sent <- msg
	return nil
}

func TestBridgeCallToolResolvesOnResp(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, [32]byte{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		resp, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: r.ID, OK: true, ResultJSON: json.RawMessage(`{"echo":1}`)})
		br.HandleInbound(resp)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := br.CallTool(ctx, "exec", json.RawMessage(`{"argv":["echo"]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(out) != `{"echo":1}` {
		t.Fatalf("got %s", out)
	}
	wg.Wait()
}
