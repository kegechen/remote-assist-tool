package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/agent/tools"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/sysinfo"
	"github.com/remote-assist/tool/internal/version"
)

// ErrPeerDisconnected 协助端断开连接（可恢复）
var ErrPeerDisconnected = errors.New("peer disconnected")

// shareP2PManager 是 share 会话编排依赖的最小 P2P manager 契约。
// 保持接口窄小，便于用可控 fake 覆盖 STUN 初始化阻塞与 helper 换代时序。
type shareP2PManager interface {
	SetRelayConn(p2p.RelayConn)
	Start(sessionID string, isShare bool) (<-chan p2p.P2PResult, error)
	HandlePeerAddrReady(*proto.PeerAddrReady)
	Close()
}

// ShareMode 被协助模式
type ShareMode struct {
	client         *Client
	sshAddr        string
	newInstance    bool
	clientID       string
	codeFile       string
	mirrorCodeFile string
	code           string
	expiresAt      time.Time
	sbCfg          agent.SandboxConfig
	daemon         *agent.Daemon
	daemonOnce     sync.Once

	// P2P 后台升级相关状态。relay 主循环与升级 goroutine 并发访问，统一由 p2pMu 保护。
	p2pMu         sync.Mutex
	p2pMgr        shareP2PManager      // 当前会话已完成 Start 的 manager
	p2pTunnel     *p2p.UDPTunnel       // 当前会话已建成的隧道，会话结束时据此关闭
	p2pPending    *proto.PeerAddrReady // manager 初始化期间提前到达的对端地址
	p2pEpoch      uint64               // 会话轮次，用于让上一轮的升级 goroutine 失效
	p2pDone       chan struct{}        // 本轮会话终止信号，唤醒阻塞在 resultCh 上的升级 goroutine
	daemonKey     [32]byte             // 最近一次工具握手派生的会话密钥，P2P 升级时沿用
	newP2PManager func(p2p.P2PMode, string, string) shareP2PManager
}

// beginP2PSession 开启新一轮会话的 P2P 状态，返回本轮 epoch 与终止信号。
//
// daemonKey 也要清：ShareMode 跨会话复用同一对象，上一轮的 key 留着会让
// toolModeReady() 在整个进程生命周期内恒为真，把之后的纯 SSH 会话也误判成工具模式。
func (s *ShareMode) beginP2PSession() (uint64, chan struct{}) {
	s.p2pMu.Lock()
	oldMgr, oldTunnel, oldDone := s.p2pMgr, s.p2pTunnel, s.p2pDone
	s.p2pEpoch++
	s.p2pMgr = nil
	s.p2pTunnel = nil
	s.p2pPending = nil
	s.daemonKey = [32]byte{}
	s.p2pDone = make(chan struct{})
	epoch, done := s.p2pEpoch, s.p2pDone
	s.p2pMu.Unlock()
	stopP2PSession(oldMgr, oldTunnel, oldDone)
	return epoch, done
}

// endP2PSession 会话结束时收拾 P2P 后台资源：递增 epoch 让仍在跑的升级 goroutine
// 认不回状态，并关掉 manager 与隧道。
//
// 必须有这一步：SSH 模式下升级 goroutine 会无超时地阻塞在 ReadModeHeader 上（要等
// 用户真正发起 SSH 连接），不主动关就会跨会话存活到 60s peerTimeout，期间还可能
// 拿旧隧道去动下一轮会话的 daemon。
func (s *ShareMode) endP2PSession() {
	s.p2pMu.Lock()
	s.p2pEpoch++
	mgr, tunnel, done := s.p2pMgr, s.p2pTunnel, s.p2pDone
	s.p2pMgr, s.p2pTunnel, s.p2pDone = nil, nil, nil
	s.p2pPending = nil
	s.p2pMu.Unlock()
	stopP2PSession(mgr, tunnel, done)
}

func stopP2PSession(mgr shareP2PManager, tunnel *p2p.UDPTunnel, done chan struct{}) {
	// 必须先放信号再关 manager：P2PManager.Close 只 close(stopChan)，其 p2pTimeout
	// 收到后直接 return 而**不往 resultCh 推结果**，光靠关 manager 唤不醒阻塞在
	// resultCh 上的升级 goroutine，每轮会话就会漏一个。
	if done != nil {
		close(done)
	}
	if tunnel != nil {
		tunnel.Close()
	}
	if mgr != nil {
		mgr.Close()
	}
}

// p2pEpochValid 报告调用方持有的 epoch 是否仍是当前轮次。升级 goroutine 在碰任何
// 共享状态（daemon / 隧道登记）之前都要过这一关。
func (s *ShareMode) p2pEpochValid(epoch uint64) bool {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	return s.p2pEpoch == epoch
}

// setP2PTunnel 登记本轮隧道；epoch 已失效时返回 false，调用方须立即关掉隧道退出。
func (s *ShareMode) setP2PTunnel(epoch uint64, tunnel *p2p.UDPTunnel) bool {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	if s.p2pEpoch != epoch {
		return false
	}
	s.p2pTunnel = tunnel
	return true
}

