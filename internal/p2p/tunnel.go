package p2p

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const (
	// Packet types
	pktTypeData = 0x01
	pktTypeAck  = 0x02

	maxPacketSize = 1400
)

// UDPTunnel represents a reliable UDP tunnel
type UDPTunnel struct {
	conn       *net.UDPConn
	remoteAddr *net.UDPAddr
	sendMutex  sync.Mutex
	recvMutex  sync.Mutex
	recvBuffer []byte
	recvOffset int
	closed     bool
	closeMutex sync.Mutex
}

// NewUDPTunnel creates a new UDP tunnel
func NewUDPTunnel(conn *net.UDPConn, remoteAddr *net.UDPAddr) *UDPTunnel {
	return &UDPTunnel{
		conn:       conn,
		remoteAddr: remoteAddr,
		recvBuffer: make([]byte, 64*1024),
	}
}

// Read reads data from the tunnel
func (t *UDPTunnel) Read(b []byte) (int, error) {
	t.closeMutex.Lock()
	if t.closed {
		t.closeMutex.Unlock()
		return 0, io.EOF
	}
	t.closeMutex.Unlock()

	t.recvMutex.Lock()
	defer t.recvMutex.Unlock()

	// If we have buffered data, return it
	if t.recvOffset > 0 {
		n := copy(b, t.recvBuffer[:t.recvOffset])
		if n < t.recvOffset {
			copy(t.recvBuffer, t.recvBuffer[n:t.recvOffset])
		}
		t.recvOffset -= n
		return n, nil
	}

	// Read from UDP
	buf := make([]byte, maxPacketSize)
	t.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	n, addr, err := t.conn.ReadFromUDP(buf)
	if err != nil {
		return 0, err
	}

	// Check if it's from the right peer
	if addr.String() != t.remoteAddr.String() {
		return 0, nil // skip, not an error
	}

	if n < 5 {
		return 0, nil // invalid packet
	}

	pktType := buf[0]
	seqNum := binary.BigEndian.Uint32(buf[1:5])
	_ = seqNum // for now, we don't use sequence numbers for simplicity

	if pktType == pktTypeData {
		dataLen := n - 5
		if dataLen > 0 {
			if len(b) >= dataLen {
				copy(b, buf[5:n])
				return dataLen, nil
			}
			// Buffer if too big
			copy(t.recvBuffer, buf[5:n])
			t.recvOffset = dataLen
			nCopy := copy(b, t.recvBuffer[:t.recvOffset])
			copy(t.recvBuffer, t.recvBuffer[nCopy:t.recvOffset])
			t.recvOffset -= nCopy
			return nCopy, nil
		}
	}

	return 0, nil
}

// Write writes data to the tunnel
func (t *UDPTunnel) Write(b []byte) (int, error) {
	t.closeMutex.Lock()
	if t.closed {
		t.closeMutex.Unlock()
		return 0, io.EOF
	}
	t.closeMutex.Unlock()

	t.sendMutex.Lock()
	defer t.sendMutex.Unlock()

	totalWritten := 0
	for totalWritten < len(b) {
		chunkSize := len(b) - totalWritten
		if chunkSize > maxPacketSize-5 {
			chunkSize = maxPacketSize - 5
		}

		pkt := make([]byte, 5+chunkSize)
		pkt[0] = pktTypeData
		binary.BigEndian.PutUint32(pkt[1:5], 0) // seq num, not used yet
		copy(pkt[5:], b[totalWritten:totalWritten+chunkSize])

		// Send with retries
		var err error
		for i := 0; i < 3; i++ {
			_, err = t.conn.WriteToUDP(pkt, t.remoteAddr)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		if err != nil {
			return totalWritten, err
		}

		totalWritten += chunkSize
	}

	return totalWritten, nil
}

// Close closes the tunnel
func (t *UDPTunnel) Close() error {
	t.closeMutex.Lock()
	defer t.closeMutex.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true
	return t.conn.Close()
}

// RemoteAddr returns the remote address
func (t *UDPTunnel) RemoteAddr() string {
	return t.remoteAddr.String()
}

// TryHolePunching tries to establish a P2P connection
func TryHolePunching(localConn *net.UDPConn, peerAddrs []*net.UDPAddr) (*UDPTunnel, error) {
	log.Printf("Trying hole punching to %d addresses", len(peerAddrs))

	// Send punch packets to all addresses
	testData := []byte("HELLO_P2P")
	for _, addr := range peerAddrs {
		if addr == nil {
			continue
		}
		log.Printf("Punching to %v", addr)
		for i := 0; i < 5; i++ {
			_, _ = localConn.WriteToUDP(testData, addr)
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Wait for response
	buf := make([]byte, 1500)
	localConn.SetReadDeadline(time.Now().Add(3 * time.Second))

	for {
		n, addr, err := localConn.ReadFromUDP(buf)
		if err != nil {
			break
		}

		log.Printf("Received response from %v: %q", addr, string(buf[:n]))

		// Check if this is a P2P response
		if string(buf[:n]) == "HELLO_P2P" || string(buf[:n]) == "HELLO_ACK" {
			// Send ack
			_, _ = localConn.WriteToUDP([]byte("HELLO_ACK"), addr)

			log.Printf("P2P connection established with %v", addr)
			return NewUDPTunnel(localConn, addr), nil
		}
	}

	return nil, nil
}
