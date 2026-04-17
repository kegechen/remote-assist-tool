package p2p

import (
	"bytes"
	"compress/flate"
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
	peerTimeout        = 60 * time.Second        // Close tunnel if no packet from peer for this long
	readDeadline       = 30 * time.Second        // UDP read deadline (shorter than peerTimeout for responsive detection)

	// 压缩相关
	compressThreshold = 128  // 小于此大小不压缩
	compressFlag      = 0x01 // payload 首字节标记：已压缩
	rawFlag           = 0x00 // payload 首字节标记：未压缩
)

var (
	flateWriterPool = sync.Pool{
		New: func() interface{} {
			w, _ := flate.NewWriter(nil, flate.DefaultCompression)
			return w
		},
	}
	bufPool = sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
)

// compressData 压缩数据，小包或压缩膨胀时返回带 rawFlag 前缀的原始数据
func compressData(data []byte) []byte {
	if len(data) < compressThreshold {
		out := make([]byte, 1+len(data))
		out[0] = rawFlag
		copy(out[1:], data)
		return out
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteByte(compressFlag)

	w := flateWriterPool.Get().(*flate.Writer)
	w.Reset(buf)
	w.Write(data)
	w.Close()
	flateWriterPool.Put(w)

	// 如果压缩后反而更大，返回原始数据
	if buf.Len() >= 1+len(data) {
		bufPool.Put(buf)
		out := make([]byte, 1+len(data))
		out[0] = rawFlag
		copy(out[1:], data)
		return out
	}

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	bufPool.Put(buf)
	return out
}

// decompressData 根据首字节标记决定是否解压
func decompressData(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	if data[0] == rawFlag {
		out := make([]byte, len(data)-1)
		copy(out, data[1:])
		return out, nil
	}
	if data[0] == compressFlag {
		r := flate.NewReader(bytes.NewReader(data[1:]))
		defer r.Close()
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
		return buf.Bytes(), nil
	}
	// 兼容：未知标记视为原始数据（不应发生）
	return data, nil
}

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

	// Relay mode: if non-nil, prepend this header to all outbound packets
	relayHeader []byte

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

	// Peer liveness detection
	lastRecvTime time.Time // last time any packet was received from peer

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
		conn:         conn,
		remoteAddr:   remoteAddr,
		lastRecvTime: time.Now(),
		sendWin:      make(map[uint32]*pendingPacket),
		recvBuf:      make(map[uint32][]byte),
		recvCh:       make(chan []byte, recvChanSize),
		stopCh:     make(chan struct{}),
	}
	go t.recvLoop()
	go t.retransmitLoop()
	go t.keepaliveLoop()
	return t
}

// NewUDPRelayTunnel creates a tunnel that routes through a UDP relay server
func NewUDPRelayTunnel(conn *net.UDPConn, relayAddr *net.UDPAddr, sessionID string) *UDPTunnel {
	t := &UDPTunnel{
		conn:         conn,
		remoteAddr:   relayAddr,
		relayHeader:  makeRelayHeader(sessionID),
		lastRecvTime: time.Now(),
		sendWin:      make(map[uint32]*pendingPacket),
		recvBuf:      make(map[uint32][]byte),
		recvCh:       make(chan []byte, recvChanSize),
		stopCh:       make(chan struct{}),
	}
	go t.recvLoop()
	go t.retransmitLoop()
	go t.keepaliveLoop()
	return t
}

// sendPacket sends a packet, adding relay header if in relay mode
func (t *UDPTunnel) sendPacket(pkt []byte) error {
	if t.relayHeader != nil {
		wrapped := make([]byte, len(t.relayHeader)+len(pkt))
		copy(wrapped, t.relayHeader)
		copy(wrapped[len(t.relayHeader):], pkt)
		_, err := t.conn.WriteToUDP(wrapped, t.remoteAddr)
		return err
	}
	_, err := t.conn.WriteToUDP(pkt, t.remoteAddr)
	return err
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
		// 预留 1 字节给压缩标记
		chunkSize := len(b) - totalWritten
		if chunkSize > maxPacketSize-6 {
			chunkSize = maxPacketSize - 6
		}

		chunk := compressData(b[totalWritten : totalWritten+chunkSize])

		seq := t.nextSendSeq
		t.nextSendSeq++

		pkt := make([]byte, 5+len(chunk))
		pkt[0] = pktTypeData
		binary.BigEndian.PutUint32(pkt[1:5], seq)
		copy(pkt[5:], chunk)

		// Track for retransmission
		t.sendWinMu.Lock()
		t.sendWin[seq] = &pendingPacket{
			data:   pkt,
			sentAt: time.Now(),
		}
		t.sendWinMu.Unlock()

		// Send packet
		if err := t.sendPacket(pkt); err != nil {
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

		t.conn.SetReadDeadline(time.Now().Add(readDeadline))
		n, addr, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Check if peer is still alive (keepalive arrives every 10s)
				if time.Since(t.lastRecvTime) > peerTimeout {
					log.Printf("P2P: no packet from peer for %v, closing tunnel", peerTimeout)
					t.Close()
					return
				}
				continue
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

		// Peer is alive - update liveness tracker
		t.lastRecvTime = time.Now()

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

			// 解压缩 payload
			payload, err := decompressData(buf[5:n])
			if err != nil {
				log.Printf("P2P: decompress error seq=%d: %v", seq, err)
				continue
			}

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
	t.sendPacket(pkt)
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
				t.sendPacket(pkt.data)
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
			if err := t.sendPacket(pkt); err != nil {
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
