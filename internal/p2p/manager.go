package p2p

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// P2PMode defines the P2P mode
type P2PMode int

const (
	P2PModeDisabled P2PMode = iota
	P2PModeAuto             // Try P2P first, fall back to relay
	P2PModeRequired         // Only use P2P, fail if not possible
)

// ParseP2PMode 将字符串转换为 P2PMode
func ParseP2PMode(s string) P2PMode {
	switch s {
	case "auto":
		return P2PModeAuto
	case "required":
		return P2PModeRequired
	default:
		return P2PModeDisabled
	}
}

// P2PResult P2P 协商结果
type P2PResult struct {
	Tunnel *UDPTunnel // 非 nil 表示 P2P 成功
	Err    error      // 非 nil 表示 required 模式下失败
}

// PeerInfo represents a peer's network information
type PeerInfo struct {
	PublicAddr  *net.UDPAddr
	PrivateAddr *net.UDPAddr
}

// P2PManager manages P2P connection attempts
type P2PManager struct {
	mode       P2PMode
	stunServer string
	stunAddr   *net.UDPAddr // resolved STUN server address for UDP relay
	// stunCandidates relay 下发的 STUN（RegisterResponse/JoinResponse.STUNServers），
	// fallbackSTUN 内置兜底；两者只参与公网地址发现与 NAT 判定，UDP 中继仍只用 stunServer。
	stunCandidates []string
	fallbackSTUN   []string
	stunUsed       string        // 实际采用的 STUN（第一个应答者），随地址上报
	stunElapsed    time.Duration // 发现耗时
	localConn      *net.UDPConn
	localAddr      *net.UDPAddr
	publicAddr     *net.UDPAddr
	peerInfo       *PeerInfo
	sessionID      string
	isShare        bool

	// authCode 协助码，用来给打洞包签 MAC / 验 MAC（见 proto/punch.go）。
	// 为空时本端签不出 MAC、也验不过任何包，P2P 因此谈不成并回落 relay——
	// 这是刻意的 fail-closed：宁可丢掉 P2P，也不要退回"谁都能冒充对端"。
	authCode string

	connected   bool
	connectedMu sync.RWMutex
	relayConn   RelayConn
	resultCh    chan P2PResult
	stopChan    chan struct{}
	closeOnce   sync.Once
	punchPlan   punchPlan // 打洞策略（由 relay 的同网 hint 与私网段推导，见 planHolePunch）
	bindIP      string    // 用户手动指定的绑定 IP
	natType     string    // 本端 NAT 类型
	peerNATType string    // 对端 NAT 类型
	// backgroundMode 后台重试模式（仅记录日志，不创建 tunnel）。
	// 必须是原子的：写在 backgroundRetry 的 goroutine 里，读在 receiveLoop / 生日攻击
	// 的 goroutine 里（onP2PConnected），两者之间没有任何 happens-before 边，裸字段在
	// -race 下就是一条实打实的 data race。
	backgroundMode atomic.Bool
	// recvStop 让 receiveLoop 在隧道接管 localConn 之前就停下，避免两个 goroutine
	// 并发读同一个 UDP socket（内核会把数据报随机派给其中一个）。
	recvStop     chan struct{}
	recvStopOnce sync.Once
	// tunnelMu 保护隧道的交接：onP2PConnected 的「投递」与 Close 的「抽干 + 回收
	// localConn」必须互斥，否则两者交错时仍会漏出孤儿隧道或反过来关掉在用的隧道。
	tunnelMu sync.Mutex
	// tunnelCreated 表示隧道已成功投递进 resultCh（由 tunnelMu 保护）。Close 用它区分
	// 「隧道已被接手，localConn 归消费者管」与「从未建过隧道，localConn 无主」。
	tunnelCreated bool

	// localIPs 列出可绑定的本机 IPv4（优先级排序，第一个是默认首选）；测试可替换。
	localIPs func() []net.IP
	// raceOpts STUN 竞速时序；测试可缩短。
	raceOpts RaceOptions
	// preferredGrace 见 defaultPreferredNICGrace；测试可缩短。
	preferredGrace time.Duration
}

// stopReceiveLoop 幂等地叫停 receiveLoop。
func (p *P2PManager) stopReceiveLoop() {
	p.recvStopOnce.Do(func() { close(p.recvStop) })
}

// RelayConn is the interface for relay fallback communication
type RelayConn interface {
	SendMessage(msgType proto.MessageType, payload interface{}) error
	ReadMessage() (*proto.Message, error)
	Close()
}

