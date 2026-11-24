package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// joinWithResumeProof 注册一个 share、让 help 带着给定证明加入，返回 share 收到的 SessionReady
// 与 help 的 JoinResponse。relay 不持有工具会话密钥，只负责透传。
func joinWithResumeProof(t *testing.T, proof *proto.P2PResumeProof) (proto.SessionReady, proto.JoinResponse) {
	t.Helper()
	srv, err := NewServer(&Config{CodeTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	shareConn := &reuseProtocolConn{remoteAddr: "203.0.113.7:1"}
	share := &ClientConn{ID: "share", Conn: shareConn}
	reg, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "1.2.3"})
	if srv.handleMessage(share, reg) {
		t.Fatal("注册不应断开 share")
	}
	frames := shareConn.messages(t)
	if len(frames) != 1 || frames[0].Type != proto.MsgRegisterResponse {
		t.Fatalf("share 应只收到 RegisterResponse: %+v", frames)
	}
	var registered proto.RegisterResponse
	if err := proto.DecodePayload(&frames[0], &registered); err != nil {
		t.Fatal(err)
	}

	helpConn := &reuseProtocolConn{remoteAddr: "198.51.100.9:2"}
	helper := &ClientConn{ID: "help", Conn: helpConn}
	join, _ := proto.NewMessage(proto.MsgJoinRequest, &proto.JoinRequest{
		Code: registered.Code, Version: "1.2.3", ResumeProof: proof,
	})
	if srv.handleMessage(helper, join) {
		t.Fatal("带证明的 join 不应被断开")
	}
	helpFrames := helpConn.messages(t)
	if len(helpFrames) != 1 || helpFrames[0].Type != proto.MsgJoinResponse {
		t.Fatalf("help 应只收到 JoinResponse: %+v", helpFrames)
	}
	var joined proto.JoinResponse
	if err := proto.DecodePayload(&helpFrames[0], &joined); err != nil {
		t.Fatal(err)
	}
	frames = shareConn.messages(t)
	last := frames[len(frames)-1]
	if last.Type != proto.MsgSessionReady {
		t.Fatalf("share 最后一帧应是 SessionReady: %+v", frames)
	}
	var ready proto.SessionReady
	if err := proto.DecodePayload(&last, &ready); err != nil {
		t.Fatal(err)
	}
	if shareConn.isClosed() || helpConn.isClosed() {
		t.Fatal("透传证明不应关闭任一端")
	}
	return ready, joined
}

// relay 必须把 help 的续会话证明原样放进 SessionReady；share 靠它判断是不是原协助端。
func TestJoinForwardsResumeProofToShare(t *testing.T) {
	proof := &proto.P2PResumeProof{Nonce: "bm9uY2Vub25jZW5vbmNlbg", MAC: "bWFjbWFjbWFjbWFjbWFjbQ"}
	ready, joined := joinWithResumeProof(t, proof)
	if !joined.Success || ready.SessionID != joined.SessionID {
		t.Fatalf("join 应成功且会话一致: join=%+v ready=%+v", joined, ready)
	}
	if ready.ResumeProof == nil || *ready.ResumeProof != *proof {
		t.Fatalf("SessionReady 应原样带上证明，实际=%+v", ready.ResumeProof)
	}
}

// 畸形 / 超长证明不转发，但 join 本身照常成功：share 收不到证明，按新协助端处理。
func TestJoinDropsMalformedResumeProof(t *testing.T) {
	for name, bad := range map[string]*proto.P2PResumeProof{
		"空 MAC":    {Nonce: "bm9uY2U", MAC: ""},
		"超长 MAC":   {Nonce: "bm9uY2U", MAC: strings.Repeat("A", 100)},
		"超长 nonce": {Nonce: strings.Repeat("A", 100), MAC: "bWFj"},
	} {
		t.Run(name, func(t *testing.T) {
			ready, joined := joinWithResumeProof(t, bad)
			if !joined.Success {
				t.Fatalf("证明畸形不应影响 join: %+v", joined)
			}
			if ready.ResumeProof != nil {
				t.Fatalf("畸形证明不得转发: %+v", ready.ResumeProof)
			}
		})
	}
}

// 普通 join（不带证明）时 SessionReady 不能凭空多出证明。
func TestJoinWithoutResumeProofSendsNone(t *testing.T) {
	ready, joined := joinWithResumeProof(t, nil)
	if !joined.Success {
		t.Fatalf("普通 join 应成功: %+v", joined)
	}
	if ready.ResumeProof != nil {
		t.Fatalf("普通 join 不应带证明: %+v", ready.ResumeProof)
	}
}
