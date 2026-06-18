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

// NoAuthCode 是 --no-auth 模式使用的固定协助码常量。
// 使用固定 code 省去 code 交换步骤，但 AEAD 会话密钥仍由
// DeriveSessionKey(NoAuthCode, randomNonce, randomNonce) 派生。
// 安全语义：
//   - 固定 code 是公开常量，**不防主动连接** —— 任何能访问 relay 地址的设备
//     都能连上并 exec/读写本机，仅限完全可信的私有 LAN。
//   - “防被动窃听” 依赖 TLS 传输层保护握手 nonce：默认自签 TLS 下窃听者拿不到
//     nonce、无法派生密钥；但 --plain 模式 nonce 明文 + code 公开 → 密钥可被派生、
//     加密失效，故 no-auth 不应与 --plain 同用。
//   - 限单 share：共享 relay 下多个 no-auth share 会撞同一固定 code（后注册覆盖
//     前者），故 no-auth 仅适用单 share / standalone 场景。
//
// 必须是 normalizeCode 安全的值（不含 '-'/' '/'_'）：relay 按原值存入
// byCode，而 join 端先 normalizeCode 再查表、且两端各自用本值派生会话密钥。
// 一旦含连字符会同时引发两个 bug：join 端 normalize 后查不到 byCode（报
// invalid code）、share 用原值/help 用 normalize 值派生导致 AEAD 密钥不一致。
const NoAuthCode = "noauth"

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
