package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"
)

// 根私钥文件：scrypt 从口令派生密钥，XChaCha20-Poly1305 加密 32 字节 seed。
// 名字与公钥写进附加认证数据（AAD），防止把备用根的文件改名冒充主根，或篡改公钥字段。
const (
	keyFileVersion    = 1
	keyAADPrefix      = "remote-assist-root-key-v1\x00"
	defaultScryptLogN = 17 // N = 131072，r = 8：约 128 MiB 内存，桌面机上不到一秒
	minScryptLogN     = 15
	maxScryptLogN     = 20 // 打开文件时的上限，防止被篡改的文件要求离谱的内存
	scryptR           = 8
	scryptP           = 1
	minPassphraseLen  = 12
)

type keyFile struct {
	Version    int    `json:"version"`
	Name       string `json:"name"`
	PublicKey  []byte `json:"public_key"`
	KDF        string `json:"kdf"`
	LogN       uint8  `json:"log_n"`
	R          int    `json:"r"`
	P          int    `json:"p"`
	Salt       []byte `json:"salt"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

var errWrongPassphrase = errors.New("口令不对，或密钥文件已损坏")

func keyAAD(name string, pub []byte) []byte {
	aad := append([]byte(keyAADPrefix), name...)
	aad = append(aad, 0)
	return append(aad, pub...)
}

func deriveKey(passphrase string, salt []byte, logN uint8) ([]byte, error) {
	return scrypt.Key([]byte(passphrase), salt, 1<<logN, scryptR, scryptP, chacha20poly1305.KeySize)
}

// sealKey 用口令加密根私钥。
func sealKey(name string, priv ed25519.PrivateKey, passphrase string, logN uint8) ([]byte, error) {
	if utf8.RuneCountInString(passphrase) < minPassphraseLen {
		return nil, fmt.Errorf("口令至少 %d 个字符", minPassphraseLen)
	}
	salt := make([]byte, 16)
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	k, err := deriveKey(passphrase, salt, logN)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	kf := keyFile{
		Version: keyFileVersion, Name: name, PublicKey: pub,
		KDF: "scrypt", LogN: logN, R: scryptR, P: scryptP, Salt: salt, Nonce: nonce,
		Ciphertext: aead.Seal(nil, nonce, priv.Seed(), keyAAD(name, pub)),
	}
	return json.MarshalIndent(kf, "", "  ")
}

// openKey 解密根私钥，并核对文件确实是 name 这把根。
func openKey(data []byte, name, passphrase string) (ed25519.PrivateKey, error) {
	var kf keyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, fmt.Errorf("密钥文件格式不对: %w", err)
	}
	if kf.Version != keyFileVersion || kf.KDF != "scrypt" || kf.R != scryptR || kf.P != scryptP ||
		kf.LogN < minScryptLogN || kf.LogN > maxScryptLogN || len(kf.Salt) < 16 ||
		len(kf.Nonce) != chacha20poly1305.NonceSizeX || len(kf.PublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("密钥文件参数不对，拒绝使用")
	}
	if kf.Name != name {
		return nil, fmt.Errorf("这是 %q 根密钥的文件，不是 %q", kf.Name, name)
	}
	k, err := deriveKey(passphrase, kf.Salt, kf.LogN)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	seed, err := aead.Open(nil, kf.Nonce, kf.Ciphertext, keyAAD(kf.Name, kf.PublicKey))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errWrongPassphrase
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if !priv.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(kf.PublicKey)) {
		return nil, errors.New("密钥文件里的公钥与私钥不符")
	}
	return priv, nil
}