// attachP2PMgr 在同步 Start 完成后把 manager 挂到当前会话，并补投初始化期间暂存的
// PeerAddrReady。epoch 已失效说明 helper 已换代，调用方须关闭这个旧 manager。
func (s *ShareMode) attachP2PMgr(epoch uint64, mgr shareP2PManager) bool {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	if s.p2pEpoch != epoch {
		return false
	}
	s.p2pMgr = mgr
	if s.p2pPending != nil {
		mgr.HandlePeerAddrReady(s.p2pPending)
		s.p2pPending = nil
	}
	return true
}

// deliverPeerAddrReady 把 relay 信令投给当前 manager；Start 尚未完成时先暂存。
// manager 调用放在 p2pMu 内，避免 helper 换代并发 Close 后仍向旧 manager 投递。
func (s *ShareMode) deliverPeerAddrReady(ready *proto.PeerAddrReady) {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	if s.p2pMgr == nil {
		copyReady := *ready
		s.p2pPending = &copyReady
		return
	}
	s.p2pMgr.HandlePeerAddrReady(ready)
}

// clearP2PAttempt 仅清理调用方所属轮次的 manager/tunnel 引用。required 模式失败时，
// 校验 epoch 与关闭 relay 必须在同一临界区：否则新 SessionReady 可能在两步之间换代，
// 旧升级 goroutine 随后会误关新 helper 的健康连接。
func (s *ShareMode) clearP2PAttempt(epoch uint64, closeRelay bool) bool {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	if s.p2pEpoch != epoch {
		return false
	}
	s.p2pMgr = nil
	s.p2pTunnel = nil
	s.p2pPending = nil
	if closeRelay {
		s.client.Close()
	}
	return true
}

func (s *ShareMode) makeP2PManager(mode p2p.P2PMode) shareP2PManager {
	if s.newP2PManager != nil {
		return s.newP2PManager(mode, s.client.config.STUNServer, s.client.config.BindIP)
	}
	return p2p.NewP2PManager(mode, s.client.config.STUNServer, s.client.config.BindIP)
}

// toolModeReady 报告工具握手是否已完成。help 端保证「先在 relay 上完成工具握手，
// 才开始 P2P 协商」，所以拿到隧道时它为真即说明这是工具通道而非 SSH 隧道。
func (s *ShareMode) toolModeReady() bool {
	return s.currentDaemonKey() != [32]byte{}
}

// currentDaemonKey 取当前会话密钥（P2P 升级沿用 relay 握手协商出的同一把）。
func (s *ShareMode) currentDaemonKey() [32]byte {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	return s.daemonKey
}

// currentDaemon 取 daemon 引用。relay 主循环与后台 P2P 升级 goroutine 现在是并发跑的
// （旧实现里 handleTunnel 与 handleTunnelP2P 二选一、互斥），s.daemon 必须加锁访问。
func (s *ShareMode) currentDaemon() *agent.Daemon {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	return s.daemon
}

// NewShareMode 创建被协助模式。codeFile 非空时，注册成功后把协助码与有效期
// 以 JSON 原子写入该文件，供宿主程序（如管家）稳定读取，无需解析 stdout。
// mirrorCodeFile 非空时再额外写一份到该路径：通道内热升级会用它，让 old share
// 原来的 --code-file 路径在升级后仍由 new share 继续刷新（见 upgradeflags.HostCodeFile）。
// newInstance 为 true 时使用进程内 ClientID，允许 CLI 额外启动独立 share。
func NewShareMode(cfg *Config, sshAddr string, newInstance bool, sbCfg agent.SandboxConfig, codeFile, mirrorCodeFile string) *ShareMode {
	share := &ShareMode{
		client:         NewClient(cfg),
		sshAddr:        sshAddr,
		newInstance:    newInstance,
		codeFile:       codeFile,
		mirrorCodeFile: mirrorCodeFile,
		sbCfg:          sbCfg,
	}
	if newInstance {
		share.clientID, _ = generateClientID()
	}
	return share
}

// Run 运行被协助模式，协助端断开时自动等待新连接
func (s *ShareMode) Run() (string, time.Time, error) {
	defer s.client.Close()

	// 持久守护：连接/注册/隧道任何环节出错（EOF、relay 重启、网络抖动、协助码过期…）
	// 都【不退出】，一律退避重连重注册，只能由信号（Ctrl+C / systemctl stop）终止。
	// 初次连接+注册也走无限退避：relay 暂时不可达不应让 share 直接退出。
	s.reconnectWithBackoff()

	// rapidFails：连续"建立即断"的会话数，用于防热循环退避（见 rapidReconnectBackoff）。
	rapidFails := 0
	for {
		start := time.Now()
		tunnelErr := s.waitAndHandleTunnel()
		sessionDur := time.Since(start)

		isPeerDisconnect := errors.Is(tunnelErr, ErrPeerDisconnected)
		codeExpired := time.Now().After(s.expiresAt)

		// 对端断开 + 协助码仍有效 → 在当前连接上等待新的协助端（不重连不退出）
		if isPeerDisconnect && !codeExpired {
			rapidFails = 0
			fmt.Printf("\n协助端已断开连接，协助码仍有效: %s\n", formatCode(s.code))
			fmt.Println("等待新的协助端连接...")
			continue
		}

		// 其余一切情况都【不退出】：含 EOF/隧道正常关闭(tunnelErr==nil，多为 relay 重启
		// 或网络中断)、连接异常、协助码过期。统一退避重连重注册。
		switch {
		case tunnelErr == nil:
			fmt.Println("\n隧道已关闭（relay 重启或网络中断），正在重连...")
		case codeExpired:
			fmt.Println("\n连接中断且协助码已过期，正在重连获取新协助码...")
		default:
			fmt.Printf("\n连接中断: %v，协助码仍有效: %s，正在重连...\n", tunnelErr, formatCode(s.code))
		}

		// 防热循环：若隧道"建立后极短时间内即断"，reconnectWithBackoff 可能每次第 1 次
		// 就成功（不退避），连续多次就变成无间隔的疯狂重连、空转 CPU + 锤 relay。这里按
		// 连续快速失败次数线性退避；正常时长的会话重置计数、立即重连（不拖慢正常恢复）。
		var floor time.Duration
		rapidFails, floor = rapidReconnectBackoff(sessionDur, rapidFails)
		if floor > 0 {
			fmt.Printf("（隧道存活仅 %v，连续 %d 次快速断开，%v 后再重连以防空转）\n",
				sessionDur.Round(time.Millisecond), rapidFails, floor)
			time.Sleep(floor)
		}
		s.reconnectWithBackoff()
	}
}

