package tests

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/proto"
)

// 工具通道 v1/v2 的端到端互通测试。
//
// 光验证"协商出了哪个版本号"是不够的：v1 与 v2 的真正差别散落在四处加解密调用点
// （请求 args、响应 result、流帧、抗重放），每一处的收发两侧都必须做出**对称**的选择。
// 任何一侧记错，协商照样成功，然后每条请求以 decrypt_failed 收场——那正是版本协商本该
// 避免的失败形态。所以这里把 mcp.Bridge 和 agent.Daemon 真的对接起来跑。
//
// v1 一侧的预期行为是照着 0.0.x 的实现定的：AAD 为 nil、空 args 不加封、空 result 不加封。

// loopConn 把一端发出的消息直接投给另一端。
//
// 异步投递是必须的：Daemon.handleReq 在处理请求的过程中会回发响应，同步投递会在
// Bridge 的 HandleInbound 里重入，两边的锁立刻互等。
type loopConn struct {
	deliver func(*proto.Message)
}

func (c *loopConn) SendMessage(t proto.MessageType, p interface{}) error {
	msg, err := proto.NewMessage(t, p)
	if err != nil {
		return err
	}
	go c.deliver(msg)
	return nil
}

// echoTool 把收到的 args 原样回显，用来确认参数确实被正确解密了。
type echoTool struct{}

func (echoTool) Name() string { return "echo" }
func (echoTool) Run(ctx context.Context, args json.RawMessage, sink agent.StreamSink) (json.RawMessage, error) {
	if len(args) == 0 {
		return json.RawMessage(`{"args":null}`), nil
	}
	return json.RawMessage(`{"args":` + string(args) + `}`), nil
}

// emptyTool 返回空 result，覆盖"响应方向的空密文"这条分支——v1 不封、v2 必须封。
type emptyTool struct{}

func (emptyTool) Name() string { return "empty" }
func (emptyTool) Run(ctx context.Context, args json.RawMessage, sink agent.StreamSink) (json.RawMessage, error) {
	return nil, nil
}

// newToolPair 建一对对接好的 Bridge/Daemon。两端各自的会话版本可以不同，用来构造
// 版本错配的场景。
func newToolPair(t *testing.T, helpSess, shareSess proto.Session) *mcp.Bridge {
	t.Helper()

	reg := agent.NewRegistry()
	reg.Register(echoTool{})
	reg.Register(emptyTool{})

	var bridge *mcp.Bridge
	daemon := agent.NewDaemon(reg, &loopConn{deliver: func(m *proto.Message) {
		bridge.HandleInbound(m)
	}}, shareSess)
	bridge = mcp.NewBridge(&loopConn{deliver: daemon.Inject}, helpSess)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go daemon.RunLoop(ctx)
	return bridge
}

func sessionFor(t *testing.T, version string) proto.Session {
	t.Helper()
	return proto.Session{
		Key:     proto.DeriveSessionKey("CODE-1234", "nonce-share", "nonce-help", version),
		Version: version,
	}
}

// TestToolChannelRoundTripPerVersion v1 与 v2 会话都必须能完整跑通一次带参调用。
//
// v1 这一行是本次改动的核心保证：--min-proto=1 之后，与 0.0.x 对端的通道不只是"握上手"，
// 而是真的能用。
func TestToolChannelRoundTripPerVersion(t *testing.T) {
	for _, version := range []string{proto.ToolProtocolVersionV1, proto.ToolProtocolVersionV2} {
		t.Run("v"+version, func(t *testing.T) {
			sess := sessionFor(t, version)
			bridge := newToolPair(t, sess, sess)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := bridge.CallTool(ctx, "echo", json.RawMessage(`{"k":"v"}`))
			if err != nil {
				t.Fatalf("v%s 调用失败: %v", version, err)
			}
			if !strings.Contains(string(out), `"k":"v"`) {
				t.Fatalf("v%s 参数未正确送达: %s", version, out)
			}
		})
	}
}

// TestToolChannelEmptyArgsPerVersion 无参调用在两个版本下都要能用。
//
// 两个版本的发送侧都把空参数封成 "{}"。v1 这边刻意**不**复刻 0.0.x 的原样行为——它
// 对空参数发 args:null 不加封，而它自己的接收侧判据也是 len > 0，于是拿 4 字节的
// "null" 去解密并回 decrypt_failed，也就是说 v1↔v1 的无参调用本来就是坏的。真实
// 0.0.x 的接收侧完全能解开我们封的 "{}"，照常封是严格更好且兼容的做法。
func TestToolChannelEmptyArgsPerVersion(t *testing.T) {
	for _, version := range []string{proto.ToolProtocolVersionV1, proto.ToolProtocolVersionV2} {
		t.Run("v"+version, func(t *testing.T) {
			sess := sessionFor(t, version)
			bridge := newToolPair(t, sess, sess)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := bridge.CallTool(ctx, "echo", nil); err != nil {
				t.Fatalf("v%s 无参调用失败: %v", version, err)
			}
		})
	}
}

// TestToolChannelEmptyResultPerVersion 空 result 的响应在两个版本下都要能用。
func TestToolChannelEmptyResultPerVersion(t *testing.T) {
	for _, version := range []string{proto.ToolProtocolVersionV1, proto.ToolProtocolVersionV2} {
		t.Run("v"+version, func(t *testing.T) {
			sess := sessionFor(t, version)
			bridge := newToolPair(t, sess, sess)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := bridge.CallTool(ctx, "empty", json.RawMessage(`{}`)); err != nil {
				t.Fatalf("v%s 空结果调用失败: %v", version, err)
			}
		})
	}
}

// TestToolChannelVersionMismatchFails 两端版本不一致时必须失败。
//
// 这条反向锁定了"版本必须由协商统一确定"：密钥本身带版本（HKDF info），行为开关也带
// 版本，任一侧擅自用另一个版本，结果都不该是"悄悄能用"。
func TestToolChannelVersionMismatchFails(t *testing.T) {
	cases := []struct{ name, help, share string }{
		{"help为v2_share为v1", proto.ToolProtocolVersionV2, proto.ToolProtocolVersionV1},
		{"help为v1_share为v2", proto.ToolProtocolVersionV1, proto.ToolProtocolVersionV2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := newToolPair(t, sessionFor(t, tc.help), sessionFor(t, tc.share))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := bridge.CallTool(ctx, "echo", json.RawMessage(`{"k":"v"}`)); err == nil {
				t.Fatal("版本错配的两端不应能成功通信")
			}
		})
	}
}
