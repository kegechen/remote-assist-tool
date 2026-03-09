package p2p

import (
	"encoding/binary"
	"fmt"
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

	// Reliable transport constants
	retransmitBase     = 200 * time.Millisecond // Initial retransmit timeout
	maxRetries         = 5                      // Max retransmit attempts per packet
	recvChanSize       = 256                    // In-order delivery channel buffer
	maxRecvBuf         = 256                    // Max out-of-order buffered packets
	retransmitInterval = 50 * time.Millisecond  // How often to check for retransmits
)

// pendingPacket tracks an unacknowledged sent packet
type pendingPacket struct {
	data    []byte    // full packet bytes (header + payload)
	sentAt  time.Time // when last sent (for retransmit timing)
	retries int       // number of retransmit attempts
}

// UDPTunnel represents a reliable UDP tunnel with ordered delivery
type UDPTunnel struct {
	conn       *net.UDPConn
	remoteAddr *net.UDPAddr

	// Send state
	sendMutex   sync.Mutex                // protects Write (chunking + seq assignment)
	nextSendSeq uint32                    // next sequence number to assign
	sendWin     map[uint32]*pendingPacket // unacked packets awaiting ACK
	sendWinMu   sync.Mutex               // protects sendWin

	// Receive state
	nextRecvSeq uint32            // next expected sequence number
	recvBuf     map[uint32][]byte // out-of-order packet buffer
	recvMu      sync.Mutex        // protects nextRecvSeq and recvBuf
	recvCh      chan []byte        // in-order delivery channel

	// Partial read buffer (when payload > caller's Read buffer)
	partialData []byte
	partialOff  int

	closed     bool
	closeMutex sync.Mutex
	stopCh     chan struct{}
}

// NewUDPTunnel creates a new reliable UDP tunnel
func NewUDPTunnel(conn *net.UDPConn, remoteAddr *net.UDPAddr) *UDPTunnel {
	t := &UDPTunnel{
		conn:       conn,
		remoteAddr: remoteAddr,
		sendWin:    make(map[uint32]*pendingPacket),
		recvBuf:    make(map[uint32][]byte),
		recvCh:     make(chan []byte, recvChanSize),
		stopCh:     make(chan struct{}),
	}
	go t.recvLoop()
	go t.retransmitLoop()
	go t.keepaliveLoop()
	return t
}

// Read reads in-order data from the tunnel
func (t *UDPTunnel) Read(b []byte) (int, error) {
	// Return partial data from previous oversized read
	if t.partialOff > 0 {
		n := copy(b, t.partialData[:t.partialOff])
		if n < t.partialOff {
			copy(t.partialData, t.partialData[n:t.partialOff])
		}
		t.partialOff -= n
		return n, nil
	}

	// Wait for next in-order payload from recvLoop
	select {
	case <-t.stopCh:
		return 0, io.EOF
	case data, ok := <-t.recvCh:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, data)
		if n < len(data) {
			// Buffer remainder for next Read call
			if t.partialData == nil {
				t.partialData = make([]byte, 64*1024)
			}
			t.partialOff = copy(t.partialData, data[n:])
		}
		return n, nil
	}
}

