package client

import (
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

func TestHandleHelloProducesAck(t *testing.T) {
	hello := proto.NewHello()
	ack, key := buildHelloAck(hello, "CODE-1234")
	if !ack.Accept {
		t.Fatalf("expected accept, got %+v", ack)
	}
	derived := proto.DeriveSessionKey("CODE-1234", ack.NonceB64, hello.NonceB64)
	if derived != key {
		t.Fatal("key derivation mismatch")
	}
}

func TestHandleHelloRejectsBadVersion(t *testing.T) {
	hello := proto.NewHello()
	hello.Version = "999"
	ack, _ := buildHelloAck(hello, "CODE-1234")
	if ack.Accept {
		t.Fatal("expected reject for unknown version")
	}
}
