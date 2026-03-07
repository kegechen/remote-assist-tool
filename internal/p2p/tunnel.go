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
	pktTypeData      = 0x01
	pktTypeAck       = 0x02
	pktTypeKeepalive = 0x03

	maxPacketSize     = 1400
	keepaliveInterval = 10 * time.Second
	readTimeout       = 60 * time.Second
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
	stopCh     chan struct{}
}

// NewUDPTunnel creates a new UDP tunnel
func NewUDPTunnel(conn *net.UDPConn, remoteAddr *net.UDPAddr) *UDPTunnel {
	t := &UDPTunnel{
		conn:       conn,
		remoteAddr: remoteAddr,
		recvBuffer: make([]byte, 64*1024),
		stopCh:     make(chan struct{}),
	}
	go t.keepaliveLoop()
	return t
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

	// Read from UDP with retry on timeout
	buf := make([]byte, maxPacketSize)
	for {
		t.closeMutex.Lock()
		if t.closed {
			t.closeMutex.Unlock()
			return 0, io.EOF
		}
		t.closeMutex.Unlock()

		t.conn.SetReadDeadline(time.Now().Add(readTimeout))
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// 超时不是致命错误，继续等待
				continue
			}
			return 0, err
		}

		// Check if it's from the right peer
		if addr.String() != t.remoteAddr.String() {
			continue
		}

		if n < 5 {
			continue
		}

		pktType := buf[0]

		// 忽略 keepalive 包
		if pktType == pktTypeKeepalive {
			continue
		}

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
	}
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
	close(t.stopCh)
	return t.conn.Close()
}

// RemoteAddr returns the remote address
func (t *UDPTunnel) RemoteAddr() string {
	return t.remoteAddr.String()
}

// keepaliveLoop 定期发送 keepalive 包，保持 NAT 映射不过期
func (t *UDPTunnel) keepaliveLoop() {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	pkt := make([]byte, 5)
	pkt[0] = pktTypeKeepalive
	binary.BigEndian.PutUint32(pkt[1:5], 0)

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.sendMutex.Lock()
			_, err := t.conn.WriteToUDP(pkt, t.remoteAddr)
			t.sendMutex.Unlock()
			if err != nil {
				log.Printf("Keepalive send failed: %v", err)
				return
			}
		}
	}
}