// NewP2PManager creates a new P2P manager
// bindIP 可选，为空则自动检测物理网卡 IP
func NewP2PManager(mode P2PMode, stunServer string, bindIP string) *P2PManager {
	return &P2PManager{
		mode:         mode,
		stunServer:   stunServer,
		fallbackSTUN: FallbackSTUNServers,
		bindIP:       bindIP,
		stopChan:     make(chan struct{}),
		recvStop:     make(chan struct{}),
		localIPs:     physicalIPCandidates,
		raceOpts:     DefaultRaceOptions,

		preferredGrace: defaultPreferredNICGrace,
	}
}

// SetSTUNCandidates 注入 relay 下发的 STUN 服务器，必须在 Start 之前调用。
func (p *P2PManager) SetSTUNCandidates(servers []string) {
	p.stunCandidates = append([]string(nil), servers...)
}

// SetRelayConn sets the relay connection for fallback and signaling
func (p *P2PManager) SetRelayConn(conn RelayConn) {
	p.relayConn = conn
}

// SetAuthCode 注入协助码，必须在 Start 之前调用。
func (p *P2PManager) SetAuthCode(code string) {
	p.authCode = code
}

// newTestPacket 造一个已签名的打洞包。所有发包路径都必须走它，漏签的包对端会直接丢弃。
func (p *P2PManager) newTestPacket() *proto.P2PTestPacket {
	pkt := &proto.P2PTestPacket{
		SessionID: p.sessionID,
		Random:    randomString(16),
	}
	proto.SignPunchPacket(pkt, p.authCode, p.isShare)
	return pkt
}

// acceptPunch 判断收到的打洞包是否可信：会话对得上，且 MAC 用对端身份验得过。
func (p *P2PManager) acceptPunch(pkt *proto.P2PTestPacket, from *net.UDPAddr) bool {
	if pkt.SessionID != p.sessionID {
		return false
	}
	if !proto.VerifyPunchPacket(pkt, p.authCode, !p.isShare) {
		// 打日志而不是静默丢弃：对端版本过旧（不发 MAC）和真有人在冒充，现场表现
		// 都是"P2P 谈不成、回落 relay"，不留一行根本无从区分。
		log.Printf("P2P: dropped unauthenticated punch packet from %v (peer too old, or spoofed)", from)
		return false
	}
	return true
}

// Start starts the P2P manager
func (p *P2PManager) Start(sessionID string, isShare bool) (<-chan P2PResult, error) {
	p.sessionID = sessionID
	p.isShare = isShare
	p.resultCh = make(chan P2PResult, 2)

	if p.mode == P2PModeDisabled {
		p.resultCh <- P2PResult{}
		return p.resultCh, nil
	}

	// 确定绑定 IP：手动指定 > 自动检测物理网卡 > 默认 0.0.0.0
	listenIP := net.IPv4zero
	var altIPs []net.IP // 自动检测时首选之外的候选网卡，STUN 发现时一起实测
	if p.bindIP != "" {
		if ip := net.ParseIP(p.bindIP); ip != nil {
			listenIP = ip
			log.Printf("Using user-specified bind IP: %v", ip)
		} else {
			log.Printf("Invalid bind IP %q, falling back to auto-detect", p.bindIP)
		}
	}
	if listenIP.Equal(net.IPv4zero) {
		if ips := p.localIPs(); len(ips) > 0 {
			listenIP = ips[0]
			altIPs = ips[1:]
			log.Printf("Proxy bypass: binding to physical NIC %v", listenIP)
		}
	}

	// Create local UDP socket
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: listenIP, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
	// 在 tunnelMu 下登记：Close 可能与 Start 并发（help 在 Start 之前就把 manager 交给了
	// teardown），它在同一把锁下回收 localConn。Close 已开始就不再登记，否则这个 socket 没人关。
	p.tunnelMu.Lock()
	select {
	case <-p.stopChan:
		p.tunnelMu.Unlock()
		conn.Close()
		return nil, net.ErrClosed
	default:
		p.localConn = conn
	}
	p.tunnelMu.Unlock()
	p.localAddr = p.localConn.LocalAddr().(*net.UDPAddr)
	// 如果绑定了具体 IP，localAddr 已经是正确的；否则尝试获取出站 IP
	if listenIP.Equal(net.IPv4zero) {
		if outboundIP := getOutboundIP(); outboundIP != nil {
			log.Printf("Local outbound IP: %v", outboundIP)
			p.localAddr = &net.UDPAddr{IP: outboundIP, Port: p.localAddr.Port}
		} else {
			log.Printf("getOutboundIP failed, advertising wildcard private address")
		}
	} else {
		log.Printf("Local bind address: %v", p.localAddr)
	}

	// Resolve STUN server address for UDP relay
	if p.stunServer != "" {
		p.stunAddr, _ = net.ResolveUDPAddr("udp", p.stunServer)
	}

	// 公网地址发现：本端配置（--stun 或默认 relay:3478）+ relay 下发 + 内置兜底一起竞速，
	// 谁先应答用谁；任何一台不通都不再串行干等（旧实现单台 16s）。stunServer 为空表示
	// standalone/loopback relay：保持不做 STUN（下发列表非空时除外）。
	if p.stunServer != "" || len(p.stunCandidates) > 0 {
		candidates := MergeSTUNCandidates(p.stunServer, p.stunCandidates, p.fallbackSTUN)
		probes, err := p.discoverAcrossNICs(candidates, altIPs)
		if err != nil {
			log.Printf("STUN discovery failed (%v), will try without public address", err)
		} else {
			p.publicAddr = probes[0].Mapped
			p.stunUsed = probes[0].Server
			p.stunElapsed = probes[0].Elapsed
			log.Printf("Discovered public address %v via %s in %v (%d answers from %d STUN candidates)",
				p.publicAddr, p.stunUsed, p.stunElapsed.Round(time.Millisecond), len(probes), len(candidates))

			// NAT 类型：用竞速里两台不同 IP、映射 IP 相同的服务器看到的映射端口判定（同一个主
			// socket，比旧的临时 socket 探测更准，也省一轮查询）。凑不够两台可比的应答时按
			// Cone，与旧逻辑「数据不足按 Cone」一致，但不会再因为 relay:3478 不通而被直接判 Cone。
			p.natType = NATFromProbes(p.localAddr.IP, probes)
			if p.natType == NATUnknown {
				log.Printf("NAT detect: fewer than two comparable STUN answers, assuming cone")
				p.natType = NATCone
			}
			log.Printf("NAT type detected: %s", p.natType)
		}
	}

	// Advertise our address via relay
	if err := p.advertiseAddr(); err != nil {
		log.Printf("Failed to advertise address: %v", err)
	}

	// Start receiving
	go p.receiveLoop()

	// Start timeout for P2P attempt
	go p.p2pTimeout()

	return p.resultCh, nil
}

