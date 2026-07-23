package p2p

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/ratelimit"
)

// Relay constants
const (
	relayMarker     = 0xFF
	relaySessionTTL = 5 * time.Minute

	// maxSessionIDLen 控制面 sessionID 的最大字节数，防止超长字段撑爆解析缓冲。
	maxSessionIDLen = 64
	// maxUDPPayloadSize P2P UDP relay 单包有效载荷上限（约 1400 字节，留 MTU 余量）。
	maxUDPPayloadSize = 1400
	// maxRelayDatagramSize relay 数据报完整上限 = marker(1) + sidLen(1) + sid + payload。
	maxRelayDatagramSize = 1 + 1 + maxSessionIDLen + maxUDPPayloadSize

	// Worker 池配置：防 UDP 包洪水无界创建 goroutine 消耗资源
	stunWorkerCount    = 4   // 固定 worker 数（STUN 响应轻量，4 个足够）
	stunTaskQueueDepth = 256 // 有界队列深度（超出则丢包，backpressure）

	udpRatePerIP           = 1000
	udpBurstPerIP          = 2000
	udpRateGlobal          = 20000
	udpBurstGlobal         = 40000
	udpLimiterMaxKeys      = 8192
	udpLimiterIdle         = 10 * time.Minute
	maxRelaySessionsTotal  = 5000
	maxRelaySessionsPerIP  = 64
	relayBytesPerSession   = 2 * 1024 * 1024
	relayBytesBurstSession = 4 * 1024 * 1024
	relayBytesGlobal       = 20 * 1024 * 1024
	relayBytesBurstGlobal  = 40 * 1024 * 1024
	invalidLogSampleEvery  = 1000
)

// stunTask 代表一个待处理的 UDP 包（STUN 或 relay）。data 已从 serve 循环复制。
type stunTask struct {
	data       []byte
	remoteAddr *net.UDPAddr
	isRelay    bool // true=relay, false=STUN
}

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
	if sidLen == 0 || sidLen > maxSessionIDLen {
		return "", nil, false
	}
	if len(data) < 2+sidLen {
		return "", nil, false
	}
	return string(data[2 : 2+sidLen]), data[2+sidLen:], true
}

// relaySession tracks two peers for UDP relay forwarding
type relaySession struct {
	peers       [2]*net.UDPAddr
	count       int
	lastSeen    time.Time
	creatorIP   string
	byteLimiter *ratelimit.Bucket
}

// STUNServer is a simple STUN server with UDP relay capability
type STUNServer struct {
	conn      *net.UDPConn
	done      chan struct{}
	closeOnce sync.Once
	runWG     sync.WaitGroup
	// UDP relay sessions (auto-discovered from relay packets)
	relaySessions   map[string]*relaySession
	relayMu         sync.Mutex
	relayCountPerIP map[string]int
	relayByteGlobal *ratelimit.Bucket

	// invalidPktCount 无效/超长 relay 包计数，用于采样日志（不逐包写日志）。原子访问。
	invalidPktCount uint64

	// dataSessionValidator 数据面准入校验器，由 relay 层在构造时注入（IsActiveDataSession）。
	// nil 表示禁用 UDP relay，仅保留 STUN binding。在 serve() 启动前设定，之后只读。
	dataSessionValidator func(sessionID string) bool

	// Worker 池：固定 goroutine 从 taskQueue 取任务处理，避免每包一个 goroutine。
	taskQueue        chan stunTask
	workers          sync.WaitGroup
	udpLimiterPerIP  *ratelimit.KeyedLimiter
	udpLimiterGlobal *ratelimit.Bucket
	limits           Limits
}

// logSampledInvalidPacket 采样记录被丢弃的无效 relay 数据报，避免攻击者逐包刷日志。
func (s *STUNServer) logSampledInvalidPacket(addr *net.UDPAddr, reason string) {
	n := atomic.AddUint64(&s.invalidPktCount, 1)
	if shouldSample(n, s.limits.InvalidLogSampleEvery) {
		log.Printf("UDP relay: dropped invalid datagram from %v (reason=%s, sample_total=%d, sample_every=%d)", addr, reason, n, s.limits.InvalidLogSampleEvery)
	}
}

func shouldSample(n, every uint64) bool {
	return every <= 1 || (n-1)%every == 0
}

// NewSTUNServer 创建仅提供 STUN binding 的服务器；未提供 validator 时 UDP relay 禁用。
func NewSTUNServer(addr string) (*STUNServer, error) {
	return NewSTUNServerWithValidator(addr, nil)
}

