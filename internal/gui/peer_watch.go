package gui

import (
	"context"
	"strings"
	"time"
)

// 被控端被杀、网络断开或中继退出后，help 的 MCP 子进程并不会退出：它收到 peer_gone 后
// 只拆掉会话，之后每次调用都返回 not_connected。GUI 后端的 guardLoop 以“子进程退出”作为
// 断线信号，所以此前页面状态一直停在“已连接”。这里加一个低频探测：连续几次探测都得到
// “对端已断”类错误，就主动结束该子进程，由 guardLoop 走既有的 reconnecting→重连→lost 流程。

const (
	peerWatchInterval  = 8 * time.Second // 探测间隔：低频、只在已连接时运行，空闲开销可忽略
	peerWatchThreshold = 2               // 连续次数：避免单次抖动误杀连接，最坏约 16 秒发现
	peerWatchTimeout   = 6 * time.Second
)

// isPeerDeadError 判断调用错误是否表示“对端/隧道已经没了”，而不是普通的工具失败
// （文件不存在、权限不足、超时等都不算）。
func isPeerDeadError(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	for _, k := range []string{"not_connected", "peer_gone", "tunnel_lost"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// peerWatch 统计连续“对端已断”次数。成功一次或遇到普通错误即清零。
type peerWatch struct {
	threshold int
	count     int
	reason    dropInfo // 连续失败期间见到的最具体的断线原因
}

func newPeerWatch(threshold int) *peerWatch { return &peerWatch{threshold: threshold} }

// observe 记录一次探测结果，返回是否应判定对端已断。
func (w *peerWatch) observe(err error) bool {
	if !isPeerDeadError(err) {
		w.count = 0
		w.reason = dropInfo{}
		return false
	}
	w.count++
	// 具体原因不被后到的通用 not_connected 覆盖
	if info := classifyDropError(err); w.reason.Code == "" || info.Code != dropGeneric {
		w.reason = info
	}
	return w.count >= w.threshold
}

// watchPeer 周期探测 client 是否还连着对端；判定已断则关闭 client 以触发 guardLoop 重连。
// 随 client 结束（Done）或 ctx 取消而退出；不连接时不运行。
func (s *Server) watchPeer(ctx context.Context, client *MCPClient) {
	w := newPeerWatch(peerWatchThreshold)
	t := time.NewTicker(peerWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-client.Done():
			return
		case <-t.C:
		}
		// 只探测当前仍在用的 client：已被替换（用户重连/改连）的旧 client 不再处理
		s.mu.Lock()
		current := s.client == client && s.connected
		s.mu.Unlock()
		if !current {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, peerWatchTimeout)
		_, err := client.CallTool(pctx, "stat", map[string]any{"path": "."})
		cancel()
		if w.observe(err) {
			s.setDropReason(client, w.reason)
			// Close 会让 Done() 触发，guardLoop 随即发 reconnecting 并按退避重连
			client.Close()
			return
		}
	}
}
