package p2p

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func createTunnelPair(t *testing.T) (*UDPTunnel, *UDPTunnel) {
	t.Helper()

	addr1, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr2, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	conn1, err := net.ListenUDP("udp", addr1)
	if err != nil {
		t.Fatal(err)
	}
	conn2, err := net.ListenUDP("udp", addr2)
	if err != nil {
		t.Fatal(err)
	}

	resolvedAddr1 := conn1.LocalAddr().(*net.UDPAddr)
	resolvedAddr2 := conn2.LocalAddr().(*net.UDPAddr)

	tunnel1 := NewUDPTunnel(conn1, resolvedAddr2)
	tunnel2 := NewUDPTunnel(conn2, resolvedAddr1)

	return tunnel1, tunnel2
}

func TestTunnelBasicRoundtrip(t *testing.T) {
	t1, t2 := createTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	msg := []byte("hello world")

	// Write from t1, read from t2
	n, err := t1.Write(msg)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("Write returned %d, want %d", n, len(msg))
	}

	buf := make([]byte, 1024)
	n, err = t2.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Fatalf("got %q, want %q", buf[:n], msg)
	}
}

func TestTunnelMultipleMessages(t *testing.T) {
	t1, t2 := createTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	messages := []string{"msg1", "msg2", "msg3", "msg4", "msg5"}

	for _, msg := range messages {
		if _, err := t1.Write([]byte(msg)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Small delay to let packets arrive
	time.Sleep(50 * time.Millisecond)

	for _, expected := range messages {
		buf := make([]byte, 1024)
		n, err := t2.Read(buf)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if string(buf[:n]) != expected {
			t.Fatalf("got %q, want %q", buf[:n], expected)
		}
	}
}

func TestTunnelLargeData(t *testing.T) {
	t1, t2 := createTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	// Send data larger than maxPacketSize to test chunking
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}

	n, err := t1.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d", n, len(data))
	}

	// Read all data (may come in multiple reads due to chunking)
	var received []byte
	buf := make([]byte, 2048)
	for len(received) < len(data) {
		n, err := t2.Read(buf)
		if err != nil {
			t.Fatalf("Read failed after %d bytes: %v", len(received), err)
		}
		received = append(received, buf[:n]...)
	}

	if !bytes.Equal(received, data) {
		t.Fatalf("data mismatch: got %d bytes, want %d bytes", len(received), len(data))
	}
}

func TestTunnelBidirectional(t *testing.T) {
	t1, t2 := createTunnelPair(t)
	defer t1.Close()
	defer t2.Close()

	msg1 := []byte("from tunnel 1")
	msg2 := []byte("from tunnel 2")

	// Send in both directions simultaneously
	errCh := make(chan error, 2)
	go func() {
		_, err := t1.Write(msg1)
		errCh <- err
	}()
	go func() {
		_, err := t2.Write(msg2)
		errCh <- err
	}()

	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Read from both sides
	buf := make([]byte, 1024)

	n, err := t2.Read(buf)
	if err != nil {
		t.Fatalf("Read from t2 failed: %v", err)
	}
	if !bytes.Equal(buf[:n], msg1) {
		t.Fatalf("t2 got %q, want %q", buf[:n], msg1)
	}

	n, err = t1.Read(buf)
	if err != nil {
		t.Fatalf("Read from t1 failed: %v", err)
	}
	if !bytes.Equal(buf[:n], msg2) {
		t.Fatalf("t1 got %q, want %q", buf[:n], msg2)
	}
}

func TestTunnelCloseUnblocksRead(t *testing.T) {
	t1, t2 := createTunnelPair(t)
	defer t2.Close()

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1024)
		_, err := t1.Read(buf)
		if err == nil {
			t.Error("expected error from Read after Close")
		}
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	t1.Close()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}