// 重连退避参数
const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
	// rapidReconnectFloor 判定"建立即断"的会话时长阈值：隧道存活短于它即认为陷入
	// 快速失败循环，触发防热循环退避（见 rapidReconnectBackoff）。
	rapidReconnectFloor = 2 * time.Second
)

// reconnectWithBackoff 连接 relay 并注册（register 会按 ClientID 复用会话，或在协助码
// 过期时换新码）。失败按指数退避（1s,2s,4s,…，封顶 30s）【无限重试，永不放弃】——
// share 是持久守护进程，relay 重启 / 弱网瞬断 / 协助码过期都不应让它退出，只能由信号
// 终止。也用于初次连接+注册。
func (s *ShareMode) reconnectWithBackoff() {
	delay := reconnectBaseDelay
	for attempt := 1; ; attempt++ {
		s.client.Close()
		err := s.client.Connect()
		if err == nil {
			err = s.register()
		}
		if err == nil {
			if attempt > 1 {
				fmt.Printf("重连成功（第 %d 次尝试）\n", attempt)
			}
			// 心跳保活绑定 relay 连接生命周期：register 成功即启动，覆盖「等待协助端 /
			// relay 中转 / P2P 转发」全程。relay 对每个 client 有 readIdleTimeout(2min)，
			// 任一阶段静默不发消息都会被判掉线 → DisconnectClient 置 session.Share=nil →
			// 协助端无法 join（standalone 下还会自连内嵌 relay 每 2min 断一次、刷屏重连）。
			// 心跳循环绑定「这一代连接」（Client.hbStop），上一连接的那个已在本轮开头的
			// Close() 里被叫停，故全程只有一个。早先这里靠 IsClosed() 收敛，但 Connect()
			// 会立刻把 closed 复位，30s 的 tick 命不中那几微秒的窗口，旧循环全都活了下来。
			s.client.StartHeartbeatLoop(30 * time.Second)
			return
		}
		fmt.Printf("连接失败(第 %d 次): %v；%s 后重试...\n", attempt, err, delay)
		time.Sleep(delay)
		delay *= 2
		if delay > reconnectMaxDelay {
			delay = reconnectMaxDelay
		}
	}
}

// rapidReconnectBackoff 根据上一会话存活时长与连续快速失败次数，决定重连前的退避。
// 会话存活 ≥ rapidReconnectFloor（正常时长）→ 重置计数、不退避，保证正常断连快速恢复；
// 短于阈值（建立即断）→ 计数 +1，按次数线性退避（1s,2s,…，封顶 reconnectMaxDelay），
// 避免无退避地疯狂重连。返回新计数与应 sleep 的时长。
func rapidReconnectBackoff(sessionDur time.Duration, rapidFails int) (int, time.Duration) {
	if sessionDur >= rapidReconnectFloor {
		return 0, 0
	}
	rapidFails++
	delay := time.Duration(rapidFails) * time.Second
	if delay > reconnectMaxDelay {
		delay = reconnectMaxDelay
	}
	return rapidFails, delay
}

// register 向 relay 注册并获取协助码
func (s *ShareMode) register() error {
	clientID, err := s.registrationClientID()
	if err != nil {
		return err
	}
	hostInfo := sysinfo.Summary()
	if err := s.client.SendMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{ClientID: clientID, Version: version.Info(), Host: hostInfo}); err != nil {
		return err
	}

	msg, err := s.client.ReadMessage()
	if err != nil {
		return err
	}

	if msg.Type != proto.MsgRegisterResponse {
		return fmt.Errorf("unexpected response: %s", msg.Type)
	}

	var resp proto.RegisterResponse
	if err := proto.DecodePayload(msg, &resp); err != nil {
		return err
	}
	s.code = resp.Code
	s.expiresAt = time.Unix(resp.ExpiresAt, 0)
	instructions := formatShareInstructions(resp.Code, hostInfo, s.client.config, s.expiresAt)
	fmt.Printf("\n================ 请复制给 Claude/Codex ================\n%s\n", instructions)
	fmt.Println("========================================================")
	// 整段复制到系统剪贴板，让未配置 MCP 的协助端也能直接按文案中的指南完成安装。
	// 首次注册与重连刷新 code 都走到这里，剪贴板始终包含最新协助信息。
	if err := copyToClipboard(instructions); err == nil {
		fmt.Println("（以上协助信息已复制到剪贴板）")
	}
	fmt.Println("等待协助端连接...")
	s.writeCodeFile()
	return nil
}

