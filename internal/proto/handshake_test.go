package proto

import (
	"bytes"
	"testing"
)

func TestDeriveSessionKeyDeterministic(t *testing.T) {
	k1 := DeriveSessionKey("CODE-1234", "NONCE_S", "NONCE_H")
	k2 := DeriveSessionKey("CODE-1234", "NONCE_S", "NONCE_H")
	if !bytes.Equal(k1[:], k2[:]) {
		t.Fatal("expected deterministic derivation")
	}
}

func TestDeriveSessionKeyDiffersByNonce(t *testing.T) {
	k1 := DeriveSessionKey("CODE-1234", "A", "B")
	k2 := DeriveSessionKey("CODE-1234", "A", "C")
	if bytes.Equal(k1[:], k2[:]) {
		t.Fatal("expected different keys for different help nonces")
	}
}

func TestNewHelloHasRandomNonce(t *testing.T) {
	h1 := NewHello()
	h2 := NewHello()
	if h1.NonceB64 == h2.NonceB64 {
		t.Fatal("expected fresh random nonces")
	}
	if len(h1.NonceB64) < 16 {
		t.Fatalf("nonce too short: %s", h1.NonceB64)
	}
}
