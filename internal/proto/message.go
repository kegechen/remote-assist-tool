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
	// Claude Code 工具通道
	MsgToolHello    MessageType = "tool_hello"
	MsgToolHelloAck MessageType = "tool_hello_ack"
	MsgToolReq      MessageType = "tool_req"
	MsgToolResp     MessageType = "tool_resp"
	MsgToolStream   MessageType = "tool_stream"
	MsgToolCancel   MessageType = "tool_cancel"
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
	Host     string `json:"host,omitempty"` // 身份串 "user@host 系统 架构"（sysinfo.Summary），看板展示用；旧客户端不发
	// SupportsUpdateNotice 表示能自行显示 UpdateInfo，relay 不再发送旧版兼容提示。
	SupportsUpdateNotice bool `json:"supports_update_notice,omitempty"`
	// RequestCode 期望复用的协助码：share 进程内缓存的上一次所得之码。relay 在码可用时原样复用，
	// 使码在 share 进程存活期间不变（relay 重启、share 离线超宽限后重连也能找回）。旧 relay 忽略。
	RequestCode string `json:"request_code,omitempty"`
	// ReuseToken 持有证明：上一次注册响应里 relay 下发的 RegisterResponse.ReuseToken。relay 只在令牌
	// 验证通过（绑定了该码、本 ClientID、原始签发时间）时才允许复用 RequestCode，否则一律发新码——
	// 防止他人凭泄露的协助码抢先注册占码。旧 share 不发、旧 relay 忽略。
	ReuseToken string `json:"reuse_token,omitempty"`
	// SupportsStickyCode 表示客户端理解 RegisterResponse.CodeSticky 且 expires_at==0 的"无到期"语义。
	SupportsStickyCode bool `json:"supports_sticky_code,omitempty"`
}

// RegisterResponse 注册响应
type RegisterResponse struct {
	Code string `json:"code"`
	// ExpiresAt 为 0 且 CodeSticky 为 true 表示码在 share 在线期间一直有效（无固定到期时刻）。
	ExpiresAt int64 `json:"expires_at"`
	// CodeSticky relay 声明码在 share 在线期间恒定有效、掉线宽限内/relay 状态在时可复用。旧 relay 不发。
	CodeSticky bool `json:"code_sticky,omitempty"`
	// ReuseToken relay 签发的复用令牌（只给 share，不是协助码，不得展示给协助端或写入协助信息），
	// share 仅缓存在进程内存里，重连时随 RegisterRequest.ReuseToken 回传以证明自己是码的原持有者。旧 relay 不发。
	ReuseToken string `json:"reuse_token,omitempty"`
	UpdateInfo
	// STUNServers relay 建议的 STUN 服务器（host:port，--advertise-stun）；新版客户端把它们
	// 加入公网地址发现的竞速候选。旧 relay 不发、旧客户端忽略。
	STUNServers []string `json:"stun_servers,omitempty"`
}

// UpdateInfo relay 下发的新版本软提示（旧 relay 不发、旧客户端忽略）。只用于提示，
// 客户端与自身版本比较后决定是否打印，从不因此拒绝连接。
type UpdateInfo struct {
	LatestVersion string `json:"latest_version,omitempty"`
	// DownloadURL 下载链接模板，支持 {os}/{arch}/{ext} 占位，由客户端按目标平台展开。
	DownloadURL string `json:"download_url,omitempty"`
	// ReleasePage 平台推断不出来时的兜底链接。
	ReleasePage string `json:"release_page,omitempty"`
}

// JoinRequest 协助端加入请求
type JoinRequest struct {
	Code                 string `json:"code"`
	Version              string `json:"version,omitempty"`
	Host                 string `json:"host,omitempty"` // 同 RegisterRequest.Host
	SupportsUpdateNotice bool   `json:"supports_update_notice,omitempty"`
	// SupportsCanonicalCode 表示本协助端理解 JoinResponse.Code（relay 实际匹配上的
	// 规范码）并会按它派生会话密钥。relay 仅对声明了本字段的 join 按大写不敏感匹配
	// 并回规范码；未声明（旧 help 用自己输入的码派生密钥，relay 不能代做）保持精确
	// 比对。旧 relay 忽略本字段。
	SupportsCanonicalCode bool `json:"supports_canonical_code,omitempty"`
}

