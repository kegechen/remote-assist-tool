package p2p

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// P2PMode defines the P2P mode
type P2PMode int

const (
	P2PModeDisabled P2PMode = iota
	P2PModeAuto     // Try P2P first, fall back to relay
	P2PModeRequired // Only use P2P, fail if not possible
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
	mode         P2PMode
	stunServer   string
	stunAddr     *net.UDPAddr // resolved STUN server address for UDP relay
	localConn    *net.UDPConn
	localAddr    *net.UDPAddr
	publicAddr   *net.UDPAddr
	peerInfo     *PeerInfo
	sessionID    string
	isShare      bool
	connected    bool
	connectedMu  sync.RWMutex
	relayConn    RelayConn
	resultCh     chan P2PResult
	stopChan     chan struct{}
	closeOnce    sync.Once
	sameNetwork    bool   // 是否检测到同网络
	bindIP         string // 用户手动指定的绑定 IP
	natType        string // 本端 NAT 类型
	peerNATType    string // 对端 NAT 类型
	backgroundMode bool   // 后台重试模式（仅记录日志，不创建 tunnel）
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
		mode:       mode,
		stunServer: stunServer,
		bindIP:     bindIP,
		stopChan:   make(chan struct{}),
	}
}

// SetRelayConn sets the relay connection for fallback and signaling
func (p *P2PManager) SetRelayConn(conn RelayConn) {
	p.relayConn = conn
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
	if p.bindIP != "" {
		if ip := net.ParseIP(p.bindIP); ip != nil {
			listenIP = ip
			log.Printf("Using user-specified bind IP: %v", ip)
		} else {
			log.Printf("Invalid bind IP %q, falling back to auto-detect", p.bindIP)
		}
	}
	if listenIP.Equal(net.IPv4zero) {
		if physIP := DetectPhysicalIP(); physIP != nil {
			listenIP = physIP
			log.Printf("Proxy bypass: binding to physical NIC %v", physIP)
		}
	}

	// Create local UDP socket
	var err error
	p.localConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: listenIP, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
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

	// Discover public address via STUN
	if p.stunServer != "" {
		p.publicAddr, err = DiscoverPublicAddrVia(p.localConn, p.stunServer)
		if err != nil {
			log.Printf("STUN discovery failed: %v, trying external STUN servers", err)
			// 回退到公共 STUN 服务器
			for _, extSTUN := range []string{"stun.l.google.com:19302", "stun1.l.google.com:19302", "stun.cloudflare.com:3478"} {
				p.publicAddr, err = DiscoverPublicAddrVia(p.localConn, extSTUN)
				if err == nil {
					log.Printf("Discovered public address via external STUN (%s): %v", extSTUN, p.publicAddr)
					break
				}
				log.Printf("External STUN %s failed: %v", extSTUN, err)
			}
			if p.publicAddr == nil {
				log.Printf("All STUN servers failed, will try without public address")
			}
		} else {
			log.Printf("Discovered public address: %v", p.publicAddr)
		}
	}

	// NAT 类型检测
	if p.publicAddr != nil {
		stunServers := []string{"stun.l.google.com:19302", "stun1.l.google.com:19302", "stun.cloudflare.com:3478"}
		if p.stunServer != "" {
			stunServers = append([]string{p.stunServer}, stunServers...)
		}
		// 去重，确保至少有 2 个不同 IP 的服务器
		p.natType = DetectNATType(p.localAddr.IP, p.publicAddr, stunServers)
		log.Printf("NAT type detected: %s", p.natType)
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

func (p *P2PManager) advertiseAddr() error {
	if p.relayConn == nil {
		return fmt.Errorf("no relay connection")
	}

	advert := &proto.PeerAddrAdvertise{
		PublicAddr:  addrToString(p.publicAddr),
		PrivateAddr: addrToString(p.localAddr),
		NATType:     p.natType,
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
	p.sameNetwork = msg.SameNetwork
	// 本地二次确认：本机 IP 和对端私网 IP 在同一 /16
	if !p.sameNetwork && p.localAddr != nil && p.peerInfo.PrivateAddr != nil {
		l := p.localAddr.IP.To4()
		r := p.peerInfo.PrivateAddr.IP.To4()
		if l != nil && r != nil && l[0] == r[0] && l[1] == r[1] {
			p.sameNetwork = true
		}
	}

	if p.sameNetwork {
		log.Printf("Same network detected, using LAN fast path")
	}

	// Start hole punching
	go p.startHolePunching()
}

// portPredictionRange 端口预测范围，用于对称型 NAT
const portPredictionRange = 10

func (p *P2PManager) startHolePunching() {
	if p.peerInfo == nil {
		return
	}

	if p.sameNetwork {
		// LAN 快速通道：只打私网地址，高频率
		if p.peerInfo.PrivateAddr != nil {
			go func() {
				log.Printf("LAN hole punching private address: %v", p.peerInfo.PrivateAddr)
				p.sendTestPackets(p.peerInfo.PrivateAddr, 50*time.Millisecond)
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

	// 私网地址：低频率持续打洞
	if p.peerInfo.PrivateAddr != nil {
		go func() {
			log.Printf("Hole punching private address: %v", p.peerInfo.PrivateAddr)
			p.sendTestPackets(p.peerInfo.PrivateAddr, 200*time.Millisecond)
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
	listenIP := net.IPv4zero
	if physIP := DetectPhysicalIP(); physIP != nil {
		listenIP = physIP
	}

	testPacket := &proto.P2PTestPacket{
		SessionID: p.sessionID,
		Random:    randomString(16),
	}
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
				if json.Unmarshal(buf[:n], &pkt) == nil && pkt.SessionID == p.sessionID {
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
	testPacket := &proto.P2PTestPacket{
		SessionID: p.sessionID,
		Random:    randomString(16),
	}

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
	testPacket := &proto.P2PTestPacket{
		SessionID: p.sessionID,
		Random:    randomString(16),
	}
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
	testPacket := &proto.P2PTestPacket{
		SessionID: p.sessionID,
		Random:    randomString(16),
	}
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
		default:
		}

		p.localConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remoteAddr, err := p.localConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		// Try to parse as test packet
		var testPacket proto.P2PTestPacket
		if err := json.Unmarshal(buf[:n], &testPacket); err == nil {
			if testPacket.SessionID == p.sessionID {
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
	confirmPacket := &proto.P2PTestPacket{
		SessionID: p.sessionID,
		Random:    randomString(16),
	}
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
	if p.backgroundMode {
		log.Printf("P2P background retry succeeded! Next connection will use P2P directly.")
		return
	}

	// Notify relay that we're switching to P2P
	if p.relayConn != nil {
		p.relayConn.SendMessage(proto.MsgP2PConnected, map[string]string{
			"session_id": p.sessionID,
		})
	}

	var tunnel *UDPTunnel
	if isRelay {
		tunnel = NewUDPRelayTunnel(p.localConn, addr, p.sessionID)
	} else {
		tunnel = NewUDPTunnel(p.localConn, addr)
	}
	p.resultCh <- P2PResult{Tunnel: tunnel}
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
	p.backgroundMode = true
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
		p.connectedMu.RLock()
		connected := p.connected
		p.connectedMu.RUnlock()
		if !connected && p.localConn != nil {
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
