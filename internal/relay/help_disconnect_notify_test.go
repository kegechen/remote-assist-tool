package relay

import (
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

func shrinkHelpDebounce(t *testing.T, d time.Duration) {
	t.Helper()
	orig := helpDisconnectDebounce
	helpDisconnectDebounce = d
	t.Cleanup(func() { helpDisconnectDebounce = orig })
}

// waitForMessages 轮询等待 conn 上攒够 want 条消息（去抖回调在独立 goroutine 里跑）。
func waitForMessages(t *testing.T, conn *reuseProtocolConn, want int, budget time.Duration) []proto.Message {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		msgs := conn.messages(t)
		if len(msgs) >= want {
			return msgs
		}
		if time.Now().After(deadline) {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Help 去抖窗口过完仍没回来 == 协助端真的走了，此时必须通知 share。
//
// 原先这条路是哑的：DisconnectClient 的 Help 分支返回的 DisconnectResult 里
// PeerToNotify 为 nil（只有 Share 分支填），去抖计时器清完 Help 槽也不发任何消息。
// share 于是继续挂在 relay 读上，要等 2 分钟读超时或协助码过期才回过神——而 share.go
// 里那段「协助端已断开连接，协助码仍有效」的处理从来没被触发过，是死代码。
func TestHelpDisconnectNotifiesShareAfterDebounce(t *testing.T) {
	shrinkHelpDebounce(t, 30*time.Millisecond)

	srv, err := NewServer(&Config{CodeTTL: time.Minute, CodeLength: 10})
	if err != nil {
		t.Fatal(err)
	}
	shareConn := &reuseProtocolConn{remoteAddr: "10.1.0.1:1001"}
	share := &ClientConn{ID: "share-1", ClientID: "cid-1", Conn: shareConn}
	if _, err := srv.sessions.CreateSession("SECRET1234", share, time.Minute, "cid-1", "10.1.0.1", 10, 100); err != nil {
		t.Fatal(err)
	}
	helpConn := &reuseProtocolConn{remoteAddr: "10.1.0.2:1002"}
	help := &ClientConn{ID: "help-1", Conn: helpConn}
	if _, err := srv.sessions.JoinSession("SECRET1234", help); err != nil {
		t.Fatal(err)
	}

	if result := srv.sessions.DisconnectClient(help.ID); result == nil {
		t.Fatal("Help 断连应返回 DisconnectResult")
	}
	// 去抖窗口内不该惊动 share。
	if msgs := shareConn.messages(t); len(msgs) != 0 {
		t.Fatalf("去抖窗口内 share 不应收到任何消息，实际=%v", msgs)
	}

	msgs := waitForMessages(t, shareConn, 1, 2*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("去抖结束后 share 应收到 1 条消息，实际=%v", msgs)
	}
	if msgs[0].Type != proto.MsgError {
		t.Fatalf("消息类型=%s，期望 %s", msgs[0].Type, proto.MsgError)
	}
	var errMsg proto.ErrorMessage
	if err := proto.DecodePayload(&msgs[0], &errMsg); err != nil {
		t.Fatal(err)
	}
	if errMsg.Code != proto.ErrCodePeerDisconnected {
		t.Fatalf("错误码=%q，期望 %q（share 端只认这一个码）", errMsg.Code, proto.ErrCodePeerDisconnected)
	}
}

// 协助端在去抖窗口内重连回来（网络抖动）时不得通知 share——那会让 share 白白把一个
// 健康会话当断开处理。去抖存在的全部意义就在这里。
func TestHelpReconnectWithinDebounceDoesNotNotifyShare(t *testing.T) {
	shrinkHelpDebounce(t, 200*time.Millisecond)

	srv, err := NewServer(&Config{CodeTTL: time.Minute, CodeLength: 10})
	if err != nil {
		t.Fatal(err)
	}
	shareConn := &reuseProtocolConn{remoteAddr: "10.1.0.1:1001"}
	share := &ClientConn{ID: "share-1", ClientID: "cid-1", Conn: shareConn}
	if _, err := srv.sessions.CreateSession("SECRET1234", share, time.Minute, "cid-1", "10.1.0.1", 10, 100); err != nil {
		t.Fatal(err)
	}
	oldHelp := &ClientConn{ID: "help-old", Conn: &reuseProtocolConn{remoteAddr: "10.1.0.2:1002"}}
	if _, err := srv.sessions.JoinSession("SECRET1234", oldHelp); err != nil {
		t.Fatal(err)
	}

	srv.sessions.DisconnectClient(oldHelp.ID)
	// 窗口内换上新协助端：Join 会替换 Help 槽，计时器随后发现 Help.ID 已不是
	// pendingHelpID，什么都不做。
	newHelp := &ClientConn{ID: "help-new", Conn: &reuseProtocolConn{remoteAddr: "10.1.0.3:1003"}}
	if _, err := srv.sessions.JoinSession("SECRET1234", newHelp); err != nil {
		t.Fatalf("去抖窗口内新协助端应能接管: %v", err)
	}

	time.Sleep(3 * helpDisconnectDebounce)
	for _, msg := range shareConn.messages(t) {
		if msg.Type != proto.MsgError {
			continue
		}
		var errMsg proto.ErrorMessage
		if err := proto.DecodePayload(&msg, &errMsg); err != nil {
			t.Fatal(err)
		}
		if errMsg.Code == proto.ErrCodePeerDisconnected {
			t.Fatal("协助端在去抖窗口内已重连，share 不该收到 PEER_DISCONNECTED")
		}
	}
}
