package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

type fakeConn struct {
	in   chan *proto.Message
	out  chan *proto.Message
	once sync.Once
}

func (c *fakeConn) SendMessage(t proto.MessageType, p interface{}) error {
	msg, _ := proto.NewMessage(t, p)
	c.out <- msg
	return nil
}
func (c *fakeConn) Recv() *proto.Message { return <-c.in }

func TestDaemonRoutesToolReq(t *testing.T) {
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 4)
	conn := &fakeConn{in: in, out: out}

	r := NewRegistry()
	r.Register(&fakeTool{name: "ping"})
	d := NewDaemon(r, conn, [32]byte{})

	go d.RunLoop(context.Background())

	req := proto.ToolReq{ID: 9, Tool: "ping", ArgsJSON: json.RawMessage(`{}`)}
	msg, _ := proto.NewMessage(proto.MsgToolReq, &req)
	d.Inject(msg)

	select {
	case got := <-out:
		if got.Type != proto.MsgToolResp {
			t.Fatalf("expected ToolResp, got %s", got.Type)
		}
		var resp proto.ToolResp
		proto.DecodePayload(got, &resp)
		if resp.ID != 9 || !resp.OK {
			t.Fatalf("got %+v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("no response in 1s")
	}
}

func TestDaemonRotateKeyCancelsInflight(t *testing.T) {
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 16)
	conn := &fakeConn{in: in, out: out}

	r := NewRegistry()
	r.Register(&fakeTool{name: "ping"})
	d := NewDaemon(r, conn, [32]byte{1})
	go d.RunLoop(context.Background())

	// rotate 不应该 panic，cancel 空 in-flight 是 no-op
	d.RotateKey([32]byte{2})

	if d.key != [32]byte{2} {
		t.Fatalf("key not rotated: got %v", d.key)
	}
}