// maxProbeNICs 首选之外最多再实测几块网卡：每块都会在各 STUN 候选上开一条映射、发几个包。
const maxProbeNICs = 3

// defaultPreferredNICGrace 备选网卡已拿到应答后，再给首选网卡多久：首选在这段时间内应答仍用
// 首选（保持原行为），否则改绑备选，不必等首选跑满竞速时限（默认 4s）。取 1.5s：盖住
// DefaultRaceOptions 首次重传（1s）加一个 RTT，首包丢失（Wi-Fi 唤醒、ARP）靠重传才通的
// 首选网卡不会被抢切到备选。
const defaultPreferredNICGrace = 1500 * time.Millisecond

type nicRace struct {
	conn   *net.UDPConn
	probes []STUNProbe
	err    error
}

// discoverAcrossNICs 在 p.localConn（首选网卡）和 altIPs 各自的新 socket 上并行做 STUN 竞速，
// 按实测结果决定绑哪块网卡，而不是按网卡名或类型去猜哪块是虚拟的：
//   - 首选网卡拿到应答：用它（与原行为一致），立即关掉其余 socket，不等它们跑满时限；
//   - 备选网卡先拿到应答：再等首选 preferredGrace，首选仍无应答就改绑最先应答的备选（典型：
//     全隧道 VPN 接管了默认路由、又不放行到公网 STUN 的 UDP，而物理网卡能直接出去）；
//   - 都没有应答：保留首选网卡，返回首选的错误（照旧回落中转）。
//
// 改绑在 tunnelMu 下替换 p.localConn / p.localAddr：Close 可能与 Start 并发（help 在 Start
// 之前就把 manager 登记给了 teardown），它在同一把锁下回收 localConn。
func (p *P2PManager) discoverAcrossNICs(servers []string, altIPs []net.IP) ([]STUNProbe, error) {
	if len(altIPs) > maxProbeNICs {
		altIPs = altIPs[:maxProbeNICs]
	}
	primary := p.localConn
	conns := []*net.UDPConn{primary}
	for _, ip := range altIPs {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 0})
		if err != nil {
			log.Printf("STUN: skip NIC %v: %v", ip, err)
			continue
		}
		conns = append(conns, c)
	}
	if len(conns) == 1 {
		return RaceDiscover(primary, servers, p.raceOpts)
	}

	results := make(chan nicRace, len(conns))
	for _, c := range conns {
		go func(c *net.UDPConn) {
			probes, err := RaceDiscover(c, servers, p.raceOpts)
			results <- nicRace{conn: c, probes: probes, err: err}
		}(c)
	}

	var (
		primaryErr  error
		primaryDone bool
		alt         *nicRace // 首选之外最先拿到应答的网卡
		chosen      *nicRace
		graceC      <-chan time.Time
	)
	for pending := len(conns); chosen == nil && pending > 0; {
		select {
		case r := <-results:
			pending--
			if r.err != nil {
				log.Printf("STUN: NIC %v: %v", r.conn.LocalAddr(), r.err)
			} else {
				log.Printf("STUN: NIC %v answered via %s in %v", r.conn.LocalAddr(), r.probes[0].Server,
					r.probes[0].Elapsed.Round(time.Millisecond))
			}
			if r.conn == primary {
				primaryDone = true
				if r.err == nil {
					chosen = &r
					continue
				}
				primaryErr = r.err
			} else if r.err == nil && alt == nil {
				alt = &r
				if !primaryDone {
					t := time.NewTimer(p.preferredGrace)
					defer t.Stop()
					graceC = t.C
				}
			}
			if primaryDone && alt != nil {
				chosen = alt
			}
		case <-graceC:
			chosen = alt
			primaryErr = fmt.Errorf("no answer within %v after another NIC answered", p.preferredGrace)
		}
	}

	keep := primary
	if chosen != nil {
		keep = chosen.conn
	}
	if keep != primary {
		p.tunnelMu.Lock()
		select {
		case <-p.stopChan:
			// Close 已经（或正在）回收原 localConn；不再换上新 socket，否则没人关它。
			keep = primary
			chosen = nil
			primaryErr = net.ErrClosed
		default:
			p.localConn = keep
			p.localAddr = keep.LocalAddr().(*net.UDPAddr)
		}
		p.tunnelMu.Unlock()
	}
	// 关掉没被选中的 socket；还在跑的竞速会因 ErrClosed 立即返回，结果落进带缓冲的 channel。
	for _, c := range conns {
		if c != keep {
			c.Close()
		}
	}
	if chosen == nil {
		return nil, primaryErr
	}
	if keep != primary {
		log.Printf("STUN: no answer on preferred NIC %v (%v); switching to %v where STUN answered",
			primary.LocalAddr(), primaryErr, p.localAddr)
	}
	return chosen.probes, nil
}