// NewSTUNServerWithValidator 创建带数据面准入校验器的 STUN 服务器。
// validator 在 serve() goroutine 启动前设定，避免与读循环产生数据竞态。
func NewSTUNServerWithValidator(addr string, validator func(sessionID string) bool) (*STUNServer, error) {
	return NewSTUNServerWithValidatorAndLimits(addr, validator, DefaultLimits())
}

// NewSTUNServerWithValidatorAndLimits 创建使用指定运营阈值的 STUN/UDP relay。
func NewSTUNServerWithValidatorAndLimits(addr string, validator func(sessionID string) bool, limits Limits) (*STUNServer, error) {
	limits = normalizeLimits(limits)
	if err := ValidateLimits(limits); err != nil {
		return nil, err
	}
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}

	s := &STUNServer{
		conn:                 conn,
		done:                 make(chan struct{}),
		relaySessions:        make(map[string]*relaySession),
		relayCountPerIP:      make(map[string]int),
		relayByteGlobal:      ratelimit.NewBucket(float64(limits.RelayBytesGlobal), float64(limits.RelayBytesBurstGlobal)),
		dataSessionValidator: validator,
		taskQueue:            make(chan stunTask, limits.TaskQueueDepth),
		udpLimiterPerIP:      ratelimit.NewKeyedLimiter(limits.PacketsPerIPRate, limits.PacketsPerIPBurst, limits.LimiterMaxKeys, time.Duration(limits.LimiterIdleSeconds)*time.Second),
		udpLimiterGlobal:     ratelimit.NewBucket(limits.PacketsGlobalRate, limits.PacketsGlobalBurst),
		limits:               limits,
	}

	// 启动固定数量的 worker goroutine
	for i := 0; i < limits.WorkerCount; i++ {
		s.workers.Add(1)
		go s.worker()
	}

	s.runWG.Add(2)
	go func() {
		defer s.runWG.Done()
		s.serve()
	}()
	go func() {
		defer s.runWG.Done()
		s.cleanupRelaySessions()
	}()
	return s, nil
}

func (s *STUNServer) serve() {
	// max+1 缓冲：ReadFromUDP 保证 n <= len(buf)，任何超过业务上限的数据报都会返回
	// 至少 maxRelayDatagramSize+1 个已复制字节，从而进入拒绝分支（跨平台，不依赖 MSG_TRUNC）。
	buf := make([]byte, maxRelayDatagramSize+1)
	for {
		n, remoteAddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}

		remoteIP := remoteAddr.IP.String()
		if !s.udpLimiterPerIP.Allow(remoteIP) {
			s.logSampledInvalidPacket(remoteAddr, "pps_limited_ip")
			continue
		}
		if !s.udpLimiterGlobal.Allow() {
			s.logSampledInvalidPacket(remoteAddr, "pps_limited_global")
			continue
		}

		// Route: relay packets (0xFF prefix) vs STUN packets
		isRelay := n > 0 && buf[0] == relayMarker
		if isRelay {
			if n > maxRelayDatagramSize {
				// 超长 relay 数据报：丢弃且不入队、不创建/刷新 relay 状态。
				s.logSampledInvalidPacket(remoteAddr, "datagram_too_large")
				continue
			}
		}

		// PPS 准入后才复制，避免队列满时仍为每个攻击包分配任务对象。
		task := stunTask{
			data:       append([]byte(nil), buf[:n]...),
			remoteAddr: remoteAddr,
			isRelay:    isRelay,
		}
		select {
		case s.taskQueue <- task:
		case <-s.done:
			return
		default:
			// 队列满：丢包（backpressure），不阻塞读循环
			s.logSampledInvalidPacket(remoteAddr, "queue_full")
		}
	}
}

// worker 从任务队列取包处理（固定 goroutine 池，避免每包一个 goroutine）。
func (s *STUNServer) worker() {
	defer s.workers.Done()
	for task := range s.taskQueue {
		if task.isRelay {
			s.handleRelayPacket(task.data, task.remoteAddr)
		} else {
			s.handlePacket(task.data, task.remoteAddr)
		}
	}
}

