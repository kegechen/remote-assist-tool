package client

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/proto"
)

// helpLink 一次 connect 建立起来的会话：relay 控制连接、工具 bridge，以及后台升级出的
// P2P 工具隧道。relay 读循环、P2P 读循环和续会话 goroutine 共用它。
//
// P2P 直连仍通时，relay 那边出事不拆会话，工具调用继续走 P2P：
//   - 本端的 relay 连接断了（relay 重启、网络中断，或被协助端换了连接、relay 要求重新加入）：
//     后台重新加入 relay 并附上续会话证明；被协助端校验通过后经 P2P 回一条确认，会话就此续上
//     （见 proto.P2PResumeProof）。续不上（对端没确认、P2P 也断了、重试窗口用完）才拆。
//   - 只是被协助端到 relay 的连接断了：本端连接留着，等它重连回来；它会请 relay 保留这一配对
//     （见 proto.RegisterRequest.KeepHelp）。
type helpLink struct {
	b      *HelpMCPBootstrap
	h      *HelpMode
	bridge *mcp.Bridge
	// sess relay 上首次握手得到的工具会话。P2P 沿用它，续会话证明也用它算：P2P 隧道
	// 一旦断开回落中转，就会重新握手换钥，那时已经没有可续的 P2P 了。
	sess      proto.Session
	addrRelay *peerAddrRelay

	// p2pConn 由后台升级 goroutine 填入，teardown 也要能关它。
	p2pConn atomic.Pointer[P2PConn]
	// mgr 供 teardown 回收 P2P manager（它持有绑定的 UDP socket 与接收循环）。不回收的话，
	// 每次 connect/重连都会漏一个——transferWithReconnect 一次传输就可能重连 5 次。
	mgrMu sync.Mutex
	mgr   shareP2PManager

	// helloAckCh 把 relay 上的 ToolHelloAck 交给等待方。P2P 断开后要重新握手才能让
	// share 把 daemon 出口切回 relay，而此时 relay 已被读循环独占，只能这样转交。
	helloAckCh chan proto.HelloAck
	// pongCh / resumeAckCh 由 P2P 读循环转交探活回应与续会话确认。
	pongCh      chan struct{}
	resumeAckCh chan proto.P2PResumeAck
	// resuming relay 已断、正在凭 P2P 重新加入 relay。期间没有 relay 读循环。
	resuming atomic.Bool

	// sessionDone 由 teardown 关闭，唤醒还阻塞在 P2P 协商结果上的升级 goroutine 与续会话
	// goroutine。只关 manager 是不够的：P2PManager.Close 仅 close(stopChan)，其
	// p2pTimeout 收到后直接 return 而不往 resultCh 推结果，干等 resultCh 就是永久泄漏。
	sessionDone  chan struct{}
	teardownOnce sync.Once
}

func newHelpLink(b *HelpMCPBootstrap, h *HelpMode, bridge *mcp.Bridge, sess proto.Session, addrRelay *peerAddrRelay) *helpLink {
	return &helpLink{
		b:           b,
		h:           h,
		bridge:      bridge,
		sess:        sess,
		addrRelay:   addrRelay,
		helloAckCh:  make(chan proto.HelloAck, 1),
		pongCh:      make(chan struct{}, 1),
		resumeAckCh: make(chan proto.P2PResumeAck, 4),
		sessionDone: make(chan struct{}),
	}
}

func (l *helpLink) setManager(mgr shareP2PManager) {
	l.mgrMu.Lock()
	l.mgr = mgr
	l.mgrMu.Unlock()
}

func (l *helpLink) manager() shareP2PManager {
	l.mgrMu.Lock()
	defer l.mgrMu.Unlock()
	return l.mgr
}

// teardown 拆除当前会话，sync.Once 保证只跑一次：关闭 relay 控制连接
// （→ 心跳循环经 IsClosed() 退出，并让 relay 端读循环 EOF → DisconnectClient
// 立即清 Help 槽）、关 P2P 隧道（对端随即收到关闭通知）、唤醒在途工具调用、清 bootstrap
// 状态。任一读循环（P2P / relay）或续会话失败都调它。修复点：旧实现 P2P 隧道断开后只
// bridge.Disconnect + 清 b.help，不关 h.client，心跳不停 → relay 端 Help 槽永驻 → 新
// connect 永久 already-has-helper。
func (l *helpLink) teardown(err error) {
	l.teardownOnce.Do(func() {
		close(l.sessionDone)
		l.bridge.Disconnect(err)
		l.h.client.Close()
		if pc := l.p2pConn.Load(); pc != nil {
			pc.Close()
		}
		if mgr := l.manager(); mgr != nil {
			mgr.Close()
		}
		b := l.b
		b.mu.Lock()
		if b.bridge == l.bridge {
			if err != nil {
				b.lastDrop = err.Error()
			}
			b.help = nil
			b.bridge = nil
			b.link = nil
			b.activeTarget = connectTarget{}
			b.activeResult = connectResult{}
		}
		b.mu.Unlock()
	})
}