func (p *P2PManager) advertiseAddr() error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection")
	}

	advert := &proto.PeerAddrAdvertise{
		PublicAddr:  addrToString(p.publicAddr),
		PrivateAddr: addrToString(p.localAddr),
		NATType:     p.natType,
	}
	if p.publicAddr != nil {
		advert.STUNServer = p.stunUsed
		advert.STUNMs = p.stunElapsed.Milliseconds()
	}

	return p.relayConn.SendMessage(proto.MsgPeerAddrAdvertise, advert)
}

func addrToString(addr *net.UDPAddr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

func parseAddr(addrStr string) *net.UDPAddr {
	if addrStr == "" {
		return nil
	}
	addr, _ := net.ResolveUDPAddr("udp", addrStr)
	return addr
}

// HandlePeerAddrReady handles peer address ready message
func (p *P2PManager) HandlePeerAddrReady(msg *proto.PeerAddrReady) {
	log.Printf("Received peer address: public=%s, private=%s, same_network=%v, peer_nat=%s", msg.PeerPublicAddr, msg.PeerPrivateAddr, msg.SameNetwork, msg.PeerNATType)

	p.peerInfo = &PeerInfo{
		PublicAddr:  parseAddr(msg.PeerPublicAddr),
		PrivateAddr: parseAddr(msg.PeerPrivateAddr),
	}

	p.peerNATType = msg.PeerNATType
	p.punchPlan = planHolePunch(msg.SameNetwork, p.localAddr, p.peerInfo.PrivateAddr)
	if p.punchPlan.lanOnly {
		log.Printf("Same network detected, using LAN fast path")
	} else if p.punchPlan.privateInterval == lanPunchInterval {
		log.Printf("Peer private address in local /16, punching it fast alongside public hole punching")
	}

	// Start hole punching
	go p.startHolePunching()
}

const (
	// lanPunchInterval 私网地址大概率可达时的打洞间隔（LAN 快速通道）。
	lanPunchInterval = 50 * time.Millisecond
	// privatePunchInterval 私网地址可达性未知时的低频打洞间隔。
	privatePunchInterval = 200 * time.Millisecond
)

// punchPlan 打洞策略。
type punchPlan struct {
	lanOnly         bool          // 只打私网地址，跳过公网打洞、端口预测、生日攻击和 UDP 中继
	privateInterval time.Duration // 私网地址的打洞间隔
}

// planHolePunch 决定打洞策略。
//
// sameEgress 是 relay 下发的 same_network（两端公网出口 IP 相同，同一 NAT 后面），此时私网
// 地址几乎必然可达，只打私网、跳过公网那套（LAN 快速通道，fd0aa13）。
//
// 私网 IP 同 /16 只说明「可能」同一局域网：企业内网、跨地域办公室普遍复用 10.x 段，私网互不
// 可达（实测苏州 10.11.115.x 与合肥 10.11.24.x）。所以只把私网打洞提到 LAN 频率，公网打洞
// 照常并行——真同 LAN 时私网照样先通，误判时也不丢公网的机会。
func planHolePunch(sameEgress bool, local, peerPrivate *net.UDPAddr) punchPlan {
	if sameEgress {
		return punchPlan{lanOnly: true, privateInterval: lanPunchInterval}
	}
	if local != nil && peerPrivate != nil {
		l, r := local.IP.To4(), peerPrivate.IP.To4()
		if l != nil && r != nil && l[0] == r[0] && l[1] == r[1] {
			return punchPlan{privateInterval: lanPunchInterval}
		}
	}
	return punchPlan{privateInterval: privatePunchInterval}
}

// portPredictionRange 端口预测范围，用于对称型 NAT
const portPredictionRange = 10

func (p *P2PManager) startHolePunching() {
	if p.peerInfo == nil {
		return
	}

	plan := p.punchPlan
	// backgroundRetry 可能看到 peerInfo 却还没看到 punchPlan（零值）；间隔为 0 时
	// time.NewTicker 会 panic，退回低频私网打洞。
	if plan.privateInterval <= 0 {
		plan.privateInterval = privatePunchInterval
	}
	if plan.lanOnly {
		// LAN 快速通道：只打私网地址，高频率
		if p.peerInfo.PrivateAddr != nil {
			go func() {
				log.Printf("LAN hole punching private address: %v", p.peerInfo.PrivateAddr)
				p.sendTestPackets(p.peerInfo.PrivateAddr, plan.privateInterval)
			}()
		}
		return // 跳过公网打洞、端口预测、UDP 中继
	}

	// === 以下为非 LAN 场景，根据 NAT 类型选择策略 ===

	hasSymmetric := p.natType == NATSymmetric || p.peerNATType == NATSymmetric
	log.Printf("Hole punch strategy: local_nat=%s, peer_nat=%s, has_symmetric=%v", p.natType, p.peerNATType, hasSymmetric)

	// 端口预测范围：涉及对称型 NAT 时扩大到 ±100
	predRange := portPredictionRange // 默认 10
	if hasSymmetric {
		predRange = 100
	}

	// 公网地址：精确端口高频 + 端口预测
	if p.peerInfo.PublicAddr != nil {
		go func() {
			log.Printf("Hole punching public address: %v", p.peerInfo.PublicAddr)
			p.sendTestPackets(p.peerInfo.PublicAddr, 100*time.Millisecond)
		}()
		go func() {
			log.Printf("Port prediction around %v (±%d ports)", p.peerInfo.PublicAddr, predRange)
			p.sendPortPredictionPackets(p.peerInfo.PublicAddr, predRange, 300*time.Millisecond)
		}()
	}

	// 本端是对称型 NAT 时，启动生日攻击（延迟 1s，让常规打洞先尝试）
	if p.natType == NATSymmetric && p.peerInfo.PublicAddr != nil {
		go func() {
			select {
			case <-time.After(1 * time.Second):
			case <-p.stopChan:
				return
			}
			p.startBirthdayAttack(p.peerInfo.PublicAddr)
		}()
	}

	// 私网地址：持续打洞（私网同 /16 时用 LAN 频率，否则低频）
	if p.peerInfo.PrivateAddr != nil {
		go func() {
			log.Printf("Hole punching private address: %v", p.peerInfo.PrivateAddr)
			p.sendTestPackets(p.peerInfo.PrivateAddr, plan.privateInterval)
		}()
	}

	// UDP 中继：延迟 2s 启动，优先直连
	if p.stunAddr != nil {
		go func() {
			select {
			case <-time.After(2 * time.Second):
			case <-p.stopChan:
				return
			}
			log.Printf("Trying UDP relay via %v", p.stunAddr)
			p.sendRelayTestPackets(p.stunAddr, 200*time.Millisecond)
		}()
	}
}

// startBirthdayAttack 使用多个额外 UDP socket 进行生日攻击打洞
// 64 sockets × 5 轮 → 在对称型 NAT 下大幅提高碰撞概率
// 每个 socket 启动接收循环，收到对端回复时触发 P2P 连接
func (p *P2PManager) startBirthdayAttack(peerAddr *net.UDPAddr) {
	const (
		numSockets = 64
		numRounds  = 5
		roundDelay = 50 * time.Millisecond
		holdTime   = 3 * time.Second
	)

	log.Printf("Starting birthday attack with %d sockets towards %v", numSockets, peerAddr)

	// 确定绑定 IP
	// 与主 socket 绑同一块网卡（含 --bind-ip 与按 STUN 实测改绑的结果）
	listenIP := net.IPv4zero
	if bound, ok := p.localConn.LocalAddr().(*net.UDPAddr); ok && bound.IP != nil {
		listenIP = bound.IP
	}

	testPacket := p.newTestPacket()
	data, _ := json.Marshal(testPacket)

	// 创建额外 socket
	sockets := make([]*net.UDPConn, 0, numSockets)
	for i := 0; i < numSockets; i++ {
		select {
		case <-p.stopChan:
			goto cleanup
		default:
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: listenIP, Port: 0})
		if err != nil {
			log.Printf("Birthday attack: socket %d creation failed: %v", i, err)
			continue
		}
		sockets = append(sockets, conn)
	}

	log.Printf("Birthday attack: created %d sockets, sending %d rounds", len(sockets), numRounds)

	// 为每个 socket 启动接收循环，收到对端 test packet 时触发连接
	for _, conn := range sockets {
		go func(c *net.UDPConn) {
			buf := make([]byte, 1500)
			c.SetReadDeadline(time.Now().Add(holdTime + time.Duration(numRounds)*roundDelay))
			for {
				n, addr, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				var pkt proto.P2PTestPacket
				if json.Unmarshal(buf[:n], &pkt) == nil && p.acceptPunch(&pkt, addr) {
					log.Printf("Birthday attack: received reply on extra socket from %v", addr)
					p.onP2PConnected(addr)
					return
				}
			}
		}(conn)
	}

	// 多轮发送
	for round := 0; round < numRounds; round++ {
		select {
		case <-p.stopChan:
			goto cleanup
		default:
		}

		for _, conn := range sockets {
			conn.WriteToUDP(data, peerAddr)
		}

		if round < numRounds-1 {
			select {
			case <-time.After(roundDelay):
			case <-p.stopChan:
				goto cleanup
			}
		}
	}

	// 保持 socket 等待回复
	select {
	case <-time.After(holdTime):
	case <-p.stopChan:
	}

cleanup:
	for _, conn := range sockets {
		conn.Close()
	}
	log.Printf("Birthday attack: cleaned up %d sockets", len(sockets))
}

