package client

import (
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

func TestHandleHelloProducesAck(t *testing.T) {
	hello := proto.NewHello("")
	ack, sess, err := buildHelloAck(hello, "CODE-1234", "")
	if err != nil {
		t.Fatalf("unexpected negotiation error: %v", err)
	}
	if !ack.Accept {
		t.Fatalf("expected accept, got %+v", ack)
	}
	if ack.Version != proto.ToolProtocolVersion {
		t.Fatalf("ack 应选定最高版本，得到 %q", ack.Version)
	}
	derived := proto.DeriveSessionKey("CODE-1234", ack.NonceB64, hello.NonceB64, proto.ToolProtocolVersion)
	if derived != sess.Key {
		t.Fatal("key derivation mismatch")
	}
}

func TestHandleHelloRejectsBadVersion(t *testing.T) {
	hello := proto.NewHello("")
	hello.Version = "999"
	hello.Versions = []string{"999"}
	ack, _, err := buildHelloAck(hello, "CODE-1234", "")
	if ack.Accept {
		t.Fatal("expected reject for unknown version")
	}
	if err == nil {
		t.Fatal("拒绝时必须返回原因，否则本机没有任何可打印的提示")
	}
}

// TestHandleHelloRejectsLegacyByDefault 锁定默认不降级：0.0.x 的 help 只会发
// version:"1" 且没有 versions 字段，默认配置下必须拒绝。
//
// 同时锁定拒绝理由是**可操作**的——ErrorMsg 会被 0.0.x 的 help 原样打印出来，
// 那是旧客户端用户唯一能看到的提示，必须告诉他该怎么办，而不是只说一句版本不对。
func TestHandleHelloRejectsLegacyByDefault(t *testing.T) {
	hello := proto.Hello{Version: proto.ToolProtocolVersionV1, NonceB64: proto.NewNonceB64()}
	ack, sess, err := buildHelloAck(hello, "CODE-1234", "")
	if ack.Accept {
		t.Fatal("默认配置不得接受 v1 对端")
	}
	if sess.Active() {
		t.Fatal("拒绝时不得派生出可用会话")
	}
	if err == nil {
		t.Fatal("拒绝 v1 必须返回原因")
	}
	if !strings.Contains(ack.ErrorMsg, "--min-proto=1") {
		t.Fatalf("ErrorMsg 必须给出可操作的兼容办法，实际：%s", ack.ErrorMsg)
	}
	// 这条提示要送达的是 share 端的用户（旧版 help 没有 --min-proto 可加）。
	if !strings.Contains(ack.ErrorMsg, proto.SideShare) {
		t.Fatalf("ErrorMsg 必须指明该在被协助端加参数，实际：%s", ack.ErrorMsg)
	}
}

// TestHandleHelloAcceptsLegacyWhenAllowed 放宽到 v1 后必须真的谈成 v1，并且密钥
// 要按 v1 的 HKDF info 派生——否则握手看着成功了，之后每条请求都 decrypt_failed。
func TestHandleHelloAcceptsLegacyWhenAllowed(t *testing.T) {
	hello := proto.Hello{Version: proto.ToolProtocolVersionV1, NonceB64: proto.NewNonceB64()}
	ack, sess, err := buildHelloAck(hello, "CODE-1234", proto.ToolProtocolVersionV1)
	if err != nil {
		t.Fatalf("放宽到 v1 后不应失败: %v", err)
	}
	if !ack.Accept {
		t.Fatalf("放宽到 v1 后应接受，得到 %+v", ack)
	}
	if ack.Version != proto.ToolProtocolVersionV1 {
		t.Fatalf("应选定 v1，得到 %q", ack.Version)
	}
	if !sess.Legacy() {
		t.Fatal("会话应标记为 v1")
	}
	want := proto.DeriveSessionKey("CODE-1234", ack.NonceB64, hello.NonceB64, proto.ToolProtocolVersionV1)
	if sess.Key != want {
		t.Fatal("v1 会话必须用 v1 的 HKDF info 派生密钥")
	}
}

// TestHandleHelloPrefersV2WhenBothSupported --min-proto=1 只是放宽下限，不是强制降级：
// 两端都支持 v2 时仍然必须谈成 v2。否则开了兼容开关的机器会在所有连接上丢掉 AAD。
func TestHandleHelloPrefersV2WhenBothSupported(t *testing.T) {
	hello := proto.NewHello(proto.ToolProtocolVersionV1) // 新版 help 开了兼容模式
	ack, sess, err := buildHelloAck(hello, "CODE-1234", proto.ToolProtocolVersionV1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ack.Version != proto.ToolProtocolVersion || sess.Legacy() {
		t.Fatalf("双方都支持 v2 时必须谈成 v2，实际 %q", ack.Version)
	}
}
