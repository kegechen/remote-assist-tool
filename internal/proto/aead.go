package proto

import (
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// AEADSeal 用 32 字节 key 对明文做 XChaCha20-Poly1305 加密。
// 返回的密文格式：[24B nonce][ct+tag]
func AEADSeal(key *[32]byte, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("new aead: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("rand nonce: %w", err)
	}
	out := append([]byte{}, nonce...)
	out = aead.Seal(out, nonce, plain, nil)
	return out, nil
}

// AEADOpen 解密 AEADSeal 输出的密文。
func AEADOpen(key *[32]byte, ct []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("new aead: %w", err)
	}
	if len(ct) < aead.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := ct[:aead.NonceSize()]
	body := ct[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("aead open: %w", err)
	}
	return plain, nil
}