// closed 报告会话是否已被拆除。
func (l *helpLink) closed() bool {
	select {
	case <-l.sessionDone:
		return true
	default:
		return false
	}
}

// usable 报告会话还能不能接着用：relay 连着，或者 relay 断了但正凭 P2P 续会话。
// 幂等复用要过这一关，否则同参数的 connect 会拿回一个已经没有任何通道的会话。
func (l *helpLink) usable() bool {
	if l.closed() {
		return false
	}
	if l.resuming.Load() {
		return l.p2pConn.Load().resumable()
	}
	return !l.h.client.IsClosed()
}

// activeUsableLocked 幂等复用前检查活动会话是否可用。调用方须持 b.mu。
// 没有 link 的会话（只有测试会这样构造）退回只看 relay 连接。
func (b *HelpMCPBootstrap) activeUsableLocked() bool {
	if b.link != nil {
		return b.link.usable()
	}
	return b.help != nil && !b.help.client.IsClosed()
}

// relayLoop relay 读循环：单点消费 relay 连接，兼做 P2P 信令的转发。协商不再由主线程
// 抢读 relay，两者争抢同一条连接的问题（以及由此需要的 ResetDecoder）就此消失。
// 续会话成功后会为新连接再起一个。
func (l *helpLink) relayLoop() {
	h := l.h
	for {
		// relay 对每个心跳都回一个，太久一个字节都没收到就是连接不通了（见 readIdle）。单独一次分不出是断网还是发回的
		// 数据被丢掉，不改传输：之后的 connect、续会话加入时还收不到数据，或者短时间内又这样静默一次，才记下这个地址的
		// TCP 收不到数据、先拨 QUIC（见 Client.silentErr）。
		msg, err := h.client.readIdle(relaySilenceTimeout)
		if err != nil {
			l.relayLost(fmt.Errorf("tunnel_lost: 隧道已断开（%w），请重新 connect", err))
			return
		}
		switch msg.Type {
		case proto.MsgPeerAddrReady:
			var ready proto.PeerAddrReady
			if err := proto.DecodePayload(msg, &ready); err == nil {
				// 经 addrRelay 投递：manager 可能还没起来（它在后台 goroutine 里
				// 启动，含同步 STUN），暂存后由 attach 补投，绝不丢。
				l.addrRelay.deliver(&ready)
			}
		case proto.MsgToolHelloAck:
			// 交给 downgradeToRelay；无人等待时丢弃（初次握手在读循环启动前已完成）。
			var ack proto.HelloAck
			if err := proto.DecodePayload(msg, &ack); err == nil {
				select {
				case l.helloAckCh <- ack:
				default:
				}
			}
		case proto.MsgError:
			// 对端消失类错误不能只打日志：此后这条 relay 上不会再有工具响应，而
			// b.activeTarget 还留着，下一次同参数 connect 会命中幂等分支拿到指向死会话的
			// connected=true，永远自愈不了（见 peerGoneError）。P2P 还通时先确认对端是否
			// 真的不在了。
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			if gone := peerGoneError(msg); gone != nil {
				if errMsg.Code == proto.ErrCodePeerDisconnected {
					// 断的是被协助端到 relay 的连接，本端这条还好好的：P2P 直连仍通就接着用，
					// 连接也留着。它重连时会请 relay 保留这一配对（见
					// proto.RegisterRequest.KeepHelp）；relay 不支持的话会发 PEER_RECONNECTED，
					// 那时再重新加入。
					if l.peerStillOnP2P() {
						fmt.Fprintln(stderrLog(), "MCP: 被协助端与中转的连接已断开，P2P 直连仍通，继续使用")
						continue
					}
					l.teardown(gone)
					return
				}
				// PEER_RECONNECTED：relay 发完就关掉本端的连接，得重新加入（见 relayLost）。
				l.relayLost(gone)
				return
			}
			log.Printf("MCP relay error: %s", errMsg.Message)
		default:
			// 始终投递，即使工具流量已经升级到 P2P。
			//
			// 不能按「P2P 活跃就只 drain」来做：P2P 断开时 share 会先把 daemon
			// 出口切回 relay 并可能立刻发出响应，而 help 这边尚未复位标志，那条
			// 响应就被当垃圾丢掉、调用方只能干等 deadline。
			// 重复投递本身是安全的——HandleInbound 的 pending channel 是
			// buffered 1 且用非阻塞 select 投递，重复的那份自然被丢弃；何况
			// share 端同一时刻只往一条通道发，实际也不会重复。
			dispatchHelpToolMessage(msg, l.bridge)
		}
	}
}

