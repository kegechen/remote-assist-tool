package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// TestBridgeDisconnectWakesInflight 验证：在途 CallTool 在 Disconnect 后立即返回断开错误，
// 不必干等兜底 deadline（2~10 分钟）。
func TestBridgeDisconnectWakesInflight(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, [32]byte{})

	type result struct {
		out json.RawMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		// 不设 deadline：若没有 Disconnect 唤醒，会卡到 10 分钟兜底。
		out, err := br.CallTool(context.Background(), "exec", json.RawMessage(`{"argv":["sleep"]}`))
		done <- result{out, err}
	}()

	// 等 ToolReq 真正发出，确保 pending 已 Store，再触发断开。
	select {
	case <-conn.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("ToolReq 未在预期内发出")
	}

	br.Disconnect(errors.New("tunnel_lost: 隧道已断开，请重新 connect"))

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("期望断开错误，得到 out=%s err=nil", r.out)
		}
		if !strings.Contains(r.err.Error(), "tunnel_lost") {
			t.Fatalf("错误文案应含 tunnel_lost，实得：%v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect 后在途 CallTool 未立即返回（疑似卡在兜底 deadline）")
	}
}

// TestBridgeCallToolAfterDisconnect 验证：Disconnect 之后再 CallTool 立即返回 not_connected，
// 不发消息、不阻塞。
func TestBridgeCallToolAfterDisconnect(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, [32]byte{})
	br.Disconnect(errors.New("tunnel_lost: 隧道已断开"))

	done := make(chan error, 1)
	go func() {
		_, err := br.CallTool(context.Background(), "exec", json.RawMessage(`{}`))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("期望 not_connected 错误，得到 nil")
		}
		if !strings.Contains(err.Error(), "not_connected") {
			t.Fatalf("错误文案应含 not_connected，实得：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect 后 CallTool 未立即返回")
	}

	// 不应往隧道发任何消息。
	select {
	case msg := <-conn.sent:
		t.Fatalf("Disconnect 后 CallTool 不应发消息，实发：%v", msg.Type)
	default:
	}
}
