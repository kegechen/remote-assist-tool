package p2p

import (
	"encoding/json"
	"fmt"
	"log"
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

// PeerInfo represents a peer's network information
type PeerInfo struct {
	PublicAddr  *net.UDPAddr
	PrivateAddr *net.UDPAddr
}

// P2PManager manages P2P connection attempts
type P2PManager struct {
	mode         P2PMode
	stunServer   string
	localConn    *net.UDPConn
	localAddr    *net.UDPAddr
	publicAddr   *net.UDPAddr
	peerInfo     *PeerInfo
	sessionID    string
	isShare      bool
	connected    bool
	connectedMu  sync.RWMutex
	relayConn    RelayConn // Interface for relay fallback
	onP2PReady   func(*net.UDPConn)
	onRelayReady func()
	stopChan     chan struct{}
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

// SetOnP2PReady sets the callback when P2P is ready
func (p *P2PManager) SetOnP2PReady(fn func(*net.UDPConn)) {
	p.onP2PReady = fn
}

// SetOnRelayReady sets the callback when falling back to relay
func (p *P2PManager) SetOnRelayReady(fn func()) {
	p.onRelayReady = fn
}

// Start starts the P2P manager
func (p *P2PManager) Start(sessionID string, isShare bool) error {
	p.sessionID = sessionID
	p.isShare = isShare

	if p.mode == P2PModeDisabled {
		if p.onRelayReady != nil {
			p.onRelayReady()
		}
		return nil
	}

	// Create local UDP socket
	var err error
	p.localConn, err = net.ListenUDP("udp", nil)
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}
	p.localAddr = p.localConn.LocalAddr().(*net.UDPAddr)

	// Discover public address via STUN
	if p.stunServer != "" {
		p.publicAddr, err = DiscoverPublicAddr(p.stunServer)
		if err != nil {
			log.Printf("STUN discovery failed: %v, will try without", err)
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

	return nil
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
	log.Printf("Received peer address: public=%s, private=%s", msg.PeerPublicAddr, msg.PeerPrivateAddr)

	p.peerInfo = &PeerInfo{
		PublicAddr:  parseAddr(msg.PeerPublicAddr),
		PrivateAddr: parseAddr(msg.PeerPrivateAddr),
	}

	// Start hole punching
	go p.startHolePunching()
}

func (p *P2PManager) startHolePunching() {
	if p.peerInfo == nil {
		return
	}

	// Try private address first
	if p.peerInfo.PrivateAddr != nil {
		log.Printf("Trying to connect via private address: %v", p.peerInfo.PrivateAddr)
		p.sendTestPackets(p.peerInfo.PrivateAddr, 10, 100*time.Millisecond)
	}

	// Try public address
	if p.peerInfo.PublicAddr != nil {
		log.Printf("Trying to connect via public address: %v", p.peerInfo.PublicAddr)
		p.sendTestPackets(p.peerInfo.PublicAddr, 20, 50*time.Millisecond)
	}
}

func (p *P2PManager) sendTestPackets(addr *net.UDPAddr, count int, interval time.Duration) {
	testPacket := &proto.P2PTestPacket{
		SessionID: p.sessionID,
		Random:    randomString(16),
	}

	data, _ := json.Marshal(testPacket)

	for i := 0; i < count; i++ {
		select {
		case <-p.stopChan:
			return
		default:
			_, _ = p.localConn.WriteToUDP(data, addr)
			time.Sleep(interval)
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

	log.Printf("P2P connection established with %v", addr)

	// Notify relay that we're switching to P2P
	if p.relayConn != nil {
		p.relayConn.SendMessage(proto.MsgP2PConnected, map[string]string{
			"session_id": p.sessionID,
		})
	}

	if p.onP2PReady != nil {
		p.onP2PReady(p.localConn)
	}
}

func (p *P2PManager) p2pTimeout() {
	timeout := 10 * time.Second
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
				if p.onRelayReady != nil {
					p.onRelayReady()
				}
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
	close(p.stopChan)
	if p.localConn != nil {
		p.localConn.Close()
	}
}

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return string(b)
}