func (p *P2PManager) sendTestPackets(addr *net.UDPAddr, interval time.Duration) {
	testPacket := p.newTestPacket()

	data, _ := json.Marshal(testPacket)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.localConn.WriteToUDP(data, addr)
		}
	}
}

// sendRelayTestPackets 通过 STUN 服务器中继发送测试包
func (p *P2PManager) sendRelayTestPackets(relayAddr *net.UDPAddr, interval time.Duration) {
	testPacket := p.newTestPacket()
	testData, _ := json.Marshal(testPacket)

	header := makeRelayHeader(p.sessionID)
	data := make([]byte, len(header)+len(testData))
	copy(data, header)
	copy(data[len(header):], testData)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.localConn.WriteToUDP(data, relayAddr)
		}
	}
}

// sendPortPredictionPackets 向基准端口附近的端口发送打洞包，应对对称型 NAT
func (p *P2PManager) sendPortPredictionPackets(baseAddr *net.UDPAddr, portRange int, interval time.Duration) {
	testPacket := p.newTestPacket()
	data, _ := json.Marshal(testPacket)

	// 生成预测地址列表（排除精确端口，由 sendTestPackets 处理）
	var addrs []*net.UDPAddr
	for offset := -portRange; offset <= portRange; offset++ {
		if offset == 0 {
			continue
		}
		port := baseAddr.Port + offset
		if port > 0 && port <= 65535 {
			addrs = append(addrs, &net.UDPAddr{IP: baseAddr.IP, Port: port})
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			for _, addr := range addrs {
				p.localConn.WriteToUDP(data, addr)
			}
		}
	}
}

