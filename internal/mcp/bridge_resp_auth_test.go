package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// respAs 造一条 ToolResp：按 sealOK/sealCode/sealMsg 加封，却按 ok/code/msg 发出。
// 两组相同 = 一条合法响应；不同 = 一条被改过明文字段的响应。
func respAs(t *testing.T, id uint64, result string, sealOK bool, sealCode, sealMsg string, ok bool, code, msg string) *proto.Message {
	t.Helper()
	wrapped, err := proto.AEADSealJSON(&streamKey, json.RawMessage(result), proto.ToolRespAAD(id, sealOK, sealCode, sealMsg))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	m, err := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: id, OK: ok, ResultJSON: wrapped, ErrorCode: code, ErrorMsg: msg})
	if err != nil {
		t.Fatalf("new msg: %v", err)
	}
	return m
}

func callWith(t *testing.T, reply func(br *Bridge, id uint64)) (json.RawMessage, error) {
	t.Helper()
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey)
	go func() {
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		reply(br, r.ID)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return br.CallTool(ctx, "read_file", json.RawMessage(`{"path":"/etc/passwd"}`))
}

// TestBridgeRejectsBlankedResult 最危险的一种改法：把一次成功的 read_file 的 result
// 清空，ok 仍留 true。旧代码「len(result) > 0 才解密」，于是直接把「空结果 + 成功」
// 交给调用方——AI 会得出「这个文件是空的」，而它没有任何别的办法察觉。
func TestBridgeRejectsBlankedResult(t *testing.T) {
	out, err := callWith(t, func(br *Bridge, id uint64) {
		m, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: id, OK: true})
		br.HandleInbound(m)
	})
	if err == nil {
		t.Fatalf("被清空 result 的响应竟返回成功: %s", out)
	}
	if !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("错误应指明未认证，实际: %v", err)
	}
}

// TestBridgeRejectsFlippedOK ok 是外层明文。把一次成功翻成失败（或反过来）不需要
// 任何密钥，除非 ok 进了 result 密文的 AAD。
func TestBridgeRejectsFlippedOK(t *testing.T) {
	_, err := callWith(t, func(br *Bridge, id uint64) {
		// 按「成功」加封，却标成失败发出。
		br.HandleInbound(respAs(t, id, `{"content":"root:x:0:0"}`, true, "", "", false, "exec_denied", "denied"))
	})
	if err == nil {
		t.Fatal("翻转 ok 的响应应被拒绝")
	}
	if !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("不能把攻击者伪造的 error_code 当真，实际: %v", err)
	}
}

// TestBridgeRejectsRewrittenErrorCode 反方向同理：把 permission_denied 改成
// file_not_found 会让调用方走完全不同的补救路径。
func TestBridgeRejectsRewrittenErrorCode(t *testing.T) {
	_, err := callWith(t, func(br *Bridge, id uint64) {
		br.HandleInbound(respAs(t, id, `{}`, false, "permission_denied", "denied", false, "file_not_found", "denied"))
	})
	if err == nil {
		t.Fatal("改了 error_code 的响应应被拒绝")
	}
	if !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("实际: %v", err)
	}
}

// TestBridgeAcceptsSealedFailure 认证通过的失败响应仍要原样报给调用方，别把
// 「对端确实报了错」也一起吞掉。
func TestBridgeAcceptsSealedFailure(t *testing.T) {
	_, err := callWith(t, func(br *Bridge, id uint64) {
		br.HandleInbound(respAs(t, id, `{}`, false, "file_not_found", "no such file", false, "file_not_found", "no such file"))
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "file_not_found") {
		t.Fatalf("应原样透出对端的 error_code，实际: %v", err)
	}
	if strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("合法的失败响应被误判成未认证: %v", err)
	}
}

// TestBridgeAcceptsSealedSuccess 正常路径不能被上面的收紧误伤。
func TestBridgeAcceptsSealedSuccess(t *testing.T) {
	out, err := callWith(t, func(br *Bridge, id uint64) {
		br.HandleInbound(respAs(t, id, `{"content":"ok"}`, true, "", "", true, "", ""))
	})
	if err != nil {
		t.Fatalf("合法响应应成功: %v", err)
	}
	if string(out) != `{"content":"ok"}` {
		t.Fatalf("result=%s", out)
	}
}

// TestBridgeDisconnectKeepsRealReasonWhenSealed 已握手（key 非零）时 Disconnect 唤醒的
// 在途调用，必须原样透出 tunnel_lost。
//
// 现有的 TestBridgeDisconnectWakesInflight 用的是零 key，正好绕开了响应认证那条路，
// 所以覆盖不到这里：响应认证要求「key 非零就必须带密文」，而 Disconnect 合成的结局
// 是本地造的、根本没有密文。不把本地结局和线上响应分开，隧道一断调用方看到的就是
// 「响应未加封（对端过旧，或响应被篡改）」——把一次普通掉线说成被人动了手脚。
func TestBridgeDisconnectKeepsRealReasonWhenSealed(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, streamKey) // 非零 key：走响应认证那条路

	done := make(chan error, 1)
	go func() {
		_, err := br.CallTool(context.Background(), "exec", json.RawMessage(`{"argv":["sleep"]}`))
		done <- err
	}()

	select {
	case <-conn.sent: // 等 pending 登记完成
	case <-time.After(2 * time.Second):
		t.Fatal("ToolReq 未在预期内发出")
	}
	br.Disconnect(nil)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("期望断开错误")
		}
		if !strings.Contains(err.Error(), "tunnel_lost") {
			t.Fatalf("应透出真正的原因 tunnel_lost，实际: %v", err)
		}
		if strings.Contains(err.Error(), "unauthenticated") {
			t.Fatalf("本地合成的断开结局被当成线上响应验真了: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Disconnect 后在途调用未返回")
	}
}