func (s *ShareMode) registrationClientID() (string, error) {
	if s.newInstance {
		return s.clientID, nil
	}
	return GetOrCreateClientID()
}

// writeCodeFile 把协助码、中转服务地址与有效期原子写入 codeFile（先写 .tmp 再 rename），
// 供宿主程序读取。失败仅记日志，不影响协助流程。重连刷新 code 时会覆盖写入。
// mirrorCodeFile 非空时再写一份（升级后保持 old 原路径继续刷新）。
//
// server 必须一起给：光有协助码，协助端并不知道该去哪台 relay 找它——而实际连的那台是
// 「编译期默认值 → REMOTE_RELAY_SERVER → --server → 补默认端口 → --standalone 改写」
// 一路算出来的，宿主程序无从推断（standalone 下更是跟命令行上写的完全不是一回事）。
func (s *ShareMode) writeCodeFile() {
	payload := struct {
		Code      string `json:"code"`
		Server    string `json:"server"`
		ExpiresAt int64  `json:"expiresAt"`
	}{Code: s.code, Server: s.client.config.ServerAddr, ExpiresAt: s.expiresAt.Unix()}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("写协助码文件失败(marshal): %v", err)
		return
	}

	for _, path := range []string{s.codeFile, s.mirrorCodeFile} {
		if path != "" {
			writeCodeFileTo(path, data)
		}
	}
}

// writeCodeFileTo 原子写单个协助码文件（.tmp + rename，0600）。失败仅记日志。
func writeCodeFileTo(path string, data []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("写协助码文件失败(write %s): %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("写协助码文件失败(rename %s): %v", path, err)
		os.Remove(tmp)
	}
}

// waitAndHandleTunnel 等待协助端连接，然后处理隧道。
//
// P2P 改为后台热升级：relay 主循环常驻，隧道打通并**双向证实**后才把流量切过去。
//
// 旧实现在这里同步等 P2P 结果、再二选一进 handleTunnel / handleTunnelP2P。但 UDP
// 打洞的成功判定天然是单向的（收到对方一个包就宣告成功、不等回应），两端完全可能
// 得出相反结论：share 认为通了就一头扎进 P2P 隧道死等 mode header（要 60s
// peerTimeout 才醒）且期间根本不读 relay，而 help 认为没通、在 relay 上发 ToolHello
// 无人应答 → MCP connect 报 "handshake failed: i/o timeout"。
// 现在 relay 永远有人读，P2P 成不成都不影响会话可用。
func (s *ShareMode) waitAndHandleTunnel() error {
	// 必须在 waitSessionReady 之前开新一轮：ShareMode 对象跨会话复用，daemonKey 不清
	// 就会永久为真，之后每个纯 SSH 会话都被误判成工具模式、走 8s mode header 超时，
	// 而 SSH 的首字节要等用户真正开连接（可能几分钟）——直连被白白掐掉，
	// --p2p required 下更会每 8s 拆一次健康会话。
	// 放在前面还顺带保证：会话重同步窗口里（waitSessionReady 内）收到的 ToolHello
	// 所设的 key 属于本轮，不会被随后的 begin 抹掉。
	epoch, sessionDone := s.beginP2PSession()
	defer s.endP2PSession()

	sessionID, err := s.waitSessionReady()
	if err != nil {
		return err
	}

	s.launchP2PUpgrade(sessionID, epoch, sessionDone)

	fmt.Println("开始中转转发流量（P2P 将在后台尝试升级）...")
	return s.handleTunnel()
}

// launchP2PUpgrade 只负责启动后台任务，绝不在 relay 读循环的关键路径里执行 Start。
// Start 包含多轮 STUN/NAT 探测，UDP 被限制时可能耗时数十秒；同步执行会让 share 无法
// 读取紧随 SessionReady 到达的 ToolHello，最终撞上 help 端 15 秒握手超时。
func (s *ShareMode) launchP2PUpgrade(sessionID string, epoch uint64, sessionDone <-chan struct{}) {
	mode := p2p.ParseP2PMode(s.client.config.P2PMode)
	if mode == p2p.P2PModeDisabled {
		return
	}

	go func() {
		mgr := s.makeP2PManager(mode)
		mgr.SetRelayConn(s.client)
		resultCh, startErr := mgr.Start(sessionID, true)
		if startErr != nil {
			log.Printf("P2P manager start failed: %v", startErr)
			active := s.clearP2PAttempt(epoch, mode == p2p.P2PModeRequired)
			mgr.Close()
			if mode == p2p.P2PModeRequired && active {
				fmt.Println("P2P 直连失败，--p2p required 下不接受中转，断开重试...")
			}
			return
		}
		if !s.attachP2PMgr(epoch, mgr) {
			mgr.Close()
			return
		}
		s.upgradeToP2P(mgr, resultCh, epoch, mode, sessionDone)
	}()
}

