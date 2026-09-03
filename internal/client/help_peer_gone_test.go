package client

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// fakeRelay 是够 doConnect 跑通的最小 relay：应答 JoinRequest 与 ToolHello，之后由测试
// 用 push 主动下发消息。不做 P2P（测试里 P2PMode=disabled）。
type fakeRelay struct {
	ln     net.Listener
	connCh chan net.Conn
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	r := &fakeRelay{ln: ln, connCh: make(chan net.Conn, 1)}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		enc := json.NewEncoder(conn)
		dec := json.NewDecoder(conn)
		for {
			var msg proto.Message
			if err := dec.Decode(&msg); err != nil {
				conn.Close()
				return
			}
			switch msg.Type {
			case proto.MsgJoinRequest:
				resp, _ := proto.NewMessage(proto.MsgJoinResponse, &proto.JoinResponse{
					Success:   true,
					SessionID: "ses-test",
				})
				if enc.Encode(resp) != nil {
					return
				}
			case proto.MsgToolHello:
				var hello proto.Hello
				proto.DecodePayload(&msg, &hello)
				peer := proto.NewHello()
				ack, _ := proto.NewMessage(proto.MsgToolHelloAck, &proto.HelloAck{
					Version:      proto.ToolProtocolVersion,
					Capabilities: peer.Capabilities,
					NonceB64:     peer.NonceB64,
					Accept:       true,
				})
				if enc.Encode(ack) != nil {
					return
				}
				// 握手完成，把连接交给测试；之后只 drain（心跳等），由 push 单向下发。
				select {
				case r.connCh <- conn:
				default:
				}
			}
		}
	}()
	return r
}

func (r *fakeRelay) addr() string { return r.ln.Addr().String() }

// handshakenConn 等到工具握手完成后的那条连接。
func (r *fakeRelay) handshakenConn(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-r.connCh:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没等到工具握手完成")
		return nil
	}
}

func (r *fakeRelay) pushError(t *testing.T, conn net.Conn, code, message string) {
	t.Helper()
	msg, _ := proto.NewMessage(proto.MsgError, &proto.ErrorMessage{Code: code, Message: message})
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		t.Fatalf("push %s: %v", code, err)
	}
}

// waitSessionTornDown 轮询等 teardown 把 bootstrap 的活动会话清干净。
func waitSessionTornDown(t *testing.T, b *HelpMCPBootstrap, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		cleared := b.help == nil && b.bridge == nil && b.activeTarget == connectTarget{}
		b.mu.Unlock()
		if cleared {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func connectToFakeRelay(t *testing.T, r *fakeRelay) *HelpMCPBootstrap {
	t.Helper()
	b := NewHelpMCPBootstrap(&Config{ServerAddr: r.addr(), P2PMode: "disabled"})
	raw, err := b.doConnect(context.Background(), json.RawMessage(`{"code":"ABCD-EF"}`))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var res connectResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode connect result: %v", err)
	}
	if !res.Connected {
		t.Fatal("connect 应报告 connected=true")
	}
	return b
}

// relay 推 PEER_DISCONNECTED 之后，help 必须拆掉整个会话。
//
// 旧实现只 log.Printf 一行就接着读。可被协助端已经没了，这条 relay 上不会再有任何工具
// 响应，而 b.activeTarget 仍留着——下一次同参数 connect 命中幂等分支，拿到一个指向死
// 会话的 connected=true，此后每个工具调用都只能干等超时，且永远自愈不了。
func TestHelpTearsDownOnPeerDisconnected(t *testing.T) {
	r := newFakeRelay(t)
	b := connectToFakeRelay(t, r)
	conn := r.handshakenConn(t)

	r.pushError(t, conn, proto.ErrCodePeerDisconnected, "被协助端已断开连接")

	if !waitSessionTornDown(t, b, 5*time.Second) {
		t.Fatal("收到 PEER_DISCONNECTED 后活动会话仍未清除：下一次 connect 会复用死会话")
	}
}

// PEER_RECONNECTED 同理：relay 发完就会关掉旧 help 的连接，留着活动会话没有意义。
func TestHelpTearsDownOnPeerReconnected(t *testing.T) {
	r := newFakeRelay(t)
	b := connectToFakeRelay(t, r)
	conn := r.handshakenConn(t)

	r.pushError(t, conn, proto.ErrCodePeerReconnected, "被协助端连接已更新，请重新加入")

	if !waitSessionTornDown(t, b, 5*time.Second) {
		t.Fatal("收到 PEER_RECONNECTED 后活动会话仍未清除")
	}
}

// 其它错误码（限流、会话超限等）不代表对端消失，不该拆会话——否则一条无关的告警就
// 能把健康连接打掉。
func TestHelpKeepsSessionOnUnrelatedRelayError(t *testing.T) {
	r := newFakeRelay(t)
	b := connectToFakeRelay(t, r)
	conn := r.handshakenConn(t)

	r.pushError(t, conn, "RATE_LIMITED", "slow down")

	time.Sleep(300 * time.Millisecond)
	b.mu.Lock()
	alive := b.help != nil && b.bridge != nil
	b.mu.Unlock()
	if !alive {
		t.Fatal("无关错误码不应拆掉会话")
	}
}

// peerGoneError 只认这两个码，其余一律放行给日志分支。
func TestPeerGoneErrorClassification(t *testing.T) {
	cases := []struct {
		code string
		gone bool
	}{
		{proto.ErrCodePeerDisconnected, true},
		{proto.ErrCodePeerReconnected, true},
		{"RATE_LIMITED", false},
		{"SESSION_LIMIT", false},
		{"", false},
	}
	for _, c := range cases {
		msg, _ := proto.NewMessage(proto.MsgError, &proto.ErrorMessage{Code: c.code})
		if got := peerGoneError(msg) != nil; got != c.gone {
			t.Errorf("code=%q gone=%v，期望 %v", c.code, got, c.gone)
		}
	}
}
