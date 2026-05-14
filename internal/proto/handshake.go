package proto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ToolProtocolVersion 工具通道协议版本（首版）
const ToolProtocolVersion = "1"

// DeriveSessionKey 以协助码 + 两端 nonce 派生 32 字节会话密钥。
// 不可逆，相同输入得到相同密钥；任一输入变化得到完全不同密钥。
func DeriveSessionKey(code, nonceShare, nonceHelp string) [32]byte {
	salt := []byte(nonceShare + "|" + nonceHelp)
	info := []byte("rat-tool-v" + ToolProtocolVersion)
	hk := hkdf.New(sha256.New, []byte(code), salt, info)
	var key [32]byte
	io.ReadFull(hk, key[:])
	return key
}

// NewHello 生成带随机 nonce 的 Hello
func NewHello() Hello {
	var n [16]byte
	rand.Read(n[:])
	return Hello{
		Version:      ToolProtocolVersion,
		Capabilities: []string{"exec", "read_file", "write_file", "list_dir", "stat", "glob", "grep", "process_list", "tail_log"},
		NonceB64:     base64.StdEncoding.EncodeToString(n[:]),
	}
}
