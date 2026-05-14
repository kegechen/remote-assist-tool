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
type Hello struct {
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	NonceB64     string   `json:"nonce_b64"` // base64(16 random bytes)
}

type HelloAck struct {
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	NonceB64     string   `json:"nonce_b64"`
	Accept       bool     `json:"accept"`
	ErrorMsg     string   `json:"error_msg,omitempty"`
}