func (s *STUNServer) handlePacket(data []byte, remoteAddr *net.UDPAddr) {
	msg, err := UnpackSTUN(data)
	if err != nil {
		s.logSampledInvalidPacket(remoteAddr, "invalid_stun")
		return
	}

	if msg.Type != STUNBindingRequest {
		s.logSampledInvalidPacket(remoteAddr, "non_binding_stun")
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
	_, err = s.conn.WriteToUDP(respBytes, remoteAddr)
	if err != nil {
		select {
		case <-s.done:
		default:
			s.logSampledInvalidPacket(remoteAddr, "stun_write_failed")
		}
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
	s.closeOnce.Do(func() {
		close(s.done)
		if s.conn != nil {
			s.conn.Close()
		}
		// producer 全部退出后才关闭队列，避免 send-on-closed-channel。
		s.runWG.Wait()
		close(s.taskQueue)
		s.workers.Wait()

		s.relayMu.Lock()
		for id, session := range s.relaySessions {
			s.deleteRelaySessionLocked(id, session)
		}
		s.relayMu.Unlock()
		log.Println("STUN server stopped")
	})
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
		// 头部非法（含 sidLen==0 / sidLen>maxSessionIDLen）或空 payload：丢弃，不创建/刷新 relay 状态。
		s.logSampledInvalidPacket(fromAddr, "bad_relay_header")
		return
	}

	// 快速预检减少无效包进入 relayMu；锁内还会复检并作为准入线性化点。
	if s.dataSessionValidator == nil || !s.dataSessionValidator(sessionID) {
		s.logSampledInvalidPacket(fromAddr, "inactive_data_session")
		return
	}

	s.relayMu.Lock()
	// 以 relayMu 内的复检作为准入线性化点。控制面从不持 sm.mu 调用 STUN，
	// 因此固定 relayMu -> sm.RLock 不会形成锁反转。
	if !s.dataSessionValidator(sessionID) {
		s.relayMu.Unlock()
		s.logSampledInvalidPacket(fromAddr, "inactive_data_session_recheck")
		return
	}
	session, exists := s.relaySessions[sessionID]
	if !exists {
		creatorIP := fromAddr.IP.String()
		if len(s.relaySessions) >= s.limits.MaxRelaySessionsTotal || s.relayCountPerIP[creatorIP] >= s.limits.MaxRelaySessionsPerIP {
			s.relayMu.Unlock()
			s.logSampledInvalidPacket(fromAddr, "relay_state_limited")
			return
		}
		session = &relaySession{
			lastSeen:    time.Now(),
			creatorIP:   creatorIP,
			byteLimiter: ratelimit.NewBucket(float64(s.limits.RelayBytesPerSession), float64(s.limits.RelayBytesBurstSession)),
		}
		s.relaySessions[sessionID] = session
		s.relayCountPerIP[creatorIP]++
	}
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
	if targetAddr != nil {
		// 先消费单会话出口额度，只有通过后才消费全局额度，避免热 session 饿死其它会话。
		if !session.byteLimiter.AllowN(float64(len(payload))) {
			s.relayMu.Unlock()
			s.logSampledInvalidPacket(fromAddr, "relay_bytes_session")
			return
		}
		if !s.relayByteGlobal.AllowN(float64(len(payload))) {
			s.relayMu.Unlock()
			s.logSampledInvalidPacket(fromAddr, "relay_bytes_global")
			return
		}
	}
	session.lastSeen = time.Now()
	s.relayMu.Unlock()

	if targetAddr != nil {
		s.conn.WriteToUDP(payload, targetAddr)
	}
}

// deleteRelaySessionLocked 删除 id 对应且与 expected 指针一致的 relay entry。
// 指针比对避免误删「同 id 但已是新一代」的 entry。调用方须持 s.relayMu。
func (s *STUNServer) deleteRelaySessionLocked(id string, expected *relaySession) {
	if cur, ok := s.relaySessions[id]; ok && cur == expected {
		delete(s.relaySessions, id)
		if cur.creatorIP != "" {
			if n := s.relayCountPerIP[cur.creatorIP]; n <= 1 {
				delete(s.relayCountPerIP, cur.creatorIP)
			} else {
				s.relayCountPerIP[cur.creatorIP] = n - 1
			}
		}
	}
}

// InvalidateRelaySession 由控制面在会话换代/断连/过期时主动失效对应 UDP relay 状态。
// 持 relayMu 查找并删除当前 entry；不存在则 no-op。绝不反向调用 SessionManager，
// 且必须在 SessionManager 解锁后调用，保证 sm.mu 与 relayMu 不同时持有。
func (s *STUNServer) InvalidateRelaySession(sessionID string) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if cur, ok := s.relaySessions[sessionID]; ok {
		s.deleteRelaySessionLocked(sessionID, cur)
		log.Printf("UDP relay: session %.30s... invalidated by control plane", sessionID)
	}
}

// cleanupRelaySessions removes stale relay sessions periodically
func (s *STUNServer) cleanupRelaySessions() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.relayMu.Lock()
			now := time.Now()
			for id, session := range s.relaySessions {
				if now.Sub(session.lastSeen) > relaySessionTTL {
					s.deleteRelaySessionLocked(id, session)
				}
			}
			s.relayMu.Unlock()
		}
	}
}