// JoinResponse 加入响应
type JoinResponse struct {
	Success     bool   `json:"success"`
	SessionID   string `json:"session_id,omitempty"`
	Error       string `json:"error,omitempty"`
	PeerVersion string `json:"peer_version,omitempty"`
	PeerHost    string `json:"peer_host,omitempty"` // share 端身份串（同 RegisterRequest.Host；旧 relay 不发）
	UpdateInfo
	STUNServers []string `json:"stun_servers,omitempty"` // 同 RegisterResponse.STUNServers
	// Code 是 relay 实际匹配上的规范协助码，仅发给声明了 supports_canonical_code
	// 的 join：help 端手打小写时需按它派生会话密钥（share 端总是用 relay 下发的
	// 原码）。旧 relay 不发本字段，help 端保持用输入的码。
	Code string `json:"code,omitempty"`
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
	PeerHost    string `json:"peer_host,omitempty"` // help 端身份串（同 JoinRequest.Host；旧 relay 不发）
}

// ErrorMessage 错误消息
type ErrorMessage struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorMessage.Code 里对两端都有语义的几个值。收发两侧原先各写各的字符串字面量，
// 改一处漏一处就退化成「只打日志」的静默失联，因此收拢到协议包里。
const (
	// ErrCodePeerDisconnected 对端已断开：发给 help 时指 share 掉线，发给 share 时
	// 指 help 掉线且已过去抖窗口。两侧收到后都应视为当前会话结束。
	ErrCodePeerDisconnected = "PEER_DISCONNECTED"
	// ErrCodePeerReconnected share 换了新连接（热升级 / 重注册），旧 help 必须重新 Join。
	// relay 发完这条会立刻关掉旧 help 的连接。
	ErrCodePeerReconnected = "PEER_RECONNECTED"
	// ErrCodeClientIDConflict 同一持久 ClientID 的 share 已在别的进程在线，且两边在互相
	// 顶替（典型是旧版 share 没退出又起了新版）。relay 拒绝后来者、保留在线的那个；
	// 收到的 share 应提示用户停掉另一个进程，并退避重试。
	ErrCodeClientIDConflict = "CLIENT_ID_CONFLICT"
)

// PeerAddrAdvertise 对等端地址通告（发送给服务器）
type PeerAddrAdvertise struct {
	PublicAddr  string `json:"public_addr"`        // "ip:port"
	PrivateAddr string `json:"private_addr"`       // "ip:port"
	NATType     string `json:"nat_type,omitempty"` // NAT 类型: "open", "cone", "symmetric"
	// STUNServer / STUNMs：实际采用的 STUN 服务器与发现耗时（毫秒），供 relay 统计各 STUN
	// 来源的 P2P 成功率。未拿到公网地址时为空 / 0。旧客户端不发、旧 relay 忽略。
	STUNServer string `json:"stun_server,omitempty"`
	STUNMs     int64  `json:"stun_ms,omitempty"`
}

// PeerAddrReady 对等端地址就绪（服务器转发给对方）
type PeerAddrReady struct {
	PeerPublicAddr  string `json:"peer_public_addr"`
	PeerPrivateAddr string `json:"peer_private_addr"`
	IsShare         bool   `json:"is_share"`                // true if this is for the share side
	SameNetwork     bool   `json:"same_network,omitempty"`  // 两端是否在同一网络
	PeerNATType     string `json:"peer_nat_type,omitempty"` // 对端 NAT 类型
}

// P2PConnected 打通 P2P 隧道后告诉 relay（MsgP2PConnected）。
type P2PConnected struct {
	SessionID string `json:"session_id"`
	// Path 隧道实际走的路，见 P2PPathDirect / P2PPathUDPRelay。旧客户端不发，按未知处理；
	// 旧 relay 忽略此字段。
	Path string `json:"path,omitempty"`
}

const (
	// P2PPathDirect 两端打洞直连，数据不经服务器。
	P2PPathDirect = "direct"
	// P2PPathUDPRelay 打洞失败后经 relay 的 UDP 中继（--stun 指向的 3478）转发：
	// 走的是同一套 UDP 隧道，但流量仍经服务器，不是 P2P 直连。
	P2PPathUDPRelay = "udp_relay"
)

// P2PTestPacket P2P 测试包
type P2PTestPacket struct {
	SessionID string `json:"session_id"`
	Random    string `json:"random"`
	// MAC 以协助码派生密钥对 SessionID/Random/发送方身份做的 HMAC，见 punch.go。
	// 空值只可能来自不认识这个字段的旧版本，一律按认证失败处理。
	MAC string `json:"mac,omitempty"`
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
