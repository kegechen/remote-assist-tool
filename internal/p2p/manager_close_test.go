package p2p

import (
	"errors"
	"net"
	"testing"
	"time"
)

// newTestUDPConn 起一个绑在环回口上的 UDP socket，充当 P2PManager 的 localConn。
func newTestUDPConn(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// udpConnClosed 判断 socket 是否已被关闭。对已关闭的 *net.UDPConn 调用 SetDeadline
// 会返回 net.ErrClosed，对存活的则返回 nil。
func udpConnClosed(conn *net.UDPConn) bool {
	return errors.Is(conn.SetDeadline(time.Now().Add(time.Second)), net.ErrClosed)
}

// backgroundRetry 打洞成功时 connected 会置位，但按设计刻意不建隧道（onP2PConnected 的
// backgroundMode 分支直接 return）。于是没有任何隧道接管 localConn，这个 UDP socket 是无主的，
// 必须由 Close 回收。
//
// 回归点：旧的 Close 用 `if !connected` 判断是否回收 localConn，恰好漏掉这一条路径——
// connected 是 true，隧道却不存在，socket 一直悬到进程退出。
func TestCloseReclaimsLocalConnAfterBackgroundRetrySucceeded(t *testing.T) {
	mgr := NewP2PManager(P2PModeAuto, "", "")
	conn := newTestUDPConn(t)
	mgr.localConn = conn
	mgr.resultCh = make(chan P2PResult, 2)

	mgr.backgroundMode.Store(true)
	mgr.connectedMu.Lock()
	mgr.connected = true // 后台重试打洞成功，但没有隧道
	mgr.connectedMu.Unlock()

	mgr.Close()

	if !udpConnClosed(conn) {
		t.Fatal("Close 未回收 localConn：后台重试成功时 connected 已置位但没有隧道接管这个 socket")
	}
}

// 从未连上时（既没 connected 也没隧道）同样必须回收。
func TestCloseReclaimsLocalConnWhenNeverConnected(t *testing.T) {
	mgr := NewP2PManager(P2PModeAuto, "", "")
	conn := newTestUDPConn(t)
	mgr.localConn = conn
	mgr.resultCh = make(chan P2PResult, 2)

	mgr.Close()

	if !udpConnClosed(conn) {
		t.Fatal("Close 未回收从未使用过的 localConn")
	}
}

// 隧道已经投递进 resultCh、但上层走了 sessionDone 分支没来取时，Close 必须抽干 channel
// 并关掉这条孤儿隧道。
//
// 回归点：旧的 Close 只看 connected，不碰 resultCh。隧道连同 UDP socket 和它的 3 个
// goroutine 一起悬空，自愈只能等 60s peerTimeout——对端仍在发 keepalive 时连这个都不生效。
func TestCloseDrainsAndClosesOrphanTunnel(t *testing.T) {
	mgr := NewP2PManager(P2PModeAuto, "", "")
	conn := newTestUDPConn(t)
	mgr.localConn = conn
	mgr.resultCh = make(chan P2PResult, 2)

	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 65000}
	tunnel := NewUDPTunnel(conn, peer)
	mgr.resultCh <- P2PResult{Tunnel: tunnel}
	mgr.tunnelMu.Lock()
	mgr.tunnelCreated = true // 投递成功，按旧逻辑 localConn 归消费者管
	mgr.tunnelMu.Unlock()

	mgr.Close()

	if !udpConnClosed(conn) {
		t.Fatal("Close 未抽干 resultCh 里没人接手的隧道，UDP socket 与隧道的 goroutine 一起悬空")
	}
}

// 隧道已被消费者取走时，Close 不能再去关 localConn——那是正在服务的连接。
func TestCloseKeepsLocalConnWhenTunnelWasTakenOver(t *testing.T) {
	mgr := NewP2PManager(P2PModeAuto, "", "")
	conn := newTestUDPConn(t)
	mgr.localConn = conn
	mgr.resultCh = make(chan P2PResult, 2)

	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 65001}
	tunnel := NewUDPTunnel(conn, peer)
	mgr.resultCh <- P2PResult{Tunnel: tunnel}
	<-mgr.resultCh // 消费者取走，从此由它负责 tunnel.Close()
	mgr.tunnelMu.Lock()
	mgr.tunnelCreated = true
	mgr.tunnelMu.Unlock()

	mgr.Close()

	if udpConnClosed(conn) {
		t.Fatal("Close 关掉了已交接给消费者的隧道所用的 localConn")
	}
	tunnel.Close()
}

// resultCh 已满（消费者早已走了 relay 回退，不会再读第二条结果）时，onP2PConnected 的
// 非阻塞投递会失败。此时必须就地关掉刚建好的隧道，否则它连同 localConn 一起成为孤儿。
func TestOnP2PConnectedClosesTunnelWhenResultChannelIsFull(t *testing.T) {
	mgr := NewP2PManager(P2PModeAuto, "", "")
	conn := newTestUDPConn(t)
	mgr.localConn = conn
	mgr.resultCh = make(chan P2PResult, 2)
	mgr.resultCh <- P2PResult{} // 回退结果
	mgr.resultCh <- P2PResult{} // 填满
	mgr.sessionID = "test-session"

	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 65002}
	mgr.onP2PConnected(peer)

	if !udpConnClosed(conn) {
		t.Fatal("投递失败的隧道没有被关闭：localConn 与隧道的 goroutine 会一直悬到 60s peerTimeout")
	}
	if mgr.tunnelCreated {
		t.Error("投递失败却把 tunnelCreated 置了位，Close 会据此放弃回收 localConn")
	}
}
