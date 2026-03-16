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
	sameNetwork  bool // 是否检测到同网络
}

// RelayConn is the interface for relay fallback communication
type RelayConn interface {
	SendMessage(msgType proto.MessageType, payload interface{}) error
	ReadMessage() (*proto.Message, error)
	Close()
}

// NewP2PManager creates a new P2P manager
func NewP2PManager(mode P2PMode, stunServer string) *P2PManager {
	return &P2PManager{
		mode:       mode,
		stunServer: stunServer,
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

	// Create local UDP socket bound to IPv4
	var err error
	p.localConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
	// Get real local IP instead of wildcard
	p.localAddr = p.localConn.LocalAddr().(*net.UDPAddr)
	if outboundIP := getOutboundIP(); outboundIP != nil {
		log.Printf("Local outbound IP: %v", outboundIP)
		p.localAddr = &net.UDPAddr{IP: outboundIP, Port: p.localAddr.Port}
	} else {
		log.Printf("getOutboundIP failed, advertising wildcard private address")
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
	log.Printf("Received peer address: public=%s, private=%s, same_network=%v", msg.PeerPublicAddr, msg.PeerPrivateAddr, msg.SameNetwork)

	p.peerInfo = &PeerInfo{
		PublicAddr:  parseAddr(msg.PeerPublicAddr),
		PrivateAddr: parseAddr(msg.PeerPrivateAddr),
	}

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

	// === 以下为原有逻辑，非 LAN 场景 ===

	// 公网地址：精确端口高频 + 端口预测应对对称型 NAT
	if p.peerInfo.PublicAddr != nil {
		go func() {
			log.Printf("Hole punching public address: %v", p.peerInfo.PublicAddr)
			p.sendTestPackets(p.peerInfo.PublicAddr, 100*time.Millisecond)
		}()
		go func() {
			log.Printf("Port prediction around %v (±%d ports)", p.peerInfo.PublicAddr, portPredictionRange)
			p.sendPortPredictionPackets(p.peerInfo.PublicAddr, portPredictionRange, 300*time.Millisecond)
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
				log.Printf("Falling back to relay mode")
				p.resultCh <- P2PResult{}
			} else if p.mode == P2PModeRequired {
				p.resultCh <- P2PResult{Err: fmt.Errorf("P2P 连接超时")}
			}
		}
	}
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