func (p *P2PManager) receiveLoop() {
	buf := make([]byte, 65536)
	for {
		select {
		case <-p.stopChan:
			return
		case <-p.recvStop:
			// 隧道已接管 localConn（可能由生日攻击的额外 socket 触发，此时本循环
			// 既没收到包也不知情）。必须退出：否则和隧道的 recvLoop 并发读同一个
			// UDP socket，内核随机派发数据报，落到这里的隧道包会被直接丢弃。
			return
		default:
		}

		// 1s 读超时是刻意的：让循环有机会回到顶部复查停止信号。
		p.localConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remoteAddr, err := p.localConn.ReadFromUDP(buf)
		if err != nil {
			// 只有超时才该重试。socket 已关时 ReadFromUDP 立即返回错误，continue
			// 会退化成没有任何休眠的忙循环吃满一个核——help 的 downgradeToRelay 就是
			// 这条路径：它 pc.Close()（连带关掉 localConn）却不调 mgr.Close()，
			// stopChan 一直开着，忙循环能持续到整个会话 teardown。
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}

		// Try to parse as test packet
		var testPacket proto.P2PTestPacket
		if err := json.Unmarshal(buf[:n], &testPacket); err == nil {
			if p.acceptPunch(&testPacket, remoteAddr) {
				log.Printf("Received P2P test packet from %v", remoteAddr)
				p.onP2PConnected(remoteAddr)
				return
			}
		}
	}
}