// upgradeToP2P 后台等打洞结果，成功则把流量切到隧道。auto 模式下全程「失败即静默留在
// relay」；required 模式下失败必须让会话失败——用户明确表示不接受中转，不能悄悄降级。
func (s *ShareMode) upgradeToP2P(mgr shareP2PManager, resultCh <-chan p2p.P2PResult, epoch uint64, mode p2p.P2PMode, sessionDone <-chan struct{}) {
	// abandon 收拾隧道与 manager；required 模式下额外拆掉 relay 连接，让 handleTunnel
	// 返回错误、由 Run 的重连循环重来，而不是留在它不接受的中转上。
	abandon := func(tunnel *p2p.UDPTunnel, reason string) {
		active := s.clearP2PAttempt(epoch, mode == p2p.P2PModeRequired)
		if tunnel != nil {
			tunnel.Close()
		}
		mgr.Close()
		if mode == p2p.P2PModeRequired && active {
			log.Printf("P2P required but failed (%s), dropping session", reason)
			fmt.Println("P2P 直连失败，--p2p required 下不接受中转，断开重试...")
		}
	}

	var result p2p.P2PResult
	var ok bool
	select {
	case result, ok = <-resultCh:
	case <-sessionDone:
		// 会话已结束。不能只等 resultCh：manager 被 Close 时不会往它推任何东西，
		// 干等就是永久泄漏一个 goroutine。
		mgr.Close()
		return
	}
	if !ok || result.Tunnel == nil {
		reason := "negotiation produced no tunnel"
		if result.Err != nil {
			reason = result.Err.Error()
			log.Printf("P2P negotiation failed: %v", result.Err)
		} else {
			fmt.Println("P2P 未建立，继续使用中转（不影响使用）")
		}
		// 必须关掉 manager：否则它的 backgroundRetry 会继续打洞，之后成功了就往
		// 没人读的 resultCh 塞一条隧道，那条隧道既不会被使用也不会被关闭。
		abandon(nil, reason)
		return
	}
	tunnel := result.Tunnel
	// 会话可能在打洞期间就结束了（对端断开 / relay 抖动）——此时这条隧道属于上一轮，
	// 绝不能拿它去动新一轮的 daemon。
	if !s.setP2PTunnel(epoch, tunnel) {
		tunnel.Close()
		return
	}

	// 工具模式与 SSH 模式对 mode header 的等待策略必须区分：
	//   - 工具模式：help 已完成 relay 握手才开始打洞，mode header 应立刻就到，
	//     等不到就是反方向不通 → 限时等待，超时关隧道回退 relay。
	//   - SSH 模式：隧道建成后要等用户真正发起 SSH 连接才有第一个字节，可能是几分钟，
	//     **不能**设超时，否则会把好端端的 SSH 直连掐掉。
	if s.toolModeReady() {
		toolMode, _, err := ReadModeHeaderTimeout(tunnel, p2pModeHeaderTimeout)
		if err != nil {
			log.Printf("P2P mode header not received (%v), tool traffic stays on relay", err)
			abandon(tunnel, "mode header timeout")
			return
		}
		if !toolMode {
			log.Printf("P2P tunnel carried SSH bytes while in tool mode, staying on relay")
			abandon(tunnel, "unexpected SSH bytes on tool-mode tunnel")
			return
		}
		s.handleToolOverP2P(tunnel, epoch)
		// 隧道用完即断。required 下同样不接受降级到 relay —— abandon 会据此断会话。
		abandon(nil, "P2P tunnel closed")
		return
	}

	toolMode, consumed, err := ReadModeHeader(tunnel)
	if err != nil {
		abandon(tunnel, "mode header read error")
		return
	}
	if toolMode {
		s.handleToolOverP2P(tunnel, epoch)
		abandon(nil, "P2P tunnel closed")
		return
	}
	fmt.Println("P2P 直连已建立，SSH 流量走直连...")
	// 只记日志，不动 relay 连接。
	//
	// handleSSHTunnelP2P 对任何隧道读错误都返回 ErrPeerDisconnected，包括「relay 健康、
	// 只是 P2P 断了」。这种情况下 SSH 流量会自然回落到 relay 主循环的 TunnelData 分支，
	// 会话照常。真的是对端断开时，relay 会推 PEER_DISCONNECTED，handleTunnel 自会返回
	// ErrPeerDisconnected 走保留协助码的快速路径。
	// 早先在这里 s.client.Close() 是错的：它让 ReadMessage 报「use of closed network
	// connection」，Run 落进 default 分支去 reconnectWithBackoff → register()，
	// 白白换掉协助码并重新打印。
	if sshErr := s.handleSSHTunnelP2P(tunnel, consumed[:]); sshErr != nil {
		log.Printf("SSH over P2P ended: %v", sshErr)
	}
	abandon(nil, "SSH over P2P ended")
}

// waitSessionReady 等待会话就绪
func (s *ShareMode) waitSessionReady() (string, error) {
	for {
		msg, err := s.client.ReadMessage()
		if err != nil {
			return "", err
		}

		switch msg.Type {
		case proto.MsgSessionReady:
			var ready proto.SessionReady
			if err := proto.DecodePayload(msg, &ready); err != nil {
				return "", err
			}
			fmt.Println("协助端已连接！")
			if ready.PeerVersion != "" {
				fmt.Printf("对端版本: %s\n", ready.PeerVersion)
			}
			if ready.PeerHost != "" {
				fmt.Printf("对端标识: %s\n", ready.PeerHost)
			}
			return ready.SessionID, nil
		case proto.MsgHeartbeat:
		case proto.MsgToolHello:
			// 会话重同步窗口（协助端重连 / session 切换）可能收到工具通道消息。
			// ToolHello 必须在 relay 上就地应答，并把 daemon 从可能残留的旧 P2P
			// 连接切回 relay。
			if err := s.handleRelayToolHello(msg); err != nil {
				return "", err
			}
		case proto.MsgToolReq, proto.MsgToolCancel:
			if d := s.currentDaemon(); d != nil {
				d.Inject(msg)
			}
		case proto.MsgError:
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			return "", fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		default:
			log.Printf("Unexpected message: %s", msg.Type)
		}
	}
}

