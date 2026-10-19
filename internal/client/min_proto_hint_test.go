package client

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)

// legacyRejectAck 0.0.9 share 拒绝 v2 Hello 时回的应答：Accept=false、只报自己的 "1"、
// 不带 versions（见 0.0.9 的 internal/client/share.go）。
func legacyRejectAck() proto.HelloAck {
	return proto.HelloAck{Version: proto.ToolProtocolVersionV1, Accept: false}
}

// 版本不兼容的错误要同时服务两类读者：命令行用户看到的文本必须和改前逐字一致
// （LocalHint），而 MCP bootstrap 要能从错误链里取回原始错误，换成 connect 能照做的指引。
func TestSessionFromAckKeepsIncompatibleError(t *testing.T) {
	h := &HelpMode{code: "ABCDEFGHIJ"}
	_, err := h.sessionFromAck(legacyRejectAck(), proto.NewHello(""))
	if err == nil {
		t.Fatal("默认下限下应拒绝只支持 v1 的对端")
	}
	var incompat *proto.IncompatibleVersionError
	if !errors.As(err, &incompat) {
		t.Fatalf("错误链里应能取回 IncompatibleVersionError，实际 %T: %v", err, err)
	}
	if err.Error() != incompat.LocalHint() {
		t.Fatalf("命令行文本应保持为 LocalHint\n got: %s\nwant: %s", err.Error(), incompat.LocalHint())
	}
}

// MCP 指引：要指向 connect 的 min_proto，要求先征得用户同意，且绝不能引导重启或改启动参数
// ——AI 照"加 --min-proto=1 重启"去做，就是改宿主配置、另起进程。
func TestMCPIncompatHintPointsToConnect(t *testing.T) {
	h := &HelpMode{code: "ABCDEFGHIJ"}
	_, err := h.sessionFromAck(legacyRejectAck(), proto.NewHello(""))
	var incompat *proto.IncompatibleVersionError
	if !errors.As(err, &incompat) {
		t.Fatalf("取不到 IncompatibleVersionError: %v", err)
	}
	hint := mcpIncompatHint(incompat)
	for _, must := range []string{`min_proto="1"`, "同意", "v1"} {
		if !strings.Contains(hint, must) {
			t.Fatalf("MCP 指引缺少 %q:\n%s", must, hint)
		}
	}
	for _, mustNot := range []string{"重启", "--min-proto"} {
		if strings.Contains(hint, mustNot) {
			t.Fatalf("MCP 指引不应出现 %q:\n%s", mustNot, hint)
		}
	}
}

// 放宽之后与 0.0.x 谈成 v1：会话版本必须是 v1，connect 结果的 warning 才有依据。
func TestSessionFromAckAcceptsLegacyWhenRelaxed(t *testing.T) {
	h := &HelpMode{code: "ABCDEFGHIJ", minProto: proto.ToolProtocolVersionV1}
	hello := proto.NewFallbackHello(proto.ToolProtocolVersionV1)
	ack := proto.HelloAck{Version: proto.ToolProtocolVersionV1, Accept: true, NonceB64: hello.NonceB64}
	sess, err := h.sessionFromAck(ack, hello)
	if err != nil {
		t.Fatalf("min_proto=1 时应接受 v1: %v", err)
	}
	if sess.Version != proto.ToolProtocolVersionV1 {
		t.Fatalf("会话版本 = %q, want 1", sess.Version)
	}
}

// P2P 断开回中转重新握手时，版本必须与首次握手一致。下限放宽到 v1 的会话里，一条
// Version:"1" 的应答能过 sessionFromAck；照单全收的话会话就在中途悄悄掉到 v1，而
// connect 结果、复用判断、GUI 状态都还以为是 v2。版本一致（本就是 v1 会话）时照常切回。
func TestDowngradeToRelayRejectsVersionChange(t *testing.T) {
	if err := runDowngradeToRelay(t, proto.ToolProtocolVersionV2); !errors.Is(err, errVersionChangedMidSession) {
		t.Fatalf("v2 会话回中转时谈出 v1 应拆除会话，实际 teardown=%v", err)
	}
	if err := runDowngradeToRelay(t, proto.ToolProtocolVersionV1); err != nil {
		t.Fatalf("版本一致时应照常切回中转，实际 teardown=%v", err)
	}
}

// runDowngradeToRelay 让对端在重新握手时应答 v1，返回 teardown 收到的错误（未拆除为 nil）。
func runDowngradeToRelay(t *testing.T, sessVersion string) error {
	t.Helper()
	helpConn, peerConn := net.Pipe()
	t.Cleanup(func() { helpConn.Close(); peerConn.Close() })
	// 对端读到 ToolHello 后，应答经 helloAckCh 投递（与真实读循环一致）。必须在读到
	// Hello 之后才投：downgradeToRelay 发 Hello 前会先排空通道里的陈旧应答。
	ackCh := make(chan proto.HelloAck, 1)
	go func() {
		var msg proto.Message
		if json.NewDecoder(peerConn).Decode(&msg) != nil {
			return
		}
		ackCh <- proto.HelloAck{Version: proto.ToolProtocolVersionV1, Accept: true, NonceB64: "AAAAAAAAAAAAAAAAAAAAAA=="}
		io.Copy(io.Discard, peerConn)
	}()

	h := &HelpMode{
		code:     "ABCDEFGHIJ",
		minProto: proto.ToolProtocolVersionV1,
		client:   &Client{conn: helpConn, enc: json.NewEncoder(helpConn), dec: json.NewDecoder(helpConn)},
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	pc := NewP2PConn(p2p.NewUDPTunnel(udp, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}))
	var p2pPtr atomic.Pointer[P2PConn]
	p2pPtr.Store(pc)

	var tornDown error
	b := NewHelpMCPBootstrap(&Config{})
	b.downgradeToRelay(h, mcp.NewBridge(nil, proto.Session{}), pc, &p2pPtr, ackCh,
		func(err error) { tornDown = err }, errors.New("p2p closed"), sessVersion)
	return tornDown
}
