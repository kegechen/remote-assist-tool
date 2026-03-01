package p2p

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// STUN message types
const (
	STUNBindingRequest  = 0x0001
	STUNBindingResponse = 0x0101
	STUNBindingError    = 0x0111
)

// STUN attribute types
const (
	STUNAttrMappedAddress = 0x0001
	STUNAttrXorMappedAddr = 0x0020
	STUNAttrSoftware      = 0x8022
)

const (
	stunMagicCookie = 0x2112A442
	stunHeaderSize  = 20
)

var (
	ErrSTUNInvalidMessage = errors.New("invalid STUN message")
	ErrSTUNTimeout        = errors.New("STUN timeout")
)

// STUNMessage represents a STUN message
type STUNMessage struct {
	Type     uint16
	Length   uint16
	Magic    uint32
	TID      [12]byte
	Attrs    []STUNAttribute
}

// STUNAttribute represents a STUN attribute
type STUNAttribute struct {
	Type   uint16
	Length uint16
	Value  []byte
}

// MappedAddress represents a STUN mapped address
type MappedAddress struct {
	Family uint16
	Port   uint16
	IP     net.IP
}

// NewBindingRequest creates a new STUN binding request
func NewBindingRequest() *STUNMessage {
	msg := &STUNMessage{
		Type:  STUNBindingRequest,
		Magic: stunMagicCookie,
	}
	// Generate random transaction ID
	_, _ = rand.Read(msg.TID[:])
	return msg
}

// Pack packs the STUN message into bytes
func (m *STUNMessage) Pack() []byte {
	// Calculate total length
	totalLen := stunHeaderSize
	for _, attr := range m.Attrs {
		totalLen += 4 + int(attr.Length)
		// Padding to 4-byte boundary
		if pad := int((4 - (attr.Length % 4)) % 4); pad > 0 {
			totalLen += pad
		}
	}

	buf := make([]byte, totalLen)
	binary.BigEndian.PutUint16(buf[0:2], m.Type)
	binary.BigEndian.PutUint16(buf[2:4], uint16(totalLen-stunHeaderSize))
	binary.BigEndian.PutUint32(buf[4:8], m.Magic)
	copy(buf[8:20], m.TID[:])

	offset := stunHeaderSize
	for _, attr := range m.Attrs {
		binary.BigEndian.PutUint16(buf[offset:offset+2], attr.Type)
		binary.BigEndian.PutUint16(buf[offset+2:offset+4], attr.Length)
		copy(buf[offset+4:offset+4+int(attr.Length)], attr.Value)
		offset += 4 + int(attr.Length)
		// Skip padding
		if pad := int((4 - (attr.Length % 4)) % 4); pad > 0 {
			offset += pad
		}
	}

	return buf
}

// UnpackSTUN unpacks bytes into a STUN message
func UnpackSTUN(data []byte) (*STUNMessage, error) {
	if len(data) < stunHeaderSize {
		return nil, ErrSTUNInvalidMessage
	}

	msg := &STUNMessage{
		Type:  binary.BigEndian.Uint16(data[0:2]),
		Length: binary.BigEndian.Uint16(data[2:4]),
		Magic: binary.BigEndian.Uint32(data[4:8]),
	}
	copy(msg.TID[:], data[8:20])

	if msg.Magic != stunMagicCookie {
		return nil, ErrSTUNInvalidMessage
	}

	expectedLen := stunHeaderSize + int(msg.Length)
	if len(data) < expectedLen {
		return nil, ErrSTUNInvalidMessage
	}

	// Parse attributes
	offset := stunHeaderSize
	for offset < expectedLen {
		if offset+4 > expectedLen {
			break
		}
		attrType := binary.BigEndian.Uint16(data[offset : offset+2])
		attrLen := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		attrEnd := offset + 4 + int(attrLen)
		if attrEnd > expectedLen {
			break
		}
		attr := STUNAttribute{
			Type:   attrType,
			Length: attrLen,
			Value:  make([]byte, attrLen),
		}
		copy(attr.Value, data[offset+4:attrEnd])
		msg.Attrs = append(msg.Attrs, attr)
		// Skip padding
		offset = attrEnd
		if pad := int((4 - (attrLen % 4)) % 4); pad > 0 {
			offset += pad
		}
	}

	return msg, nil
}

// GetMappedAddress extracts the mapped address from STUN response
func (m *STUNMessage) GetMappedAddress() (*MappedAddress, error) {
	for _, attr := range m.Attrs {
		if attr.Type == STUNAttrXorMappedAddr || attr.Type == STUNAttrMappedAddress {
			return parseMappedAddress(attr.Value, m.Magic, m.TID, attr.Type == STUNAttrXorMappedAddr)
		}
	}
	return nil, errors.New("no mapped address found")
}

func parseMappedAddress(data []byte, magic uint32, tid [12]byte, xor bool) (*MappedAddress, error) {
	if len(data) < 8 {
		return nil, errors.New("invalid address data")
	}

	addr := &MappedAddress{
		Family: binary.BigEndian.Uint16(data[0:2]),
		Port:   binary.BigEndian.Uint16(data[2:4]),
	}

	if xor {
		addr.Port ^= uint16(magic >> 16)
	}

	if addr.Family == 0x01 {
		// IPv4
		ip := binary.BigEndian.Uint32(data[4:8])
		if xor {
			ip ^= magic
		}
		addr.IP = make(net.IP, 4)
		binary.BigEndian.PutUint32(addr.IP, ip)
	} else if addr.Family == 0x02 {
		// IPv6 (not fully implemented)
		addr.IP = make(net.IP, 16)
		copy(addr.IP, data[4:20])
		if xor {
			// XOR with magic + TID
			magicBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(magicBytes, magic)
			for i := 0; i < 16; i++ {
				if i < 4 {
					addr.IP[i] ^= magicBytes[i]
				} else {
					addr.IP[i] ^= tid[i-4]
				}
			}
		}
	}

	return addr, nil
}

// AddSoftwareAttribute adds a software attribute to the message
func (m *STUNMessage) AddSoftwareAttribute(name string) {
	m.Attrs = append(m.Attrs, STUNAttribute{
		Type:   STUNAttrSoftware,
		Length: uint16(len(name)),
		Value:  []byte(name),
	})
}

// DiscoverPublicAddr discovers the public address using a STUN server
func DiscoverPublicAddr(stunServer string) (*net.UDPAddr, error) {
	// Resolve STUN server address
	serverAddr, err := net.ResolveUDPAddr("udp", stunServer)
	if err != nil {
		return nil, fmt.Errorf("resolve STUN server: %w", err)
	}

	// Create UDP socket
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
	defer conn.Close()

	// Send binding request
	req := NewBindingRequest()
	req.AddSoftwareAttribute("remote-assist-tool/1.0")
	reqBytes := req.Pack()

	_, err = conn.WriteToUDP(reqBytes, serverAddr)
	if err != nil {
		return nil, fmt.Errorf("send STUN request: %w", err)
	}

	// Wait for response
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, ErrSTUNTimeout
	}

	// Parse response
	resp, err := UnpackSTUN(buf[:n])
	if err != nil {
		return nil, fmt.Errorf("parse STUN response: %w", err)
	}

	if resp.Type != STUNBindingResponse {
		return nil, errors.New("not a binding response")
	}

	// Get mapped address
	mappedAddr, err := resp.GetMappedAddress()
	if err != nil {
		return nil, fmt.Errorf("get mapped address: %w", err)
	}

	return &net.UDPAddr{
		IP:   mappedAddr.IP,
		Port: int(mappedAddr.Port),
	}, nil
}
