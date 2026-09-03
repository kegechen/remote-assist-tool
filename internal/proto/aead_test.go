package proto

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"
)

func TestAEADSealOpenRoundtrip(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	plain := []byte(`{"argv":["ls","-l"]}`)
	ct, err := AEADSeal(&key, plain, nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	out, err := AEADOpen(&key, ct, nil)
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
	ct, _ := AEADSeal(&key, []byte("hello"), nil)
	ct[len(ct)-1] ^= 0x01
	if _, err := AEADOpen(&key, ct, nil); err == nil {
		t.Fatal("expected open to fail on tampered ciphertext")
	}
}

func TestAEADOpenRejectsWrongKey(t *testing.T) {
	var k1, k2 [32]byte
	rand.Read(k1[:])
	rand.Read(k2[:])
	ct, _ := AEADSeal(&k1, []byte("hello"), nil)
	if _, err := AEADOpen(&k2, ct, nil); err == nil {
		t.Fatal("expected open to fail under wrong key")
	}
}

func TestAEADSealOpenJSONRoundtrip(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	plain := []byte(`{"path":"/etc/passwd","offset":0}`)
	wrapped, err := AEADSealJSON(&key, plain, nil)
	if err != nil {
		t.Fatalf("SealJSON: %v", err)
	}
	// wrapped 应该是合法 JSON string literal
	var s string
	if err := json.Unmarshal(wrapped, &s); err != nil {
		t.Fatalf("wrapped is not valid JSON string: %v", err)
	}
	out, err := AEADOpenJSON(&key, wrapped, nil)
	if err != nil {
		t.Fatalf("OpenJSON: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("roundtrip mismatch: got %s", out)
	}
}

func TestAEADOpenJSONRejectsInvalidBase64(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	bad := json.RawMessage(`"not-valid-base64!!!"`)
	if _, err := AEADOpenJSON(&key, bad, nil); err == nil {
		t.Fatal("expected base64 decode failure")
	}
}

func TestAEADOpenJSONRejectsNonStringJSON(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	bad := json.RawMessage(`{"not":"a string"}`)
	if _, err := AEADOpenJSON(&key, bad, nil); err == nil {
		t.Fatal("expected JSON string unmarshal failure")
	}
}