// peerStillOnP2P relay 通知被协助端到 relay 的连接断了，判断它是不是只断了 relay 这一侧：
// P2P 直连还在，而且探活有回应。不能只看隧道状态——对端进程退出时不会发关闭通知，隧道
// 要等 60 秒才判死。
func (l *helpLink) peerStillOnP2P() bool {
	pc := l.p2pConn.Load()
	return pc.resumable() && l.b.sessionAlive(l.bridge) && l.probeP2P(pc, resumeProbeTimeout)
}

// relayLost 本端的 relay 连接断了：读错（relay 重启、网络中断），或 relay 通知被协助端
// 换了连接（随后就会关掉本端的连接）。
//
// 工具流量已在 P2P 直连上时，先转入续会话（见 resumeRelay）；否则照旧拆会话——relay
// 信令通道断开后不拆，relay 端 Help 槽会泄漏，新 connect 撞 already-has-helper。
func (l *helpLink) relayLost(cause error) {
	pc := l.p2pConn.Load()
	if !pc.resumable() || !l.b.sessionAlive(l.bridge) || !l.resuming.CompareAndSwap(false, true) {
		l.teardown(cause)
		return
	}
	go l.resumeRelay(pc, cause)
}

// 续会话的时间参数。是变量只为单测能调小。
var (
	// resumeWindow 续会话的总时限。期间工具调用照常走 P2P；到点还没续上就拆会话。
	resumeWindow = 3 * time.Minute
	// resumeFirstDelay / resumeMaxDelay 重试间隔从前者起翻倍、封顶于后者。relay 连不上的
	// 重试不进 relay，不计分。
	resumeFirstDelay = 500 * time.Millisecond
	resumeMaxDelay   = 30 * time.Second
	// resumeMaxRejectedJoins relay 拒绝加入（码不存在、被协助端不在线等）的次数上限，到了就
	// 放弃续会话。relay 把这类失败按来源 IP 计分（10 分钟满 20 分封禁），同一出口后面可能有
	// 好几个协助端，所以每次续会话最多只贡献这么几分。
	resumeMaxRejectedJoins = 4
	// resumeRejectedMinDelay 被拒后下次重试至少隔这么久。relay 重启后被协助端要按自己的退避
	// （封顶 30 秒）重新注册，赶在它前面重试只会白白计分；4 次被拒跨度 5+10+20 秒，够它回来。
	resumeRejectedMinDelay = 5 * time.Second
	// resumeAckTimeout join 成功后等 share 经 P2P 回确认的上限。share 收到 SessionReady
	// 就会立刻校验并回复，P2P 上是毫秒级；等不到说明对端没接受（旧版本、换了进程或证明
	// 不对）。
	resumeAckTimeout = 8 * time.Second
	// resumeProbeTimeout relay 断开后经 P2P 探活的上限，同 P2P 升级时的双向探活。
	resumeProbeTimeout = p2pProbeTimeout
)

// errResumeNotAcked join 成功但对端没有确认续会话：对端已经按新协助端处理（会拆掉旧
// 隧道、等新的工具握手），这个会话续不上了，不必再试。
var errResumeNotAcked = errors.New("被协助端未确认续会话")

// errSessionClosed 续会话途中会话已被拆除。
var errSessionClosed = errors.New("会话已关闭")

// resumeRelay relay 断开、P2P 直连仍在时，把会话续到一条新的 relay 连接上。
//
// 第一步先经 P2P 探活：relay 通知被协助端掉线，也可能是它的进程已经退出（Ctrl+C、关窗
// 口都不会发 P2P 关闭通知）。这时隧道要等 60 秒才判死，若不探活就会让在途调用白等这么久，
// 还平白多出几次注定失败的 join。
func (l *helpLink) resumeRelay(pc *P2PConn, cause error) {
	l.h.client.Close()
	if !l.probeP2P(pc, resumeProbeTimeout) {
		l.teardown(cause)
		return
	}
	fmt.Fprintf(stderrLog(), "MCP: 中转连接中断（%v），P2P 直连仍通，正在重新接入中转...\n", cause)

	deadline := time.Now().Add(resumeWindow)
	delay := resumeFirstDelay
	rejected := 0
	for {
		select {
		case <-l.sessionDone:
			return
		case <-time.After(delay):
		}
		if !pc.resumable() {
			l.teardown(fmt.Errorf("tunnel_lost: 中转断开期间 P2P 隧道也断了，请重新 connect"))
			return
		}
		err := l.rejoinRelay(pc)
		if err == nil {
			return
		}
		switch {
		case errors.Is(err, errSessionClosed):
			return
		case errors.Is(err, proto.ErrBlocked):
			l.teardown(blockedConnectError())
			return
		case errors.Is(err, errResumeNotAcked):
			l.teardown(fmt.Errorf("tunnel_lost: 重新接入中转后%v，请重新 connect", err))
			return
		}
		delay *= 2
		if delay > resumeMaxDelay {
			delay = resumeMaxDelay
		}
		if isJoinRejected(err) {
			rejected++
			if rejected >= resumeMaxRejectedJoins {
				l.teardown(fmt.Errorf("tunnel_lost: 中转连续 %d 次拒绝重新加入（被协助端可能已不在中转上），请重新 connect", rejected))
				return
			}
			if delay < resumeRejectedMinDelay {
				delay = resumeRejectedMinDelay
			}
		}
		if time.Now().Add(delay).After(deadline) {
			l.teardown(fmt.Errorf("tunnel_lost: 中转断开后 %v 内未能重新接入（%v），请重新 connect", resumeWindow, err))
			return
		}
		log.Printf("MCP: rejoin relay failed (%v), retrying in %v", err, delay)
	}
}