// Write writes data to the tunnel with reliable delivery
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

		seq := t.nextSendSeq
		t.nextSendSeq++

		pkt := make([]byte, 5+chunkSize)
		pkt[0] = pktTypeData
		binary.BigEndian.PutUint32(pkt[1:5], seq)
		copy(pkt[5:], b[totalWritten:totalWritten+chunkSize])

		// Track for retransmission
		t.sendWinMu.Lock()
		t.sendWin[seq] = &pendingPacket{
			data:   pkt,
			sentAt: time.Now(),
		}
		t.sendWinMu.Unlock()

		// Send packet
		if _, err := t.conn.WriteToUDP(pkt, t.remoteAddr); err != nil {
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

// recvLoop reads UDP packets and handles ACKs, reordering, and in-order delivery
func (t *UDPTunnel) recvLoop() {
	buf := make([]byte, maxPacketSize)
	for {
		select {
		case <-t.stopCh:
			return
		default:
		}

		t.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // timeout is not fatal, keepalive keeps NAT alive
			}
			// Fatal error - close tunnel
			t.Close()
			return
		}

		// Ignore packets from wrong peer
		if addr.String() != t.remoteAddr.String() {
			continue
		}

		if n < 5 {
			continue
		}

		pktType := buf[0]
		seq := binary.BigEndian.Uint32(buf[1:5])

		switch pktType {
		case pktTypeKeepalive:
			// ignore

		case pktTypeAck:
			// Remove from send window - packet was received by peer
			t.sendWinMu.Lock()
			delete(t.sendWin, seq)
			t.sendWinMu.Unlock()

		case pktTypeData:
			// Always ACK immediately to stop retransmissions
			t.sendAck(seq)

			dataLen := n - 5
			if dataLen <= 0 {
				continue
			}

			payload := make([]byte, dataLen)
			copy(payload, buf[5:n])

			t.recvMu.Lock()
			if seq == t.nextRecvSeq {
				// In-order: deliver immediately
				t.deliverToChannel(payload)
				t.nextRecvSeq++

				// Deliver any consecutive buffered packets
				for {
					if buffered, ok := t.recvBuf[t.nextRecvSeq]; ok {
						t.deliverToChannel(buffered)
						delete(t.recvBuf, t.nextRecvSeq)
						t.nextRecvSeq++
					} else {
						break
					}
				}
			} else if seq > t.nextRecvSeq {
				// Out of order: buffer for later delivery
				if _, exists := t.recvBuf[seq]; !exists && len(t.recvBuf) < maxRecvBuf {
					t.recvBuf[seq] = payload
				}
			}
			// seq < nextRecvSeq is a duplicate, ACK already sent above
			t.recvMu.Unlock()
		}
	}
}

// deliverToChannel sends payload to the receive channel for Read() to consume.
// Blocks if channel is full (natural backpressure). Must be called with recvMu held.
func (t *UDPTunnel) deliverToChannel(data []byte) {
	select {
	case t.recvCh <- data:
	case <-t.stopCh:
	}
}

// sendAck sends an ACK packet for the given sequence number
func (t *UDPTunnel) sendAck(seq uint32) {
	pkt := make([]byte, 5)
	pkt[0] = pktTypeAck
	binary.BigEndian.PutUint32(pkt[1:5], seq)
	t.conn.WriteToUDP(pkt, t.remoteAddr)
}

// retransmitLoop periodically checks for unacked packets and retransmits them
func (t *UDPTunnel) retransmitLoop() {
	ticker := time.NewTicker(retransmitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			t.sendWinMu.Lock()
			for seq, pkt := range t.sendWin {
				// Exponential backoff: 200ms, 400ms, 800ms, 1.6s, 3.2s
				rto := retransmitBase << uint(pkt.retries)
				if now.Sub(pkt.sentAt) < rto {
					continue
				}
				if pkt.retries >= maxRetries {
					// Packet permanently lost after all retries - tunnel is unreliable now
					log.Printf("P2P: packet seq=%d dropped after %d retries, closing tunnel", seq, maxRetries)
					t.sendWinMu.Unlock()
					t.Close()
					return
				}
				pkt.retries++
				pkt.sentAt = now
				t.conn.WriteToUDP(pkt.data, t.remoteAddr)
			}
			t.sendWinMu.Unlock()
		}
	}
}

// keepaliveLoop sends keepalive packets to keep NAT mapping alive
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
			if _, err := t.conn.WriteToUDP(pkt, t.remoteAddr); err != nil {
				log.Printf("Keepalive send failed: %v", err)
				return
			}
		}
	}
}

// Stats returns tunnel statistics for debugging
func (t *UDPTunnel) Stats() string {
	t.sendWinMu.Lock()
	unacked := len(t.sendWin)
	t.sendWinMu.Unlock()

	t.recvMu.Lock()
	buffered := len(t.recvBuf)
	nextRecv := t.nextRecvSeq
	t.recvMu.Unlock()

	return fmt.Sprintf("unacked=%d buffered=%d nextRecvSeq=%d", unacked, buffered, nextRecv)
}