// handleToolOverP2P 在已建成的 P2P 隧道上跑工具通道。
//
// 返回即代表隧道不可用，**不再判死整个会话**：relay 主循环一直在跑，daemon 的响应
// 出口会被切回 relay，工具调用继续可用。旧实现在这里 return ErrPeerDisconnected，
// P2P 一有闪失就把整条会话报废，是本次要修掉的行为之一。
func (s *ShareMode) handleToolOverP2P(tunnel *p2p.UDPTunnel, epoch uint64) {
	pc := NewP2PConn(tunnel)
	defer func() {
		tunnel.Close()
		// 隧道已死，主动把 daemon 出口切回 relay，不必干等 help 重发 ToolHello。
		s.swapDaemonToRelay(epoch)
	}()

	swapped := false
	for {
		msg, err := pc.ReadMessage()
		if err != nil {
			log.Printf("P2P tool channel closed (%v), tool traffic falls back to relay", err)
			return
		}
		if !s.p2pEpochValid(epoch) {
			return // 会话已翻篇，这条隧道上的一切都不该再影响 daemon
		}
		switch msg.Type {
		case proto.MsgHeartbeat:
			// 回 pong。这是对端判断「反方向也通」的唯一依据：打洞成功只证明我收得到
			// 它，不证明它收得到我。不回这个 pong，对端就会放弃 P2P 留在 relay
			// （旧版 share 正是如此，属预期内的向后兼容降级）。
			//
			// 注意这里**不能**顺手把出口切到隧道：发出 pong 不等于 pong 到达。若
			// share→help 方向不通，对端探活会超时并留在 relay，而我已经把响应写向
			// 一条它根本收不到的隧道——请求从 relay 来、响应进黑洞，要等 60s
			// peerTimeout 才暴露。切换的时机放到下面收到真实请求时。
			if err := pc.SendMessage(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()}); err != nil {
				log.Printf("P2P probe pong failed (%v), staying on relay", err)
				return
			}
		case proto.MsgToolHello:
			// 向后兼容：旧版 help 会在 P2P 隧道上做完整工具握手（而非探活 + 复用 key）。
			var hello proto.Hello
			proto.DecodePayload(msg, &hello)
			ack, key := buildHelloAck(hello, s.code)
			pc.SendMessage(proto.MsgToolHelloAck, &ack)
			if ack.Accept {
				s.ensureDaemon(key) // 已在锁内把 daemonKey 设为 key
				if s.swapDaemonTo(pc, epoch) {
					swapped = true
				}
			}
		case proto.MsgToolReq, proto.MsgToolCancel:
			// 收到走 P2P 的真实工具请求 ⟹ 对端的双向探活已经通过并切了过来。这才是
			// share 侧「两个方向都可用」的可靠证据（对端只有收到我的 pong 才会切）。
			// 到这一步再把 daemon 的响应出口切到隧道，保证响应与请求同路。
			// 沿用 relay 握手已协商的会话密钥，key 不变 → SwapConn 不会轮换 key，
			// 也就不会取消正在跑的工具调用。
			if !swapped && s.swapDaemonTo(pc, epoch) {
				swapped = true
				log.Printf("P2P confirmed by inbound tool request, response path switched to P2P")
				fmt.Println("已切换到 P2P 直连")
			}
			if d := s.currentDaemon(); d != nil {
				d.Inject(msg)
			}
		}
	}
}

// swapDaemonToRelay 把 daemon 的响应出口切回 relay。P2P 隧道断开后调用：请求会从
// relay 进来，响应必须跟着回 relay，否则会写向一条已死的隧道。
// epoch 失效说明会话已经换代，此时 daemon 归新一轮所有，不能碰。
//
// 取 key、查 epoch、换 conn 必须在同一临界区内完成。这三步和
// handleRelayToolHello → ensureDaemon 是由同一个事件触发的（help 的 downgradeToRelay
// 关掉隧道后立刻重发 ToolHello）：若在锁外分三次做，可能先采样到旧 key，等重新握手
// 装好新 key 之后再把旧 key 盖回去，daemon 此后用错 key 解密，整条会话余下的请求
// 全部 decrypt_failed。
func (s *ShareMode) swapDaemonToRelay(epoch uint64) {
	s.swapDaemonTo(s.client, epoch)
}

// swapDaemonTo 在同一临界区内完成「取当前 key → 校验 epoch → 换出口」，返回是否真的
// 切换了。所有换出口的路径都必须走它，理由见 swapDaemonToRelay 的注释。
func (s *ShareMode) swapDaemonTo(conn agent.MsgConn, epoch uint64) bool {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	if s.daemon == nil || s.daemonKey == [32]byte{} || s.p2pEpoch != epoch {
		return false
	}
	s.daemon.SwapConn(conn, s.daemonKey)
	return true
}

