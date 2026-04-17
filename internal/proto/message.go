package proto

import (
	"encoding/json"
)

// MessageType 消息类型
type MessageType string

const (
	MsgRegisterRequest  MessageType = "register_request"
	MsgRegisterResponse MessageType = "register_response"
	MsgJoinRequest      MessageType = "join_request"
	MsgJoinResponse     MessageType = "join_response"
	MsgTunnelData       MessageType = "tunnel_data"
	MsgHeartbeat        MessageType = "heartbeat"
	MsgError            MessageType = "error"
	MsgSessionReady     MessageType = "session_ready"
	// P2P related messages
	MsgPeerAddrAdvertise MessageType = "peer_addr_advertise"
	MsgPeerAddrReady     MessageType = "peer_addr_ready"
	MsgP2PTestPacket     MessageType = "p2p_test_packet"
	MsgP2PConnected      MessageType = "p2p_connected"
)

// Message 协议消息
type Message struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// RegisterRequest 被协助端注册请求
type RegisterRequest struct {
	ClientID string `json:"client_id,omitempty"`
	Version  string `json:"version,omitempty"`
}

// RegisterResponse 注册响应
type RegisterResponse struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
}

// JoinRequest 协助端加入请求
type JoinRequest struct {
	Code    string `json:"code"`
	Version string `json:"version,omitempty"`
}

// JoinResponse 加入响应
type JoinResponse struct {
	Success     bool   `json:"success"`
	SessionID   string `json:"session_id,omitempty"`
	Error       string `json:"error,omitempty"`
	PeerVersion string `json:"peer_version,omitempty"`
}

// TunnelData 隧道数据
type TunnelData struct {
	Data []byte `json:"data"`
}

// Heartbeat 心跳
type Heartbeat struct {
	Timestamp int64 `json:"timestamp"`
}

// SessionReady 会话就绪通知
type SessionReady struct {
	SessionID   string `json:"session_id"`
	PeerVersion string `json:"peer_version,omitempty"`
}

// ErrorMessage 错误消息
type ErrorMessage struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PeerAddrAdvertise 对等端地址通告（发送给服务器）
type PeerAddrAdvertise struct {
	PublicAddr  string `json:"public_addr"`            // "ip:port"
	PrivateAddr string `json:"private_addr"`           // "ip:port"
	NATType     string `json:"nat_type,omitempty"`     // NAT 类型: "open", "cone", "symmetric"
}

// PeerAddrReady 对等端地址就绪（服务器转发给对方）
type PeerAddrReady struct {
	PeerPublicAddr  string `json:"peer_public_addr"`
	PeerPrivateAddr string `json:"peer_private_addr"`
	IsShare         bool   `json:"is_share"`                  // true if this is for the share side
	SameNetwork     bool   `json:"same_network,omitempty"`    // 两端是否在同一网络
	PeerNATType     string `json:"peer_nat_type,omitempty"`   // 对端 NAT 类型
}

// P2PTestPacket P2P 测试包
type P2PTestPacket struct {
	SessionID string `json:"session_id"`
	Random    string `json:"random"`
}


// NewMessage 创建消息
func NewMessage(msgType MessageType, payload interface{}) (*Message, error) {
	m := &Message{Type: msgType}
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		m.Payload = data
	}
	return m, nil
}

// ParseMessage 解析消息
func ParseMessage(data []byte) (*Message, error) {
	var m Message
	err := json.Unmarshal(data, &m)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// DecodePayload 解码payload
func DecodePayload(m *Message, v interface{}) error {
	return json.Unmarshal(m.Payload, v)
}

// MustMarshal 序列化消息
func MustMarshal(msg interface{}) []byte {
	data, _ := json.Marshal(msg)
	return data
}
