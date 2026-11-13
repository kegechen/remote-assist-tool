package gui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/remote-assist/tool/internal/client"
	"github.com/remote-assist/tool/internal/proto"
)

// 连接失败的归一化原因码，前端据此决定提示文案与是否弹出降级确认。
const (
	reasonCanceled   = "canceled"
	reasonPeerLegacy = "peer_legacy"
	reasonBadCode    = "bad_code"
	reasonTimeout    = "timeout"
	reasonRefused    = "refused"
	reasonRejected   = "rejected"
	reasonUnreach    = "unreachable"
	reasonOther      = "other"
)

// classifyConnectError 把 connect 的原始错误归一为（原因码, 人话）。
// 顺序有讲究：旧版识别必须最先（且只认固定前缀，见 client.IsLegacyPeerError），
// 其后「握手被对端拒绝」要排在 join 之前，免得对端回传的文本干扰分类。
func classifyConnectError(err error) (code, reason string) {
	if err == nil {
		return "", ""
	}
	if errors.Is(err, context.Canceled) {
		return reasonCanceled, "已取消"
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case client.IsLegacyPeerError(msg):
		return reasonPeerLegacy, "对端是旧版（0.0.x），协议版本不兼容"
	case strings.Contains(low, "handshake failed") && strings.Contains(msg, "对端拒绝"):
		return reasonRejected, "被对端拒绝（握手未通过）"
	case strings.Contains(low, "refused") || strings.Contains(msg, "拒绝"):
		return reasonRefused, "连接被拒绝（中继未启动或端口不对）"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(low, "timeout"),
		strings.Contains(low, "timed out"), strings.Contains(low, "deadline exceeded"):
		return reasonTimeout, "连接超时"
	case strings.Contains(low, "no such host"), strings.Contains(low, "unreachable"),
		strings.Contains(low, "relay connect failed"), strings.Contains(low, "failed to connect"),
		strings.Contains(low, "dial"):
		return reasonUnreach, "网络不可达（请检查中继地址与网络）"
	case strings.Contains(low, "join failed"):
		return reasonBadCode, "协助码错误，或被协助端未在线"
	}
	return reasonOther, "连接失败"
}

// connectRequest /api/connect 的请求体。
//
// 历史上还有 no_downgrade（「禁止降级」开关）字段：该开关已从界面移除，自动降级策略恒为开启
// （loopback/私网中继自动降级，公网中继需用户确认）。这里不再声明该字段，旧调用方若仍带上它，
// 解码时被忽略（不报错、不改变策略）。
type connectRequest struct {
	Code   string `json:"code"`
	Server string `json:"server"`
	NoAuth bool   `json:"no_auth"`
	// MinProto 显式指定 connect 的 min_proto（旧接口，单次直连不自动重试）；取值由 help 校验。
	MinProto string `json:"min_proto"`
	// AllowLegacy 用户已确认以兼容模式（v1）连接。
	AllowLegacy bool `json:"allow_legacy"`
}

// fallbackPolicy 自动降级的策略输入。
type fallbackPolicy struct {
	// MinProto 非空表示调用方显式指定了下限（旧接口/强制兼容）：只连一次，不做任何自动重试。
	MinProto string
	// AllowLegacy 用户已在界面确认「以兼容模式继续」：直接按 v1 下限连一次。
	AllowLegacy bool
	// LAN 中继是 loopback/私网/链路本地。公网中继上的降级必须由用户确认。
	LAN bool
}

// connectResult 一次（含降级重试的）连接的结果。
type connectResult struct {
	Raw        json.RawMessage
	MinProto   string // 实际成功/最后一次尝试使用的下限，自动重连沿用它
	FellBack   bool   // 经自动降级才连上
	NeedsAsk   bool   // 对端是旧版，但中继非私网：需用户确认后才降级
	Attempts   int
	LastReason string
}

// connectCaller 发起一次 connect 工具调用，minProto 为空表示走默认（v2）。
type connectCaller func(ctx context.Context, minProto string) (json.RawMessage, error)

// connectWithFallback 先按 v2 连；仅当失败原因明确是「对端为 0.0.x 旧版」时，
// 在安全策略允许下以 min_proto=1 重试**至多一次**。
//
// 安全策略（防诱导降级）：
//   - 只认握手结果里的固定标记，网络错误/超时/协助码错误一律不降级；
//   - 公网（非 loopback/私网）中继上，不自动降级，交给用户确认（NeedsAsk）：中继是
//     不可信第三方，可伪造「旧版」应答把双方打回无认证的 v1；
//   - 上下文已取消（用户取消）绝不重试。
func connectWithFallback(ctx context.Context, call connectCaller, p fallbackPolicy, onRetry func()) (connectResult, error) {
	direct := p.MinProto
	if direct == "" && p.AllowLegacy {
		direct = proto.ToolProtocolVersionV1
	}
	if direct != "" {
		raw, err := call(ctx, direct)
		return connectResult{Raw: raw, MinProto: direct, Attempts: 1}, err
	}

	raw, err := call(ctx, "")
	res := connectResult{Raw: raw, Attempts: 1}
	if err == nil || ctx.Err() != nil || !client.IsLegacyPeerError(err.Error()) {
		return res, err
	}
	if !p.LAN {
		res.NeedsAsk = true
		return res, err
	}
	if onRetry != nil {
		onRetry()
	}
	raw, err = call(ctx, proto.ToolProtocolVersionV1)
	res = connectResult{Raw: raw, MinProto: proto.ToolProtocolVersionV1, FellBack: err == nil, Attempts: 2}
	return res, err
}

// isLANRelay 判断本次连接所用的中继是否在私网/本机。地址未知（走内置默认公网中继）按公网处理。
func isLANRelay(server, defaultServer string) bool {
	addr := strings.TrimSpace(server)
	if addr == "" {
		addr = strings.TrimSpace(defaultServer)
	}
	if addr == "" {
		return false
	}
	return client.IsLANServer(client.NormalizeServerAddr(addr))
}
