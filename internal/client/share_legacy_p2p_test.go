package client

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// 协商到 v1 之后 share 端怎么收拾 P2P。
//
// 分工：--p2p required 在**握手阶段**就被拒（P2P 与 v1 对端注定谈不成，而用户明确
// 表示不接受中转，这个会话从一开始就不成立），所以 refuseP2PForLegacyPeer 只需处理
// auto —— 没有 P2P 只是功能降级，会话继续走中转。

func TestRefuseP2PForLegacyPeerAutoStaysOnRelay(t *testing.T) {
	s := &ShareMode{client: NewClient(&Config{P2PMode: "auto"})}
	s.refuseP2PForLegacyPeer()
	if s.client.IsClosed() {
		t.Fatal("auto 模式下没有 P2P 只是功能降级，会话必须继续走中转")
	}
}

func TestRefuseP2PForLegacyPeerDisabledIsNoop(t *testing.T) {
	s := &ShareMode{client: NewClient(&Config{P2PMode: "disabled"})}
	s.refuseP2PForLegacyPeer()
	if s.client.IsClosed() {
		t.Fatal("--p2p disabled 本就不要 P2P，不该因为对端旧而拆会话")
	}
}

// TestRequiredModeRejectsLegacyAtHandshake --p2p required 遇上 v1 对端必须在握手阶段
// 带理由拒绝，而不是先 Accept 再掐断连接。
//
// 后者在对端那里只看得到"握手成功 → tunnel_lost → 重连"的无理由循环，真正的原因
// 只印在本机控制台上。回一条带理由的 Accept:false 才能把话送到对端终端 —— 0.0.x 会把
// ErrorMsg 原样打印出来，而那是旧版用户唯一的线索。
func TestRequiredModeRejectsLegacyAtHandshake(t *testing.T) {
	s := &ShareMode{
		code:     "CODE-1234",
		minProto: proto.ToolProtocolVersionV1, // 开了兼容模式，否则版本协商本身就会拒
	}
	// 0.0.x 形状的 Hello：只有 version 字段，没有 versions。
	hello := proto.Hello{Version: proto.ToolProtocolVersionV1, NonceB64: proto.NewNonceB64()}
	msg, err := proto.NewMessage(proto.MsgToolHello, &hello)
	if err != nil {
		t.Fatal(err)
	}

	// net.Pipe 是同步的：handleRelayToolHello 里的 SendMessage 会一直阻塞到对端读走，
	// 所以得让读方先就位。
	shareConn, helpConn := net.Pipe()
	t.Cleanup(func() { shareConn.Close(); helpConn.Close() })
	s.client = &Client{
		config: &Config{P2PMode: "required"}, // 代码要读 client.config.P2PMode，不能省
		conn:   shareConn,
		enc:    json.NewEncoder(shareConn),
		dec:    json.NewDecoder(shareConn),
	}

	ackCh := make(chan proto.HelloAck, 1)
	go func() {
		var m proto.Message
		if err := json.NewDecoder(helpConn).Decode(&m); err != nil {
			return
		}
		var a proto.HelloAck
		proto.DecodePayload(&m, &a)
		ackCh <- a
	}()

	if err := s.handleRelayToolHello(msg); err != nil {
		t.Fatalf("handleRelayToolHello: %v", err)
	}

	var ack proto.HelloAck
	select {
	case ack = <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("未收到 HelloAck")
	}
	if ack.Accept {
		t.Fatal("--p2p required 遇上 v1 对端必须在握手阶段拒绝，不能先 Accept 再掐断")
	}
	if !strings.Contains(ack.ErrorMsg, "p2p") && !strings.Contains(ack.ErrorMsg, "P2P") {
		t.Fatalf("拒绝理由必须说明是 P2P 要求导致的，实际：%s", ack.ErrorMsg)
	}
	if !strings.Contains(ack.ErrorMsg, "--p2p auto") {
		t.Fatalf("拒绝理由应给出可操作的出路，实际：%s", ack.ErrorMsg)
	}
	// 握手被拒 ⟹ 没有会话，daemon 也不该被建起来。
	if s.currentDaemonSess().Active() {
		t.Fatal("被拒的握手不该留下可用会话")
	}
	if s.currentDaemon() != nil {
		t.Fatal("被拒的握手不该建立 daemon")
	}
}
