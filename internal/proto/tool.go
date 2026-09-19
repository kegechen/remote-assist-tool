package proto

import "encoding/json"

// ToolReq Claude Code 通过 help 端发起的工具调用请求
type ToolReq struct {
	ID         uint64          `json:"id"`
	Tool       string          `json:"tool"`
	ArgsJSON   json.RawMessage `json:"args"`        // 已 AEAD 解密后的工具参数 JSON
	DeadlineMs uint32          `json:"deadline_ms"` // 0 = 工具默认
}

// ToolResp share 端处理完的应答（或流的终止帧）
type ToolResp struct {
	ID         uint64          `json:"id"`
	OK         bool            `json:"ok"`
	ResultJSON json.RawMessage `json:"result,omitempty"`
	ErrorCode  string          `json:"error_code,omitempty"`
	ErrorMsg   string          `json:"error_msg,omitempty"`
}

// StreamChunk exec stream=true / tail_log follow / 大文件分块
type StreamChunk struct {
	ID     uint64 `json:"id"`
	Seq    uint32 `json:"seq"`
	Fin    bool   `json:"fin"`
	Stream string `json:"stream,omitempty"` // "stdout" | "stderr" | "" (binary)
	Data   []byte `json:"data,omitempty"`
}

// Cancel 取消指定 in-flight 请求
type Cancel struct {
	ID     uint64 `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// Hello / HelloAck 工具通道版本与能力协商
//
// Version 与 Versions 的分工（见 handshake.go NewHello 的注释）：Version 是给 0.0.x 看的
// 兼容锚点——那些版本做的是严格相等比对，只认得这一个字段；Versions 才是真正的协商依据。
// 旧版本会忽略它不认识的 Versions 字段，所以加这个字段本身不破坏任何东西。
type Hello struct {
	Version      string   `json:"version"`
	Versions     []string `json:"versions,omitempty"` // 本端支持的全部版本，降序；空表示对端是 0.0.x
	Capabilities []string `json:"capabilities"`
	NonceB64     string   `json:"nonce_b64"` // base64(16 random bytes)
}

// HelloAck 的 Version 承载**协商选定**的版本，而不是应答方支持的最高版本。
// 0.0.x 的 share 在这里填的是它自己的常量 "1"，语义恰好一致，不需要特判。
type HelloAck struct {
	Version      string   `json:"version"`
	Versions     []string `json:"versions,omitempty"` // 应答方支持的全部版本，供发起方核对
	Capabilities []string `json:"capabilities"`
	NonceB64     string   `json:"nonce_b64"`
	Accept       bool     `json:"accept"`
	ErrorMsg     string   `json:"error_msg,omitempty"`
}
