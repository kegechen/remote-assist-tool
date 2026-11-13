package gui

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 断线原因码。随 reconnecting / reconnect-fail / lost 事件的 JSON data 发给前端，
// 前端据此显示不同的中文文案与建议动作。
const (
	dropPeerGone      = "peer_gone"      // 被控端主动退出/断开（relay 推 PEER_DISCONNECTED）
	dropPeerRestarted = "peer_restarted" // 被控端重启或热升级（relay 推 PEER_RECONNECTED）
	dropTunnelLost    = "tunnel_lost"    // 中继/隧道断开（读循环出错、心跳超时）
	dropLocalExit     = "local_exit"     // 本地 MCP 子进程自行退出
	dropGeneric       = "disconnected"   // 只知道会话没了，原因未知
)

// dropInfo 一次断线的原因：Code 稳定可判定，Text 是一句话，Hint 是建议动作。
type dropInfo struct {
	Code string `json:"reason"`
	Text string `json:"text"`
	Hint string `json:"hint"`
}

func dropByCode(code string) dropInfo {
	switch code {
	case dropPeerGone:
		return dropInfo{code, "被控端已断开", "被控端已退出，请确认对方程序仍在运行"}
	case dropPeerRestarted:
		return dropInfo{code, "被控端已重启", "被控端已重启或热升级，正在重新连接"}
	case dropTunnelLost:
		return dropInfo{code, "中继或隧道已断开", "中继不可达或隧道中断，请检查网络与中继服务"}
	case dropLocalExit:
		return dropInfo{code, "本地连接组件已退出", "本地连接组件意外退出，正在重新连接"}
	case reasonUnreach:
		return dropInfo{code, "网络不可达", "无法连到中继，请检查网络、VPN 与中继地址"}
	case reasonRefused:
		return dropInfo{code, "中继拒绝连接", "中继未启动或端口不对，请检查中继服务与地址"}
	case reasonTimeout:
		return dropInfo{code, "连接超时", "网络较差或中继无响应，请检查网络"}
	case reasonBadCode:
		return dropInfo{code, "被控端未在线", "协助码已失效或被控端已退出，请确认对方程序仍在运行"}
	case reasonRejected:
		return dropInfo{code, "被控端拒绝连接", "对方拒绝了本次握手，请让对方确认授权后重新连接"}
	case reasonPeerLegacy:
		return dropInfo{code, "被控端版本不兼容", "对端为旧版（0.0.x），请升级被控端"}
	}
	return dropInfo{dropGeneric, "连接已断开", "与被控端的连接已断开，正在尝试重新连接"}
}

// classifyDropError 依据 help 侧会话断开错误（peer_gone / tunnel_lost / not_connected
// 及其携带的 last_drop 原因）判定断线原因。只有 not_connected 且不带原因时返回通用原因。
func classifyDropError(err error) dropInfo {
	if err == nil {
		return dropByCode(dropGeneric)
	}
	m := err.Error()
	switch {
	case strings.Contains(m, "peer_gone") && strings.Contains(m, "连接已更新"):
		return dropByCode(dropPeerRestarted)
	case strings.Contains(m, "peer_gone"):
		return dropByCode(dropPeerGone)
	case strings.Contains(m, "tunnel_lost"):
		return dropByCode(dropTunnelLost)
	}
	return dropByCode(dropGeneric)
}

// classifyReconnectError 把重连时的拨号/握手错误归为断线原因，复用连接失败的归一化。
// 无法归类（reasonOther/canceled）时返回 ok=false，调用方沿用先前的断线原因。
func classifyReconnectError(err error) (dropInfo, bool) {
	code, _ := classifyConnectError(err)
	switch code {
	case reasonUnreach, reasonRefused, reasonTimeout, reasonBadCode, reasonRejected, reasonPeerLegacy:
		return dropByCode(code), true
	}
	return dropInfo{}, false
}

// sseEventWithInfo 生成带 JSON data 的 SSE 事件文本（单行 data，normalizeSSEEvent 原样保留）。
func sseEventWithInfo(event string, info dropInfo, extra map[string]any) string {
	m := map[string]any{"reason": info.Code, "text": info.Text, "hint": info.Hint}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return fmt.Sprintf("event: %s\ndata: %s\n", event, b)
}
