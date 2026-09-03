package proto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// AEADSeal 用 32 字节 key 对明文做 XChaCha20-Poly1305 加密。
// 返回的密文格式：[24B nonce][ct+tag]
//
// aad 是附加认证数据：不加密，但参与认证标签。工具通道用它把密文与外层的明文字段
// （工具名、调用 ID、帧序号）绑死，见 aad.go。传 nil 表示不绑定任何上下文。
func AEADSeal(key *[32]byte, plain, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("new aead: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("rand nonce: %w", err)
	}
	out := append([]byte{}, nonce...)
	out = aead.Seal(out, nonce, plain, aad)
	return out, nil
}

// AEADOpen 解密 AEADSeal 输出的密文。aad 必须与 Seal 时逐字节一致，否则解密失败。
func AEADOpen(key *[32]byte, ct, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("new aead: %w", err)
	}
	if len(ct) < aead.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := ct[:aead.NonceSize()]
	body := ct[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, fmt.Errorf("aead open: %w", err)
	}
	return plain, nil
}

// AEADSealJSON 加密 plaintext 并以 JSON 字符串字面量（"<base64>"）形式返回，
// 可作为 json.RawMessage 嵌入到外层结构的 RawMessage 字段而不破坏 json.Marshal。
// 原始 AEAD ciphertext 是任意二进制，不是合法 JSON，所以必须先 base64。
func AEADSealJSON(key *[32]byte, plain, aad []byte) (json.RawMessage, error) {
	ct, err := AEADSeal(key, plain, aad)
	if err != nil {
		return nil, err
	}
	wrapped, err := json.Marshal(base64.StdEncoding.EncodeToString(ct))
	if err != nil {
		return nil, fmt.Errorf("aead json marshal: %w", err)
	}
	return wrapped, nil
}

// AEADOpenJSON 反解 AEADSealJSON 的输出：解 JSON 字符串 → base64 → AEAD Open。
func AEADOpenJSON(key *[32]byte, raw json.RawMessage, aad []byte) ([]byte, error) {
	var b64 string
	if err := json.Unmarshal(raw, &b64); err != nil {
		return nil, fmt.Errorf("aead json unmarshal: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("aead base64 decode: %w", err)
	}
	return AEADOpen(key, ct, aad)
}
