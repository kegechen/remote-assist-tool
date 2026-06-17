package client

import (
	"net"
	"testing"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)

// createTestTunnelPair creates a connected pair of UDPTunnels for testing.
func createTestTunnelPair(t *testing.T) (*p2p.UDPTunnel, *p2p.UDPTunnel) {
	t.Helper()

	conn1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	conn2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		conn1.Close()
		t.Fatal(err)
	}

	addr1 := conn1.LocalAddr().(*net.UDPAddr)
	addr2 := conn2.LocalAddr().(*net.UDPAddr)

	t1 := p2p.NewUDPTunnel(conn1, addr2)
	t2 := p2p.NewUDPTunnel(conn2, addr1)
	return t1, t2
}

func TestP2PConnSendAndReceive(t *testing.T) {
	t1, t2 := createTestTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	sender := NewP2PConn(t1)
	receiver := NewP2PConn(t2)

	// Send a ToolReq message
	req := &proto.ToolReq{ID: 42, Tool: "exec", ArgsJSON: []byte(`{"argv":["ls"]}`)}
	if err := sender.SendMessage(proto.MsgToolReq, req); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	// Receive and verify
	msg, err := receiver.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if msg.Type != proto.MsgToolReq {
		t.Fatalf("expected MsgToolReq, got %s", msg.Type)
	}

	var decoded proto.ToolReq
	if err := proto.DecodePayload(msg, &decoded); err != nil {
		t.Fatalf("DecodePayload failed: %v", err)
	}
	if decoded.ID != 42 {
		t.Fatalf("expected ID=42, got %d", decoded.ID)
	}
	if decoded.Tool != "exec" {
		t.Fatalf("expected Tool=exec, got %s", decoded.Tool)
	}
}

func TestP2PConnBidirectional(t *testing.T) {
	t1, t2 := createTestTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	connA := NewP2PConn(t1)
	connB := NewP2PConn(t2)

	// A sends request, B sends response
	req := &proto.ToolReq{ID: 1, Tool: "stat", ArgsJSON: []byte(`{"path":"/"}`)}
	if err := connA.SendMessage(proto.MsgToolReq, req); err != nil {
		t.Fatalf("A SendMessage failed: %v", err)
	}

	msg, err := connB.ReadMessage()
	if err != nil {
		t.Fatalf("B ReadMessage failed: %v", err)
	}
	if msg.Type != proto.MsgToolReq {
		t.Fatalf("B expected MsgToolReq, got %s", msg.Type)
	}

	// B responds
	resp := &proto.ToolResp{ID: 1, OK: true, ResultJSON: []byte(`{"size":4096}`)}
	if err := connB.SendMessage(proto.MsgToolResp, resp); err != nil {
		t.Fatalf("B SendMessage failed: %v", err)
	}

	msg, err = connA.ReadMessage()
	if err != nil {
		t.Fatalf("A ReadMessage failed: %v", err)
	}
	if msg.Type != proto.MsgToolResp {
		t.Fatalf("A expected MsgToolResp, got %s", msg.Type)
	}

	var decoded proto.ToolResp
	if err := proto.DecodePayload(msg, &decoded); err != nil {
		t.Fatalf("DecodePayload failed: %v", err)
	}
	if !decoded.OK {
		t.Fatal("expected OK=true")
	}
	if decoded.ID != 1 {
		t.Fatalf("expected ID=1, got %d", decoded.ID)
	}
}

func TestP2PConnModeHeader(t *testing.T) {
	t1, t2 := createTestTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	sender := NewP2PConn(t1)

	// Send tool mode header
	if err := sender.WriteModeHeader(); err != nil {
		t.Fatalf("WriteModeHeader failed: %v", err)
	}

	// Read and detect on the other side
	toolMode, _, err := ReadModeHeader(t2)
	if err != nil {
		t.Fatalf("ReadModeHeader failed: %v", err)
	}
	if !toolMode {
		t.Fatal("expected tool mode, got SSH mode")
	}
}

func TestP2PConnModeHeaderSSHFallback(t *testing.T) {
	t1, t2 := createTestTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	// Simulate SSH data (starts with 'S' from "SSH-2.0-...")
	sshBanner := []byte("SS")
	if _, err := t1.Write(sshBanner); err != nil {
		t.Fatalf("Write SSH banner failed: %v", err)
	}

	toolMode, consumed, err := ReadModeHeader(t2)
	if err != nil {
		t.Fatalf("ReadModeHeader failed: %v", err)
	}
	if toolMode {
		t.Fatal("expected SSH mode, got tool mode")
	}
	// Consumed bytes should be the SSH banner prefix
	if consumed[0] != 'S' || consumed[1] != 'S' {
		t.Fatalf("expected consumed='SS', got %q", consumed)
	}
}

func TestP2PConnModeHeaderThenMessages(t *testing.T) {
	t1, t2 := createTestTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	sender := NewP2PConn(t1)

	// Send mode header, then a tool message
	if err := sender.WriteModeHeader(); err != nil {
		t.Fatalf("WriteModeHeader failed: %v", err)
	}
	req := &proto.ToolReq{ID: 99, Tool: "stat", ArgsJSON: []byte(`{"path":"/"}`)}
	if err := sender.SendMessage(proto.MsgToolReq, req); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	// Receiver side: detect mode, then read message
	toolMode, _, err := ReadModeHeader(t2)
	if err != nil {
		t.Fatalf("ReadModeHeader failed: %v", err)
	}
	if !toolMode {
		t.Fatal("expected tool mode")
	}

	receiver := NewP2PConn(t2)
	msg, err := receiver.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if msg.Type != proto.MsgToolReq {
		t.Fatalf("expected MsgToolReq, got %s", msg.Type)
	}
	var decoded proto.ToolReq
	if err := proto.DecodePayload(msg, &decoded); err != nil {
		t.Fatalf("DecodePayload failed: %v", err)
	}
	if decoded.ID != 99 {
		t.Fatalf("expected ID=99, got %d", decoded.ID)
	}
}

func TestP2PConnMultipleMessages(t *testing.T) {
	t1, t2 := createTestTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	sender := NewP2PConn(t1)
	receiver := NewP2PConn(t2)

	// Send 10 messages in sequence
	const count = 10
	for i := uint64(1); i <= count; i++ {
		req := &proto.ToolReq{ID: i, Tool: "exec", ArgsJSON: []byte(`{}`)}
		if err := sender.SendMessage(proto.MsgToolReq, req); err != nil {
			t.Fatalf("SendMessage %d failed: %v", i, err)
		}
	}

	// Receive all 10 in order
	for i := uint64(1); i <= count; i++ {
		msg, err := receiver.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage %d failed: %v", i, err)
		}
		var decoded proto.ToolReq
		if err := proto.DecodePayload(msg, &decoded); err != nil {
			t.Fatalf("DecodePayload %d failed: %v", i, err)
		}
		if decoded.ID != i {
			t.Fatalf("message %d: expected ID=%d, got %d", i, i, decoded.ID)
		}
	}
}
