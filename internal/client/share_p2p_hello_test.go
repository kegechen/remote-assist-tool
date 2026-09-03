package client

import (
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// TestP2PToolHelloIgnoredAfterRelayHandshake 回归：relay 上已经握过手之后，P2P 隧道
// 上再来的 ToolHello 必须被忽略。
//
// 合法的新版 help 不会发它（它只在隧道上探活并复用 relay 协商出的 key），所以这种帧
// 只可能来自往隧道里注入的第三方——UDP relay 回退模式下隧道的来源判定是「等于 STUN
// 服务器地址」，任何经 relay 转发进来的帧都能过关。放行的话 ensureDaemon 会用攻击者
// 的 nonce 重新派生 daemonKey，合法 help 此后每条请求都 decrypt_failed。
func TestP2PToolHelloIgnoredAfterRelayHandshake(t *testing.T) {
	shareTunnel, peerTunnel := createTestTunnelPair(t)
	defer shareTunnel.Close()
	defer peerTunnel.Close()

	const epoch = 7
	key := [32]byte{1, 2, 3}
	s := &ShareMode{code: "ABCD-2345"}
	s.p2pEpoch = epoch
	s.daemonKey = key

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleToolOverP2P(shareTunnel, epoch)
	}()

	peer := NewP2PConn(peerTunnel)
	hello := proto.NewHello()
	if err := peer.SendMessage(proto.MsgToolHello, hello); err != nil {
		t.Fatalf("发送 ToolHello: %v", err)
	}

	// 修复前这里会收到 ToolHelloAck；修复后应当读超时。
	msg, err := peer.ReadMessageTimeout(2 * time.Second)
	if err == nil {
		t.Fatalf("relay 握手后仍响应了 P2P ToolHello: %s", msg.Type)
	}

	if got := s.currentDaemonKey(); got != key {
		t.Errorf("会话密钥被注入的 ToolHello 改写了: %x", got)
	}

	peerTunnel.Close()
	shareTunnel.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("handleToolOverP2P 未退出")
	}
}
