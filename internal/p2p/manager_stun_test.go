package p2p

import (
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// captureRelay 记录 P2PManager 经 relay 发出的消息。
type captureRelay struct {
	mu   sync.Mutex
	msgs []*proto.Message
}

func (c *captureRelay) SendMessage(t proto.MessageType, payload interface{}) error {
	m, err := proto.NewMessage(t, payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	return nil
}
func (c *captureRelay) ReadMessage() (*proto.Message, error) { select {} }
func (c *captureRelay) Close()                               {}

func (c *captureRelay) advertise(t *testing.T) proto.PeerAddrAdvertise {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if m.Type == proto.MsgPeerAddrAdvertise {
			var a proto.PeerAddrAdvertise
			if err := proto.DecodePayload(m, &a); err != nil {
				t.Fatal(err)
			}
			return a
		}
	}
	t.Fatal("no PeerAddrAdvertise sent")
	return proto.PeerAddrAdvertise{}
}

// relay 自己的 3478 不通（Windows relay 安全组的情形）时，用 relay 下发的候选拿到公网
// 地址，不再先等满 16 秒；上报里带上实际采用的 STUN 与耗时，供 relay 统计。
func TestManagerUsesAdvertisedSTUNWhenRelaySTUNDead(t *testing.T) {
	dead := fakeSTUN(t, "127.0.0.1", neverReply, nil)
	live := fakeSTUN(t, "127.0.0.1", alwaysReply, nil)

	mgr := NewP2PManager(P2PModeAuto, dead, "127.0.0.1")
	mgr.SetSTUNCandidates([]string{live})
	mgr.fallbackSTUN = nil // 测试不碰公网
	relay := &captureRelay{}
	mgr.SetRelayConn(relay)

	start := time.Now()
	if _, err := mgr.Start("sess-1", true); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Start took %v; dead relay STUN must not block discovery", elapsed)
	}

	a := relay.advertise(t)
	if a.PublicAddr == "" {
		t.Fatal("public address not discovered")
	}
	if a.STUNServer != live {
		t.Fatalf("stun_server = %q, want %q", a.STUNServer, live)
	}
	if a.STUNMs < 0 || a.STUNMs > 2000 {
		t.Fatalf("stun_ms = %d", a.STUNMs)
	}
	// UDP 中继仍只指向 relay 自己的 STUN，不随下发列表改变。
	if mgr.stunAddr == nil || mgr.stunAddr.String() != dead {
		t.Fatalf("UDP relay addr = %v, want relay's own %s", mgr.stunAddr, dead)
	}
}

// 没有配置 STUN（standalone/loopback relay）且没有下发时，保持现状：不做任何 STUN 发现，
// 也不去碰内置的公共服务器。
func TestManagerNoSTUNConfiguredSkipsDiscovery(t *testing.T) {
	mgr := NewP2PManager(P2PModeAuto, "", "127.0.0.1")
	relay := &captureRelay{}
	mgr.SetRelayConn(relay)
	start := time.Now()
	if _, err := mgr.Start("sess-2", true); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Start took %v without any STUN configured", time.Since(start))
	}
	if a := relay.advertise(t); a.PublicAddr != "" || a.STUNServer != "" {
		t.Fatalf("advertise = %+v, want no public address / stun", a)
	}
}