func (p *P2PManager) onP2PConnected(addr *net.UDPAddr) {
	p.connectedMu.Lock()
	if p.connected {
		p.connectedMu.Unlock()
		return
	}
	p.connected = true
	p.connectedMu.Unlock()

	// 检测是否通过 UDP 中继连接
	isRelay := p.stunAddr != nil && addr.IP.Equal(p.stunAddr.IP) && addr.Port == p.stunAddr.Port

	if isRelay {
		log.Printf("P2P connection established via UDP relay through %v", addr)
	} else {
		log.Printf("P2P connection established with %v (direct)", addr)
	}

	// 回发确认包，帮助对方也检测到连接
	confirmPacket := p.newTestPacket()
	confirmData, _ := json.Marshal(confirmPacket)
	if isRelay {
		header := makeRelayHeader(p.sessionID)
		wrapped := make([]byte, len(header)+len(confirmData))
		copy(wrapped, header)
		copy(wrapped[len(header):], confirmData)
		for i := 0; i < 5; i++ {
			p.localConn.WriteToUDP(wrapped, addr)
			time.Sleep(20 * time.Millisecond)
		}
	} else {
		for i := 0; i < 5; i++ {
			p.localConn.WriteToUDP(confirmData, addr)
			time.Sleep(20 * time.Millisecond)
		}
	}

	// 后台重试模式：仅记录日志，不创建 tunnel，不通知 relay
	if p.backgroundMode.Load() {
		log.Printf("P2P background retry succeeded! Next connection will use P2P directly.")
		// 这条路径没有隧道接管 localConn，socket 的回收全靠 Close 里的
		// !tunnelCreated 分支——connected 已经置位了，别再指望它。
		return
	}

	// Notify relay that we're switching to P2P
	if p.relayConn != nil {
		p.relayConn.SendMessage(proto.MsgP2PConnected, map[string]string{
			"session_id": p.sessionID,
		})
	}

	// 建隧道前先停 receiveLoop：隧道的 recvLoop 马上要读同一个 localConn，两个
	// goroutine 并发读一个 UDP socket 会互相偷包。生日攻击路径下本调用来自额外
	// socket 的 goroutine，receiveLoop 不会自己发现"已经连上了"。
	//
	// 只发信号不等它退出：本函数也可能就是从 receiveLoop 里调进来的（见 receiveLoop
	// 收到测试包那一支），等自己退出会死锁。因此仍有 ≤1s（一个读超时周期）的重叠窗口，
	// 期间可能被偷走个别数据报——UDP 本就允许丢包，隧道有 keepalive 兜底。这比改之前
	// 好得多：那时 receiveLoop 压根不停，整场会话都在跟隧道抢包。
	p.stopReceiveLoop()

	var tunnel *UDPTunnel
	if isRelay {
		tunnel = NewUDPRelayTunnel(p.localConn, addr, p.sessionID)
	} else {
		tunnel = NewUDPTunnel(p.localConn, addr)
	}
	// 投递与 Close 的回收互斥，且用非阻塞发送：resultCh 满（消费者早已走别的路）
	// 或 manager 已经 Close 时都不能把隧道就这么撂在那儿——localConn 与它的 3 个
	// goroutine 会一直悬到 60s peerTimeout，对端仍在发 keepalive 时连这个都不生效。
	p.tunnelMu.Lock()
	delivered := false
	select {
	case <-p.stopChan:
	default:
		select {
		case p.resultCh <- P2PResult{Tunnel: tunnel}:
			p.tunnelCreated = true
			delivered = true
		default:
		}
	}
	p.tunnelMu.Unlock()
	if !delivered {
		tunnel.Close()
	}
}

