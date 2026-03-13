package p2p

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
	"time"
)

// Relay constants
const (
	relayMarker     = 0xFF
	relaySessionTTL = 5 * time.Minute
)

// makeRelayHeader creates the binary header for a relay packet
func makeRelayHeader(sessionID string) []byte {
	header := make([]byte, 2+len(sessionID))
	header[0] = relayMarker
	header[1] = byte(len(sessionID))
	copy(header[2:], sessionID)
	return header
}

// parseRelayHeader parses a relay packet, returns sessionID, payload
func parseRelayHeader(data []byte) (sessionID string, payload []byte, ok bool) {
	if len(data) < 2 || data[0] != relayMarker {
		return "", nil, false
	}
	sidLen := int(data[1])
	if len(data) < 2+sidLen {
		return "", nil, false
	}
	return string(data[2 : 2+sidLen]), data[2+sidLen:], true
}

// relaySession tracks two peers for UDP relay forwarding
type relaySession struct {
	peers    [2]*net.UDPAddr
	count    int
	lastSeen time.Time
}

// STUNServer is a simple STUN server with UDP relay capability
type STUNServer struct {
	conn     *net.UDPConn
	closed   bool
	closeMu  sync.Mutex
	// UDP relay sessions (auto-discovered from relay packets)
	relaySessions   map[string]*relaySession
	relayMu         sync.Mutex
}

// NewSTUNServer creates a new STUN server
func NewSTUNServer(addr string) (*STUNServer, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}

	s := &STUNServer{
		conn:          conn,
		relaySessions: make(map[string]*relaySession),
	}

	go s.serve()
	go s.cleanupRelaySessions()
	return s, nil
}

func (s *STUNServer) serve() {
	buf := make([]byte, 1500)
	for {
		s.closeMu.Lock()
		if s.closed {
			s.closeMu.Unlock()
			return
		}
		s.closeMu.Unlock()

		n, remoteAddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		// Route: relay packets (0xFF prefix) vs STUN packets
		if n > 0 && buf[0] == relayMarker {
			s.handleRelayPacket(buf[:n], remoteAddr)
		} else {
			go s.handlePacket(buf[:n], remoteAddr)
		}
	}
}

func (s *STUNServer) handlePacket(data []byte, remoteAddr *net.UDPAddr) {
	msg, err := UnpackSTUN(data)
	if err != nil {
		log.Printf("STUN: invalid packet from %v: %v", remoteAddr, err)
		return
	}

	if msg.Type != STUNBindingRequest {
		log.Printf("STUN: non-binding request (type=0x%04x) from %v", msg.Type, remoteAddr)
		return
	}

	log.Printf("STUN: binding request from %v", remoteAddr)

	// Build response
	resp := &STUNMessage{
		Type:  STUNBindingResponse,
		Magic: msg.Magic,
		TID:   msg.TID,
	}

	// Add XOR-MAPPED-ADDRESS attribute
	resp.AddXorMappedAddress(remoteAddr, msg.Magic, msg.TID)

	// Add SOFTWARE attribute
	resp.AddSoftwareAttribute("remote-assist-stun/1.0")

	// Send response
	respBytes := resp.Pack()
	_, err = s.conn.WriteToUDP(respBytes, remoteAddr)
	if err != nil {
		log.Printf("STUN: failed to send response to %v: %v", remoteAddr, err)
	}
}

// AddXorMappedAddress adds the XOR-MAPPED-ADDRESS attribute
func (m *STUNMessage) AddXorMappedAddress(addr *net.UDPAddr, magic uint32, tid [12]byte) {
	family := uint16(0x01) // IPv4
	if addr.IP.To4() == nil {
		family = 0x02 // IPv6
	}

	// Build value
	value := make([]byte, 8)
	binary.BigEndian.PutUint16(value[0:2], family)

	// XOR port with magic
	xorPort := uint16(addr.Port) ^ uint16(magic>>16)
	binary.BigEndian.PutUint16(value[2:4], xorPort)

	if family == 0x01 {
		// IPv4: XOR with magic
		ip := binary.BigEndian.Uint32(addr.IP.To4())
		xorIP := ip ^ magic
		binary.BigEndian.PutUint32(value[4:8], xorIP)
	} else {
		// IPv6: XOR with magic + TID
		value = make([]byte, 20)
		binary.BigEndian.PutUint16(value[0:2], family)
		binary.BigEndian.PutUint16(value[2:4], xorPort)
		magicBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(magicBytes, magic)
		ip6 := addr.IP.To16()
		for i := 0; i < 16; i++ {
			if i < 4 {
				value[4+i] = ip6[i] ^ magicBytes[i]
			} else {
				value[4+i] = ip6[i] ^ tid[i-4]
			}
		}
	}

	m.Attrs = append(m.Attrs, STUNAttribute{
		Type:   STUNAttrXorMappedAddr,
		Length: uint16(len(value)),
		Value:  value,
	})
}

// Close closes the STUN server
func (s *STUNServer) Close() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.closed {
		return
	}
	s.closed = true
	if s.conn != nil {
		s.conn.Close()
	}
	log.Println("STUN server stopped")
}

// LocalAddr returns the local address the server is listening on
func (s *STUNServer) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

// handleRelayPacket handles UDP relay packets (0xFF prefix)
// Auto-discovers peers: first packet from each peer registers it,
// subsequent packets are forwarded to the other peer.
func (s *STUNServer) handleRelayPacket(data []byte, fromAddr *net.UDPAddr) {
	sessionID, payload, ok := parseRelayHeader(data)
	if !ok || len(payload) == 0 {
		return
	}

	s.relayMu.Lock()
	session, exists := s.relaySessions[sessionID]
	if !exists {
		session = &relaySession{lastSeen: time.Now()}
		s.relaySessions[sessionID] = session
	}
	session.lastSeen = time.Now()

	// Find or register this peer
	peerIdx := -1
	for i := 0; i < session.count; i++ {
		if session.peers[i].IP.Equal(fromAddr.IP) && session.peers[i].Port == fromAddr.Port {
			peerIdx = i
			break
		}
	}
	if peerIdx == -1 {
		if session.count >= 2 {
			// Try matching by IP only (port may have changed)
			for i := 0; i < session.count; i++ {
				if session.peers[i].IP.Equal(fromAddr.IP) {
					peerIdx = i
					session.peers[i] = fromAddr // update port
					break
				}
			}
			if peerIdx == -1 {
				s.relayMu.Unlock()
				return // session full, unknown peer
			}
		} else {
			peerIdx = session.count
			session.peers[peerIdx] = fromAddr
			session.count++
			log.Printf("UDP relay: peer %d registered for session %.30s...: %v", peerIdx+1, sessionID, fromAddr)
		}
	}

	// Forward to other peer
	otherIdx := 1 - peerIdx
	var targetAddr *net.UDPAddr
	if otherIdx < session.count {
		targetAddr = session.peers[otherIdx]
	}
	s.relayMu.Unlock()

	if targetAddr != nil {
		s.conn.WriteToUDP(payload, targetAddr)
	}
}

// cleanupRelaySessions removes stale relay sessions periodically
func (s *STUNServer) cleanupRelaySessions() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		s.closeMu.Lock()
		if s.closed {
			s.closeMu.Unlock()
			return
		}
		s.closeMu.Unlock()

		<-ticker.C

		s.relayMu.Lock()
		now := time.Now()
		for id, session := range s.relaySessions {
			if now.Sub(session.lastSeen) > relaySessionTTL {
				delete(s.relaySessions, id)
			}
		}
		s.relayMu.Unlock()
	}
}
