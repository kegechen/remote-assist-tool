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

	// 经访问器读：key 现在由 connMu 保护，裸读会和 SwapConn 构成竞态（-race 会报）。
	if got := d.currentKey(); got != [32]byte{2} {
		t.Fatalf("key not rotated: got %v", got)
	}
}

// TestDaemonRotateKeySameKeyKeepsInflight 锁定 P2P 热升级的前提：换通道不换 key 时
// 不得取消在途请求。升级发生在会话中途（connect 后几秒），很可能正压在用户的第一个
// exec 上——若这里退回无条件 cancel，那个调用会直接以 cancelled 收场。
func TestDaemonRotateKeySameKeyKeepsInflight(t *testing.T) {
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 16)
	conn := &fakeConn{in: in, out: out}

	r := NewRegistry()
	r.Register(&fakeTool{name: "ping"})
	key := [32]byte{7}
	d := NewDaemon(r, conn, key)

	cancelled := false
	d.cancels.Store(uint64(1), context.CancelFunc(func() { cancelled = true }))
	defer d.cancels.Delete(uint64(1))

	d.SwapConn(&fakeConn{in: in, out: out}, key) // 同一把 key，仅换通道
	if cancelled {
		t.Fatal("key 未变时不应取消在途请求（P2P 热升级会误杀用户正在跑的工具调用）")
	}

	d.SwapConn(conn, [32]byte{8}) // key 变了：重新握手，旧 key 的在途请求确实该取消
	if !cancelled {
		t.Fatal("key 变更时应取消在途请求")
	}
}