// handleSSHTunnelP2P handles SSH-over-P2P (original behavior), with the first
// consumed bytes from mode detection prepended to the SSH stream.
func (s *ShareMode) handleSSHTunnelP2P(tunnel *p2p.UDPTunnel, prefix []byte) error {
	defer tunnel.Close()

	var sshConn net.Conn
	var connMu sync.Mutex

	connectSSH := func() net.Conn {
		connMu.Lock()
		defer connMu.Unlock()

		if sshConn != nil {
			return sshConn
		}

		conn, err := net.Dial("tcp", s.sshAddr)
		if err != nil {
			log.Printf("Failed to connect to local SSH: %v", err)
			return nil
		}
		sshConn = conn
		fmt.Println("已连接到本地SSH服务 (P2P直连)...")

		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					break
				}
				if _, err := tunnel.Write(buf[:n]); err != nil {
					break
				}
			}
			connMu.Lock()
			if sshConn == conn {
				sshConn = nil
			}
			connMu.Unlock()
			conn.Close()
			fmt.Println("本地SSH连接已断开，等待新的SSH会话...")
		}()

		return conn
	}

	// prefixPending holds the 2 bytes consumed during mode detection. They
	// are prepended to the very first tunnel.Read payload so both go to the
	// same SSH connection in one Write call (no split / lost-prefix risk).
	prefixPending := prefix

	buf := make([]byte, 32*1024)
	for {
		n, err := tunnel.Read(buf)
		if err != nil {
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			// P2P 隧道断开，视为协助端断开
			return ErrPeerDisconnected
		}
		if n == 0 {
			continue
		}

		// Merge prefix (mode-header bytes) with first payload
		data := buf[:n]
		if len(prefixPending) > 0 {
			merged := make([]byte, len(prefixPending)+n)
			copy(merged, prefixPending)
			copy(merged[len(prefixPending):], buf[:n])
			data = merged
			prefixPending = nil
		}

		conn := connectSSH()
		if conn != nil {
			if _, err := conn.Write(data); err != nil {
				connMu.Lock()
				if sshConn == conn {
					sshConn = nil
				}
				connMu.Unlock()
				conn.Close()
			}
		}
	}
}

// handleTunnel 处理隧道（支持多次SSH连接）
func (s *ShareMode) handleTunnel() error {
	// 心跳已由 reconnectWithBackoff 在连接建立时启动（覆盖等待/中转/P2P 全生命周期），此处不再重复

	// Shared state: current SSH connection to local SSH server
	var sshConn net.Conn
	var connMu sync.Mutex
	closeSSH := func() {
		connMu.Lock()
		conn := sshConn
		sshConn = nil
		connMu.Unlock()
		if conn != nil {
			conn.Close()
		}
	}

	// connectSSH lazily connects to local SSH and starts a reader goroutine
	connectSSH := func() net.Conn {
		connMu.Lock()
		defer connMu.Unlock()

		if sshConn != nil {
			return sshConn
		}

		conn, err := net.Dial("tcp", s.sshAddr)
		if err != nil {
			log.Printf("Failed to connect to local SSH: %v", err)
			return nil
		}
		sshConn = conn
		fmt.Println("已连接到本地SSH服务...")

		// Start SSH → relay goroutine
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					break
				}
				if err := s.client.SendMessage(proto.MsgTunnelData, &proto.TunnelData{Data: buf[:n]}); err != nil {
					break
				}
			}
			connMu.Lock()
			if sshConn == conn {
				sshConn = nil
			}
			connMu.Unlock()
			conn.Close()
			fmt.Println("本地SSH连接已断开，等待新的SSH会话...")
		}()

		return conn
	}

	// Main loop: read from relay, forward to SSH (connecting on demand)
	for {
		// Set read timeout to detect dead connections (reset after each successful read)
		s.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
		msg, err := s.client.ReadMessage()
		if err != nil {
			closeSSH()
			if err.Error() == "EOF" {
				return nil
			}
			return err
		}

		switch msg.Type {
		case proto.MsgSessionReady:
			// relay 对 helper 断连有 5 秒去抖；新 helper 在窗口内重连时会直接替换旧
			// helper 并下发新的 SessionReady，share 不会先收到 PEER_DISCONNECTED。
			// 因此常驻主循环必须在这里开启新 epoch、关闭旧隧道并重建 manager。
			var ready proto.SessionReady
			if err := proto.DecodePayload(msg, &ready); err != nil {
				return err
			}
			closeSSH()
			fmt.Println("新的协助端已连接，正在重建 P2P 协商...")
			if ready.PeerVersion != "" {
				fmt.Printf("对端版本: %s\n", ready.PeerVersion)
			}
			if ready.PeerHost != "" {
				fmt.Printf("对端标识: %s\n", ready.PeerHost)
			}
			epoch, sessionDone := s.beginP2PSession()
			s.launchP2PUpgrade(ready.SessionID, epoch, sessionDone)
		case proto.MsgToolHello:
			if err := s.handleRelayToolHello(msg); err != nil {
				return err
			}
		case proto.MsgPeerAddrReady:
			// 转喂后台协商中的 P2P manager。协商不再自己抢读 relay，relay 始终
			// 只有本循环一个读者 —— 这也顺带消除了原先协商设 read deadline 后
			// 需要 ResetDecoder 的隐患。
			var ready proto.PeerAddrReady
			if err := proto.DecodePayload(msg, &ready); err == nil {
				s.deliverPeerAddrReady(&ready)
			}
		case proto.MsgToolReq, proto.MsgToolCancel:
			if d := s.currentDaemon(); d != nil {
				d.Inject(msg)
			}
		case proto.MsgTunnelData:
			var dataMsg proto.TunnelData
			if err := json.Unmarshal(msg.Payload, &dataMsg); err != nil {
				continue
			}
			conn := connectSSH()
			if conn != nil {
				if _, err := conn.Write(dataMsg.Data); err != nil {
					connMu.Lock()
					if sshConn == conn {
						sshConn = nil
					}
					connMu.Unlock()
					conn.Close()
				}
			}
		case proto.MsgHeartbeat:
			// ignore
		case proto.MsgError:
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			closeSSH()
			if errMsg.Code == "PEER_DISCONNECTED" {
				return ErrPeerDisconnected
			}
			return fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		}
	}
}

