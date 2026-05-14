package proto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestAEADSealOpenRoundtrip(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	plain := []byte(`{"argv":["ls","-l"]}`)
	ct, err := AEADSeal(&key, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	out, err := AEADOpen(&key, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("roundtrip mismatch: got %s", out)
	}
}

func TestAEADOpenRejectsTamper(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	ct, _ := AEADSeal(&key, []byte("hello"))
	ct[len(ct)-1] ^= 0x01
	if _, err := AEADOpen(&key, ct); err == nil {
		t.Fatal("expected open to fail on tampered ciphertext")
	}
}

func TestAEADOpenRejectsWrongKey(t *testing.T) {
	var k1, k2 [32]byte
	rand.Read(k1[:])
	rand.Read(k2[:])
	ct, _ := AEADSeal(&k1, []byte("hello"))
	if _, err := AEADOpen(&k2, ct); err == nil {
		t.Fatal("expected open to fail under wrong key")
	}
}