// rejoinRelay 一次重新加入：连 relay、带证明 join、等 share 经 P2P 确认。成功后接回心跳与
// relay 读循环，结束续会话状态。
func (l *helpLink) rejoinRelay(pc *P2PConn) (err error) {
	h := l.h
	if err := h.client.Connect(); err != nil {
		h.client.Close()
		return err
	}
	defer func() {
		if err != nil {
			h.client.Close()
		}
	}()
	// teardown 若在 Connect 之前跑过，它关不到这条新连接，这里自己收。
	if l.closed() {
		return errSessionClosed
	}
	proof, err := proto.NewP2PResumeProof(l.sess, h.code)
	if err != nil {
		return err
	}
	// 排空上一次尝试残留的确认，免得被当成这一次的。
	for len(l.resumeAckCh) > 0 {
		<-l.resumeAckCh
	}
	resp, err := h.joinWithProof(proof)
	if err != nil {
		return err
	}
	if err := l.awaitResumeAck(proof); err != nil {
		return err
	}
	if l.closed() {
		return errSessionClosed
	}
	h.client.StartHeartbeatLoop(30 * time.Second)
	b := l.b
	b.mu.Lock()
	if b.bridge == l.bridge {
		// relay 重启后会话 ID 会变，connect 结果要跟着更新。
		b.activeResult.SessionID = resp.SessionID
	}
	b.mu.Unlock()
	l.resuming.Store(false)
	go l.relayLoop()
	fmt.Fprintln(stderrLog(), "MCP: 已重新接入中转（被协助端已确认续会话），继续使用 "+pc.tunnel.Describe())
	return nil
}

// awaitResumeAck 等 share 经 P2P 回来的续会话确认。校验不过的确认（陈旧的、伪造的）丢掉
// 继续等。
func (l *helpLink) awaitResumeAck(proof *proto.P2PResumeProof) error {
	timer := time.NewTimer(resumeAckTimeout)
	defer timer.Stop()
	for {
		select {
		case ack := <-l.resumeAckCh:
			if proto.VerifyP2PResumeAck(&ack, proof, l.sess, l.h.code) {
				return nil
			}
			log.Printf("MCP: ignoring invalid P2P resume ack")
		case <-timer.C:
			return errResumeNotAcked
		case <-l.sessionDone:
			return errSessionClosed
		}
	}
}

// probeP2P 经隧道发一次探活并等对端回应。P2P 读循环在跑，回应由它经 pongCh 转交。
// share 对隧道上的每个心跳都回一个（见 ShareMode.handleToolOverP2P），旧版本也是如此。
func (l *helpLink) probeP2P(pc *P2PConn, d time.Duration) bool {
	select {
	case <-l.pongCh:
	default:
	}
	if err := pc.SendMessage(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()}); err != nil {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-l.pongCh:
		return true
	case <-timer.C:
		return false
	case <-l.sessionDone:
		return false
	}
}

// consumeP2PControl 处理 P2P 隧道上的会话控制消息（探活回应、续会话确认），返回是否已消费。
func (l *helpLink) consumeP2PControl(msg *proto.Message) bool {
	switch msg.Type {
	case proto.MsgHeartbeat:
		select {
		case l.pongCh <- struct{}{}:
		default:
		}
		return true
	case proto.MsgP2PResumeAck:
		var ack proto.P2PResumeAck
		if proto.DecodePayload(msg, &ack) == nil {
			select {
			case l.resumeAckCh <- ack:
			default:
			}
		}
		return true
	}
	return false
}
