package p2p

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
)

// STUNServer is a simple STUN server implementation
type STUNServer struct {
	conn     *net.UDPConn
	closed   bool
	closeMu  sync.Mutex
}

// NewSTUNServer creates a new STUN server
func NewSTUNServer(addr string) (*STUNServer, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	s := &STUNServer{
		conn: conn,
	}

	go s.serve()
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

		go s.handlePacket(buf[:n], remoteAddr)
	}
}

func (s *STUNServer) handlePacket(data []byte, remoteAddr *net.UDPAddr) {
	msg, err := UnpackSTUN(data)
	if err != nil {
		return
	}

	if msg.Type != STUNBindingRequest {
		return
	}

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
	_, _ = s.conn.WriteToUDP(respBytes, remoteAddr)
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