// GetCode 获取协助码
func (s *ShareMode) GetCode() string {
	return s.code
}

// GetExpiresAt 获取过期时间
func (s *ShareMode) GetExpiresAt() time.Time {
	return s.expiresAt
}

func formatCode(code string) string {
	if len(code) < 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// daemonSink share 端把 Tool 消息转给 agent.Daemon 的契约
type daemonSink interface {
	Inject(msg *proto.Message)
}

// dispatchToolMessage 若 msg 属于工具通道则投递并返回 true，否则 false。
// 注意：Task 13 仅提供 helper；Task 17 才在 handleTunnel 的 dispatch 循环中调用并启动 daemon。
func dispatchToolMessage(msg *proto.Message, d daemonSink) bool {
	switch msg.Type {
	case proto.MsgToolReq, proto.MsgToolCancel, proto.MsgToolHello:
		if d != nil {
			d.Inject(msg)
		}
		return true
	}
	return false
}

// buildHelloAck share 端对 Hello 的应答 + 派生 session_key
func buildHelloAck(hello proto.Hello, code string) (proto.HelloAck, [32]byte) {
	if hello.Version != proto.ToolProtocolVersion {
		return proto.HelloAck{
			Version:  proto.ToolProtocolVersion,
			Accept:   false,
			ErrorMsg: "unsupported tool protocol version: " + hello.Version,
		}, [32]byte{}
	}
	ack := proto.HelloAck{
		Version:      proto.ToolProtocolVersion,
		Capabilities: []string{"exec", "read_file", "write_file", "list_dir", "stat", "glob", "grep", "process_list", "tail_log"},
		NonceB64:     proto.NewHello().NonceB64,
		Accept:       true,
	}
	key := proto.DeriveSessionKey(code, ack.NonceB64, hello.NonceB64)
	return ack, key
}

// handleRelayToolHello 在 relay 通道完成工具握手，并明确把 daemon 的响应出口切回
// relay。daemon 可能曾被 P2P 握手 SwapConn 到 P2PConn；若后续重连回退 relay，仅轮换
// key 会让请求从 relay 进入、响应却继续写向已失效的旧 P2P 隧道。
func (s *ShareMode) handleRelayToolHello(msg *proto.Message) error {
	var hello proto.Hello
	if err := proto.DecodePayload(msg, &hello); err != nil {
		return fmt.Errorf("decode relay tool hello: %w", err)
	}
	ack, key := buildHelloAck(hello, s.code)
	if err := s.client.SendMessage(proto.MsgToolHelloAck, &ack); err != nil {
		return fmt.Errorf("send relay tool hello ack: %w", err)
	}
	if ack.Accept {
		s.ensureDaemon(key)
		if d := s.currentDaemon(); d != nil {
			d.SwapConn(s.client, key)
		}
	}
	return nil
}

// ensureDaemon 首次 hello 时构造 daemon；后续 hello 仅 rotate key。
// sync.Once 保证 reg/goroutine 只初始化一次；每次 hello 后都用最新 key 覆盖。
func (s *ShareMode) ensureDaemon(key [32]byte) {
	s.daemonOnce.Do(func() {
		reg := agent.NewRegistry()
		sb := agent.NewSandbox(s.sbCfg)
		reg.Register(tools.NewExec(sb))
		reg.Register(tools.NewReadFile(sb))
		reg.Register(tools.NewWriteFile(sb))
		reg.Register(tools.NewFileMD5(sb))
		reg.Register(tools.NewListDir(sb))
		reg.Register(tools.NewStat(sb))
		reg.Register(tools.NewGlob(sb))
		reg.Register(tools.NewGrep(sb))
		reg.Register(tools.NewProcessList())
		reg.Register(tools.NewTailLog(sb))
		d := agent.NewDaemon(reg, s.client, key)
		d.OnActivity = func(line string) { fmt.Println(line) }
		s.p2pMu.Lock()
		s.daemon = d
		s.p2pMu.Unlock()
		go d.RunLoop(context.Background())
	})
	// 不管是首次还是续连，都用最新 key 覆盖（首次 RotateKey 等同于设置已有 key，无害）
	d := s.currentDaemon()
	if d == nil {
		return
	}
	d.RotateKey(key)
	// 记下当前会话密钥：P2P 升级时沿用它，无需在隧道上再握一次手。
	s.p2pMu.Lock()
	s.daemonKey = key
	s.p2pMu.Unlock()
}