func (p *P2PManager) p2pTimeout() {
	timeout := 8 * time.Second
	if p.mode == P2PModeRequired {
		timeout = 30 * time.Second
	}

	select {
	case <-p.stopChan:
		return
	case <-time.After(timeout):
		p.connectedMu.RLock()
		connected := p.connected
		p.connectedMu.RUnlock()

		if !connected {
			log.Printf("P2P connection timed out")
			if p.mode == P2PModeAuto {
				log.Printf("Falling back to relay mode, will retry P2P in background")
				// 先置位再推回退结果。反过来的话，从「消费者拿到回退结果并走 abandon」
				// 到「backgroundMode 变为 true」之间有个窗口，此时若打洞包刚好到达，
				// onP2PConnected 会读到 false 而照常建隧道并塞进 resultCh，但已经没有
				// 人再读第二条结果了——隧道成为孤儿。
				p.backgroundMode.Store(true)
				p.resultCh <- P2PResult{}
				go p.backgroundRetry()
				return
			} else if p.mode == P2PModeRequired {
				p.resultCh <- P2PResult{Err: fmt.Errorf("P2P 连接超时")}
			}
		}
	}
}

// backgroundRetry 在 relay 模式下后台重试 P2P 打洞
// 成功后仅记录日志，不创建 tunnel（避免资源泄露）
func (p *P2PManager) backgroundRetry() {
	p.backgroundMode.Store(true) // p2pTimeout 已置位，这里只是让本函数可被独立调用
	retryIntervals := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}

	for i, interval := range retryIntervals {
		select {
		case <-time.After(interval):
		case <-p.stopChan:
			return
		}

		p.connectedMu.RLock()
		connected := p.connected
		p.connectedMu.RUnlock()
		if connected {
			return
		}

		if p.peerInfo == nil {
			log.Printf("P2P background retry %d/%d: no peer info, skipping", i+1, len(retryIntervals))
			continue
		}

		log.Printf("P2P background retry %d/%d", i+1, len(retryIntervals))
		go p.startHolePunching()
	}

	log.Printf("P2P background retries exhausted")
}

// IsConnected returns whether P2P is connected
func (p *P2PManager) IsConnected() bool {
	p.connectedMu.RLock()
	defer p.connectedMu.RUnlock()
	return p.connected
}

// Close closes the P2P manager
func (p *P2PManager) Close() {
	p.closeOnce.Do(func() {
		close(p.stopChan)
		p.stopReceiveLoop()

		p.tunnelMu.Lock()
		defer p.tunnelMu.Unlock()

		// 抽干 resultCh，关掉投递了却没人接手的隧道。
		//
		// 上层（share.go / help_bootstrap.go）的 `select { case <-resultCh; case
		// <-sessionDone }` 在会话已拆时会直接走 sessionDone 分支 → mgr.Close() → return，
		// 此时 onP2PConnected 可能已经把隧道塞进了这个 buffer=2 的 channel。没人取，
		// 也就没人 Close：UDP socket 加隧道的 3 个 goroutine 一起悬空，自愈只能等 60s
		// peerTimeout，而对端若仍在发 keepalive 就连这个都指望不上。
		for drained := false; !drained; {
			select {
			case res := <-p.resultCh:
				if res.Tunnel != nil {
					res.Tunnel.Close() // 幂等；内部会关掉 localConn
				}
			default:
				drained = true
			}
		}

		// 隧道已交接给消费者时由消费者负责关 localConn（UDPTunnel.Close 就是关的它），
		// 这里再关一次会打断仍在使用中的隧道。反之——从未建过隧道——localConn 就是无主的，
		// 必须由这里回收。注意不能用 connected 判断：backgroundRetry 成功时 connected
		// 已置位却刻意不建隧道（见 onP2PConnected），旧代码那条 `!connected` 分支正好
		// 漏掉这一种，socket 一直悬到进程退出。
		if !p.tunnelCreated && p.localConn != nil {
			p.localConn.Close()
		}
	})
}

// getOutboundIP gets the preferred outbound IP of this machine
// by dialing a public address (doesn't actually send packets)
func getOutboundIP() net.IP {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return nil
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP
}

func randomString(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
		b[i] = charset[num.Int64()]
	}
	return string(b)
}
