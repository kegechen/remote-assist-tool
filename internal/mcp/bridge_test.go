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

// discardConn 丢弃一切发送，用于并发压测（不会因 channel 满而阻塞）。
type discardConn struct{}

func (discardConn) SendMessage(proto.MessageType, interface{}) error { return nil }

// TestBridgeSwapConnRoutesToNewConn 验证 relay→P2P 热升级：SwapConn 之后的 CallTool
// 必须走新连接，旧连接不该再收到任何请求。
func TestBridgeSwapConnRoutesToNewConn(t *testing.T) {
	oldConn := &stubConn{sent: make(chan *proto.Message, 4)}
	newConn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(oldConn, [32]byte{})

	br.SwapConn(newConn, [32]byte{})

	go func() {
		req := <-newConn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		resp, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: r.ID, OK: true, ResultJSON: json.RawMessage(`{"ok":1}`)})
		br.HandleInbound(resp)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := br.CallTool(ctx, "stat", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("SwapConn 后调用失败: %v", err)
	}
	if string(out) != `{"ok":1}` {
		t.Fatalf("got %s", out)
	}
	select {
	case msg := <-oldConn.sent:
		t.Fatalf("SwapConn 后不应再往旧连接发消息，实发：%v", msg.Type)
	default:
	}
}

// TestBridgeSwapConnSwitchesKey 验证 SwapConn 会同时换掉会话密钥：请求用新 key 加密，
// 响应也按新 key 解密。P2P 断开后降级回 relay 要重新握手换 key，靠的就是这条。
func TestBridgeSwapConnSwitchesKey(t *testing.T) {
	oldKey := [32]byte{1}
	newKey := [32]byte{2}
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, oldKey)
	br.SwapConn(conn, newKey)

	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		// 用新 key 应能解开请求参数；解不开就不应答，让 CallTool 超时把问题暴露出来。
		if _, err := proto.AEADOpenJSON(&newKey, r.ArgsJSON, proto.ToolReqAAD(r.ID, r.Tool, r.DeadlineMs)); err != nil {
			return
		}
		sealed, _ := proto.AEADSealJSON(&newKey, json.RawMessage(`{"ok":2}`), proto.ToolRespAAD(r.ID))
		resp, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: r.ID, OK: true, ResultJSON: sealed})
		br.HandleInbound(resp)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := br.CallTool(ctx, "stat", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("换 key 后调用失败: %v", err)
	}
	if string(out) != `{"ok":2}` {
		t.Fatalf("got %s", out)
	}
}

// TestBridgeSwapConnRace 并发 SwapConn 与 CallTool 不应触发数据竞态（go test -race）。
// 后台 P2P 升级正是在工具调用可能同时在飞的时候换 conn/key 的。
func TestBridgeSwapConnRace(t *testing.T) {
	br := NewBridge(discardConn{}, [32]byte{})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := byte(0)
		for {
			select {
			case <-stop:
				return
			default:
				i++
				br.SwapConn(discardConn{}, [32]byte{i})
			}
		}
	}()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, _ = br.CallTool(ctx, "stat", json.RawMessage(`{}`))
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
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
