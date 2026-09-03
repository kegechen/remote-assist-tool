package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/logger"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/ratelimit"
)

// 服务端加固默认值（防资源耗尽型 DoS / slowloris / 超大消息）
const (
	// maxMessageSize 单条消息上限。最大单条来自 read_file 响应：单次最多 1 MiB 原始字节
	// (readFileMaxChunk)，内层 JSON 把 []byte base64(×4/3)，AEADSealJSON 再对整段密文
	// base64(×4/3) 二次膨胀 ≈ 1.86 MiB + 信封；与 MCP 端 scanner 上限对齐取 4 MiB（约 2x 余量）。
	// 注意：maxMessageSize 与 readFileMaxChunk 强耦合，调大任一方需同步复核另一方。
	maxMessageSize = 4 << 20 // 4 MiB
	// readIdleTimeout 读空闲超时。客户端每 30s 发心跳，2 分钟（≈4 个心跳周期）内
	// 无任何消息即判定为僵尸 / slowloris 连接并断开。
	readIdleTimeout = 2 * time.Minute
	// writeTimeout 单次写超时，挡写端 slowloris：对端慢读撑满发送缓冲会让 Write 无限
	// 阻塞、卡死转发方的读循环 goroutine；超时即按失败关闭连接。
	writeTimeout = 30 * time.Second
	// maxConnsTotal 全局并发连接数上限。
	maxConnsTotal = 2000
	// maxConnsPerIP 单 IP 并发连接数上限，挡住单机海量建连。公网 relay 下企业出口/CGNAT
	// 会让多用户共用同一源 IP，故放宽到 128（全局 DoS 由 maxConnsTotal 兜底），避免误伤共享 NAT。
	maxConnsPerIP = 128

	// maxJoinFailures 单连接 Join 失败（含限流拒绝）达到此次数后关闭连接，挡协助码枚举爆破。
	maxJoinFailures = 5

	// Join 尝试限流：per-IP 挡连接循环爆破，global 兜底保护 relay。数值取宽松默认，
	// 正常协助端每会话只 Join 一次；共享 NAT 由 burst 提供余量，LAN/NoAuth 可整体旁路。
	joinRatePerIP   = 5
	joinBurstPerIP  = 20
	joinRateGlobal  = 200
	joinBurstGlobal = 400

	// rejectAuditSampleN 限流拒绝的审计日志采样率：每 N 次写一条，避免攻击下刷爆日志。
	rejectAuditSampleN = 1000

	// Create(注册)尝试限流：挡高频注册、会话创建爆破。
	createRatePerIP   = 2
	createBurstPerIP  = 10
	createRateGlobal  = 100
	createBurstGlobal = 200

	// 活跃会话数上限：防单 IP 或全局会话耗尽。
	maxActiveSessionsPerIP = 10
	maxActiveSessionsTotal = 5000

	// Heartbeat 限流：防心跳风暴（客户端正常 30s 一次，允许一定突发）
	heartbeatRatePerIP   = 10  // 每秒 10 次（正常是 0.033 次/秒）
	heartbeatBurstPerIP  = 20  // 允许突发 20 次
	heartbeatRateGlobal  = 500 // 全局每秒 500 次
	heartbeatBurstGlobal = 1000

	// Tunnel data 限流：防数据洪水（单位：消息数/秒，不是字节数）
	tunnelDataRatePerConn  = 100  // 单连接每秒 100 条消息
	tunnelDataBurstPerConn = 200  // 允许突发 200 条
	tunnelDataRateGlobal   = 5000 // 全局每秒 5000 条消息
	tunnelDataBurstGlobal  = 10000

	// 工具通道限流：单位是 KiB/s，按 payload 实际大小计费。
	//
	// 为什么不能沿用 Tunnel data 那套"条数/秒"：exec stream=true 的 chunkSink 是管道读到
	// 多少发多少（单帧上限 32 KiB），cat 一个大日志轻松超过 100 帧/秒——按条数限流会把
	// 一次完全正常的输出判成洪水。按字节算才对得上"占用了多少带宽"这件事。
	//
	// toolMinFrameCostKiB 是每帧的最低计费：按字节计费之后，海量小帧的字节数很低，
	// 但每帧都是一次 JSON 解析 + 转发，CPU 开销与大小无关。给个地板价把帧率一起压住
	// （16 MiB/s ÷ 16 KiB = 1024 帧/秒，远高于任何正常用法，也远低于洪水）。
	toolRateKiBPerConn  = 16384  // 单连接 16 MiB/s
	toolBurstKiBPerConn = 32768  // 允许突发 32 MiB
	toolRateKiBGlobal   = 131072 // 全局 128 MiB/s
	toolBurstKiBGlobal  = 262144 // 允许突发 256 MiB
	toolMinFrameCostKiB = 16     // 每帧地板价

	// 工具通道超速时读循环最多让路多久。单帧最大 4 MiB、速率 16 MiB/s，补满一帧只要
	// 250ms，所以正常情况永远碰不到这个上限——它只是防"限额配得离谱"时无限等待。
	toolThrottleMaxWait = 2 * time.Second
	toolThrottleStep    = 20 * time.Millisecond

	// 低频控制消息限流：PeerAddrAdvertise / P2PConnected。
	controlRatePerConn  = 2
	controlBurstPerConn = 5
	controlRateGlobal   = 1000
	controlBurstGlobal  = 2000
)

// 限流器 idle 参数
const (
	limiterIdle    = 10 * time.Minute
	limiterMaxKeys = 8192
)

// Config 服务器配置
type Config struct {
	ListenAddr     string
	TLSCertFile    string
	TLSKeyFile     string
	CodeTTL        time.Duration
	CodeLength     int
	AuditLogFile   string
	UseTLS         bool
	STUNListenAddr string // STUN server listen address (empty to disable)
	NoAuth         bool   // true: use fixed proto.NoAuthCode instead of random code generation (LAN-only, no auth)
	// DisableSourceIPLimits 旁路所有按来源 IP 计费的限制（零值=启用，安全默认）。
	// 仅用于无法恢复真实源 IP 的 SNAT 部署；per-connection 与 global 限制始终生效。
	DisableSourceIPLimits bool
	Limits                Limits
}

// Server 中转服务器
type Server struct {
	config     *Config
	sessions   *SessionManager
	codes      *CodeManager
	clients    map[string]*ClientConn
	clientsMu  sync.RWMutex
	stunServer *p2p.STUNServer

	connMu    sync.Mutex     // 保护 connTotal / connPerIP
	connTotal int            // 当前全局连接数
	connPerIP map[string]int // 每 IP 当前连接数

	joinLimiterPerIP  *ratelimit.KeyedLimiter // per-IP Join 尝试限流（自带并发安全）
	joinLimiterGlobal *ratelimit.Bucket       // 全局 Join 尝试限流（自带并发安全）
	rejectSampleCtr   uint64                  // 限流拒绝审计采样计数（原子）

	createLimiterPerIP  *ratelimit.KeyedLimiter // per-IP Create(注册)尝试限流
	createLimiterGlobal *ratelimit.Bucket       // 全局 Create 尝试限流

	heartbeatLimiterPerIP  *ratelimit.KeyedLimiter // per-IP Heartbeat 限流
	heartbeatLimiterGlobal *ratelimit.Bucket       // 全局 Heartbeat 限流

	tunnelDataLimiterPerConn *ratelimit.KeyedLimiter // per-connection Tunnel data 限流
	tunnelDataLimiterGlobal  *ratelimit.Bucket       // 全局 Tunnel data 限流
	controlLimiterPerConn    *ratelimit.KeyedLimiter // per-connection 控制消息限流
	controlLimiterGlobal     *ratelimit.Bucket       // 全局控制消息限流
	toolLimiterPerConn       *ratelimit.KeyedLimiter // per-connection 工具通道限流（KiB/s）
	toolLimiterGlobal        *ratelimit.Bucket       // 全局工具通道限流（KiB/s）
	logSampleCtr             uint64                  // TCP 高频拒绝日志采样计数
	p2pSampleCtr             uint64                  // P2P 协商信息独立采样，避免污染拒绝计数
	toolThrottleSampleCtr    uint64                  // 工具通道节流独立采样：节流是背压不是拒绝，同样不该污染拒绝计数
	limits                   Limits
}

// NewServer 创建服务器
func NewServer(cfg *Config) (*Server, error) {
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = 30 * time.Minute
	}
	if cfg.CodeLength == 0 {
		cfg.CodeLength = 10
	}
	if cfg.AuditLogFile != "" {
		if err := logger.InitAuditLogger(cfg.AuditLogFile); err != nil {
			log.Printf("Warning: failed to init audit log: %v", err)
		}
	}
	limits := normalizeLimits(cfg.Limits)
	if err := ValidateLimits(limits); err != nil {
		return nil, fmt.Errorf("invalid relay limits: %w", err)
	}
	cfg.Limits = limits
	limiterIdleDuration := time.Duration(limits.LimiterIdleSeconds) * time.Second

	srv := &Server{
		config:                   cfg,
		sessions:                 NewSessionManager(),
		codes:                    NewCodeManager(cfg.CodeLength),
		clients:                  make(map[string]*ClientConn),
		connPerIP:                make(map[string]int),
		joinLimiterPerIP:         ratelimit.NewKeyedLimiter(limits.JoinRatePerIP, limits.JoinBurstPerIP, limits.LimiterMaxKeys, limiterIdleDuration),
		joinLimiterGlobal:        ratelimit.NewBucket(limits.JoinRateGlobal, limits.JoinBurstGlobal),
		createLimiterPerIP:       ratelimit.NewKeyedLimiter(limits.CreateRatePerIP, limits.CreateBurstPerIP, limits.LimiterMaxKeys, limiterIdleDuration),
		createLimiterGlobal:      ratelimit.NewBucket(limits.CreateRateGlobal, limits.CreateBurstGlobal),
		heartbeatLimiterPerIP:    ratelimit.NewKeyedLimiter(limits.HeartbeatRatePerIP, limits.HeartbeatBurstPerIP, limits.LimiterMaxKeys, limiterIdleDuration),
		heartbeatLimiterGlobal:   ratelimit.NewBucket(limits.HeartbeatRateGlobal, limits.HeartbeatBurstGlobal),
		tunnelDataLimiterPerConn: ratelimit.NewKeyedLimiter(limits.DataRatePerConnection, limits.DataBurstPerConnection, limits.LimiterMaxKeys, limiterIdleDuration),
		tunnelDataLimiterGlobal:  ratelimit.NewBucket(limits.DataRateGlobal, limits.DataBurstGlobal),
		controlLimiterPerConn:    ratelimit.NewKeyedLimiter(limits.ControlRatePerConnection, limits.ControlBurstPerConnection, limits.LimiterMaxKeys, limiterIdleDuration),
		controlLimiterGlobal:     ratelimit.NewBucket(limits.ControlRateGlobal, limits.ControlBurstGlobal),
		toolLimiterPerConn:       ratelimit.NewKeyedLimiter(limits.ToolRateKiBPerConnection, limits.ToolBurstKiBPerConnection, limits.LimiterMaxKeys, limiterIdleDuration),
		toolLimiterGlobal:        ratelimit.NewBucket(limits.ToolRateKiBGlobal, limits.ToolBurstKiBGlobal),
		limits:                   limits,
	}
	// Help 去抖计时器真正清掉 Help 槽时，把 share 从「等隧道数据」里叫醒。
	// 复用既有的 PEER_DISCONNECTED 码：share 端 waitAndHandleTunnel 已经认它
	// （share.go 的 MsgError 分支 → ErrPeerDisconnected → Run 打印「协助码仍有效」
	// 并等待新协助端），只是此前 relay 从不在 Help 断连时发，那条路径一直是死代码。
	// 在 Start 之前装好，之后只读不写，无并发。
	srv.sessions.onHelpCleared = srv.notifyShareHelpGone
	return srv, nil
}

// notifyShareHelpGone 在 SessionManager 解锁后被调用，可安全做同步网络写。
func (s *Server) notifyShareHelpGone(share *ClientConn) {
	s.sendError(share, proto.ErrCodePeerDisconnected, "协助端已断开连接")
}

// Start starts the server (backward compatible)
func (s *Server) Start() error {
	return s.StartWithContext(context.Background())
}

// StartWithContext starts the server with context for graceful shutdown
func (s *Server) StartWithContext(ctx context.Context) error {
	return s.StartWithContextReady(ctx, nil)
}

// StartWithContextReady starts the server and calls onReady after all listeners
// are established, immediately before the accept loop begins.
func (s *Server) StartWithContextReady(ctx context.Context, onReady func()) error {
	// Start STUN server if configured
	if s.config.STUNListenAddr != "" {
		var err error
		// 注入数据面准入校验：STUN relay 只为控制面就绪的会话创建/刷新 UDP 状态。
		s.stunServer, err = p2p.NewSTUNServerWithValidatorAndLimits(s.config.STUNListenAddr, s.sessions.IsActiveDataSession, s.limits.UDP)
		if err != nil {
			log.Printf("Warning: failed to start STUN server: %v", err)
		} else {
			log.Printf("STUN server listening on %s", s.stunServer.LocalAddr())
			defer s.stunServer.Close()
		}
	}

	var listener net.Listener
	var err error

	if s.config.UseTLS && s.config.TLSCertFile != "" && s.config.TLSKeyFile != "" {
		var tlsConfig *tls.Config
		tlsConfig, err = crypto.NewTLSConfig(s.config.TLSCertFile, s.config.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("failed to create TLS config: %w", err)
		}
		listener, err = tls.Listen("tcp", s.config.ListenAddr, tlsConfig)
	} else {
		listener, err = net.Listen("tcp", s.config.ListenAddr)
	}

	if err != nil {
		return err
	}

	log.Printf("Server starting on %s", s.config.ListenAddr)
	log.Printf("Relay limits: %s", s.limits.JSON())
	go s.cleanupLoop(ctx)

	// Close listener when context is cancelled
	go func() {
		<-ctx.Done()
		log.Printf("Shutting down server...")
		listener.Close()
	}()
	if onReady != nil {
		onReady()
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				log.Printf("Server stopped")
				return nil
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}
		go s.handleConn(conn)
	}
}

// acquireConnSlot 连接准入：未超限则占一个名额并返回 true；超限返回 false，调用方应关闭连接。
func (s *Server) acquireConnSlot(ip string) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.connTotal >= s.limits.MaxConnectionsTotal {
		return false
	}
	if !s.sourceIPLimitsDisabled() && s.connPerIP[ip] >= s.limits.MaxConnectionsPerIP {
		return false
	}
	s.connTotal++
	s.connPerIP[ip]++
	return true
}

// sourceIPLimitsDisabled 保持 Server 零值安全：nil config 与配置零值都表示启用限制。
func (s *Server) sourceIPLimitsDisabled() bool {
	return s.config != nil && s.config.DisableSourceIPLimits
}

func (s *Server) logSampled(format string, args ...interface{}) {
	n := atomic.AddUint64(&s.logSampleCtr, 1)
	if sampleHit(n, s.limits.RejectAuditSampleEvery) {
		args = append(args, n, s.limits.RejectAuditSampleEvery)
		log.Printf(format+" (sample_total=%d, sample_every=%d)", args...)
	}
}

func (s *Server) logP2PSampled(format string, args ...interface{}) {
	n := atomic.AddUint64(&s.p2pSampleCtr, 1)
	if sampleHit(n, s.limits.RejectAuditSampleEvery) {
		args = append(args, n, s.limits.RejectAuditSampleEvery)
		log.Printf(format+" (p2p_sample_total=%d, sample_every=%d)", args...)
	}
}

// logToolThrottleSampled 与 logP2PSampled 同理：工具通道节流是正常的背压（帧一条没丢，
// 只是慢了一拍），不是拒绝。混进 logSampleCtr 会让 sample_total 的差值不再等于"期间被拒
// 绝的消息数"，运维按文档去推算就会推错。
func (s *Server) logToolThrottleSampled(format string, args ...interface{}) {
	n := atomic.AddUint64(&s.toolThrottleSampleCtr, 1)
	if sampleHit(n, s.limits.RejectAuditSampleEvery) {
		args = append(args, n, s.limits.RejectAuditSampleEvery)
		log.Printf(format+" (tool_throttle_sample_total=%d, sample_every=%d)", args...)
	}
}

func sampleHit(n, every uint64) bool {
	return every <= 1 || (n-1)%every == 0
}

func (s *Server) allowDataMessage(client *ClientConn) bool {
	if s.tunnelDataLimiterPerConn != nil && !s.tunnelDataLimiterPerConn.Allow(client.ID) {
		s.logSampled("Data message rate limited (per-conn) from %s", client.ID)
		return false
	}
	if s.tunnelDataLimiterGlobal != nil && !s.tunnelDataLimiterGlobal.Allow() {
		s.logSampled("Data message rate limited (global) from %s", client.ID)
		return false
	}
	return true
}

// allowToolMessage 按 payload 字节数给工具通道计费，超速时**压住这条连接的读循环**
// 而不是丢帧。
//
// relay 的转发是同步的：读一条、转一条。不读就等于不收，TCP 窗口自然收敛，背压一路
// 传回发送端——发送端被压慢，一个字节都不丢。这正是丢帧唯一该被替换掉的原因：payload
// 是 AEAD 的、每帧独立 nonce，中间少掉一帧不影响后续解密，两端谁都发现不了，用户拿到
// 的是一份被悄悄挖空、却仍标着成功的输出。
//
// 只有等到 toolThrottleMaxWait 还拿不到额度才返回 false（调用方回 RATE_LIMITED 并断连）。
// 按默认限额这条路走不到：单帧最大 4 MiB，16 MiB/s 补满只需 250ms。它是配置离谱时的兜底，
// 保证读循环不会真的卡死。
//
// 每只桶一帧只扣一次：per-conn 扣成功后用 perConnPaid 记住，后面即使还在等 global 也不再
// 重扣。否则全局被别人打满时，本连接每 20ms 重试一次就白扣一次自己的额度——等 global 恢复，
// 自己的桶反倒先空了，成了"被别人的流量拖垮"。这类跨连接的放大效应正是 per-conn 桶要防的。
func (s *Server) allowToolMessage(client *ClientConn, payloadBytes int) bool {
	cost := float64((payloadBytes + 1023) / 1024)
	if cost < toolMinFrameCostKiB {
		cost = toolMinFrameCostKiB
	}
	deadline := time.Now().Add(toolThrottleMaxWait)
	perConnPaid := s.toolLimiterPerConn == nil
	throttled := false
	for {
		if !perConnPaid {
			perConnPaid = s.toolLimiterPerConn.AllowN(client.ID, cost)
		}
		if perConnPaid && (s.toolLimiterGlobal == nil || s.toolLimiterGlobal.AllowN(cost)) {
			return true
		}
		if !throttled {
			// 每次调用只记一条：按重试次数记会让采样计数器随睡眠粒度浮动，失去可比性。
			throttled = true
			s.logToolThrottleSampled("Tool channel throttled from %s", client.ID)
		}
		if !time.Now().Before(deadline) {
			s.logSampled("Tool channel rate limit exceeded past throttle window from %s", client.ID)
			return false
		}
		time.Sleep(toolThrottleStep)
	}
}

func (s *Server) allowControlMessage(client *ClientConn) bool {
	if s.controlLimiterPerConn != nil && !s.controlLimiterPerConn.Allow(client.ID) {
		s.logSampled("Control message rate limited (per-conn) from %s", client.ID)
		return false
	}
	if s.controlLimiterGlobal != nil && !s.controlLimiterGlobal.Allow() {
		s.logSampled("Control message rate limited (global) from %s", client.ID)
		return false
	}
	return true
}

// releaseConnSlot 连接结束时释放名额。必须与一次成功的 acquireConnSlot 严格配对调用；
// 内部对未持有名额的 ip 做自防御，避免误调用把 connTotal 推成负数、削弱限流。
func (s *Server) releaseConnSlot(ip string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	cur, ok := s.connPerIP[ip]
	if !ok {
		return
	}
	if cur <= 1 {
		delete(s.connPerIP, ip)
	} else {
		s.connPerIP[ip] = cur - 1
	}
	if s.connTotal > 0 {
		s.connTotal--
	}
}

// handleConn 处理连接
func (s *Server) handleConn(conn net.Conn) {
	clientID := generateClientID()
	clientIP := conn.RemoteAddr().String()

	// 连接数准入：按源 IP 限流，超限直接拒绝，防资源耗尽型 DoS
	host, _, err := net.SplitHostPort(clientIP)
	if err != nil {
		host = clientIP
	}
	if !s.acquireConnSlot(host) {
		log.Printf("Connection rejected from %s: too many connections", clientIP)
		conn.Close()
		return
	}
	defer s.releaseConnSlot(host)

	// Enable TCP KeepAlive to detect dead connections
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	} else if tlsConn, ok := conn.(*tls.Conn); ok {
		if tcpConn, ok := tlsConn.NetConn().(*net.TCPConn); ok {
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(30 * time.Second)
		}
	}

	log.Printf("New connection from %s (client_id: %s, version: pending)", clientIP, clientID)
	logger.LogConnection(clientIP, clientID, true, "客户端已连接")

	wrapped := &connWrapper{Conn: conn}

	client := &ClientConn{
		ID:   clientID,
		Conn: wrapped,
		Send: make(chan []byte, 100),
	}

	s.clientsMu.Lock()
	s.clients[clientID] = client
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, clientID)
		s.clientsMu.Unlock()
		result := s.sessions.DisconnectClient(clientID)
		if result != nil {
			// 断连时主动失效旧 UDP relay entry（SessionManager 已解锁）。
			if result.ResetDataPlane && s.stunServer != nil {
				s.stunServer.InvalidateRelaySession(result.SessionID)
			}
			if result.PeerToNotify != nil {
				s.sendError(result.PeerToNotify, proto.ErrCodePeerDisconnected, "被协助端已断开连接")
			}
		}
		conn.Close()
		log.Printf("Connection closed: %s", clientID)
	}()

	// 读循环：按行读。依赖写端契约——每条消息均为紧凑单行 JSON 并以 '\n' 分隔
	// （relay sendMsg = json.Marshal+'\n'；client = json.Encoder.Encode）；禁止 MarshalIndent。
	// 限制单条消息大小（maxMessageSize）并对每次读设空闲超时（readIdleTimeout），
	// 挡住超大消息 OOM 与 slowloris 僵尸连接。
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMessageSize)
	for {
		conn.SetReadDeadline(time.Now().Add(readIdleTimeout))
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				log.Printf("Read error from %s: %v", clientID, err)
			}
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg proto.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("Invalid message from %s: %v", clientID, err)
			return
		}
		if s.handleMessage(client, &msg) {
			return
		}
	}
}

// handleMessage 处理消息。返回 closeConn=true 时读循环应关闭连接。
// 状态机：每连接只能执行一次注册(register 或 join)；业务消息必须在注册后。
func (s *Server) handleMessage(client *ClientConn, msg *proto.Message) (closeConn bool) {
	switch msg.Type {
	case proto.MsgRegisterRequest:
		// register 只能在 new 状态执行一次
		if client.Type != connStateNew {
			log.Printf("Protocol error: register after %s from %s", client.Type, client.ID)
			return true
		}
		var req proto.RegisterRequest
		if err := proto.DecodePayload(msg, &req); err != nil {
			// register payload 解码失败立即关闭
			log.Printf("Malformed register payload from %s: PROTO_DECODE", client.ID)
			return true
		}
		// 身份字段只在连接首帧写入（Type 为空 = ClientConn 尚未发布进会话）：
		// 发布后其他 goroutine 可能并发读这些字段，读循环 goroutine 的无锁
		// 重写构成数据竞态；正常客户端每个连接也只发一次 register/join。
		client.ClientID = req.ClientID
		client.Version = sanitizePeerString(req.Version)
		client.Host = sanitizePeerString(req.Host)
		return s.handleRegister(client)
	case proto.MsgJoinRequest:
		// join 只能在 new 状态执行一次
		if client.Type != connStateNew {
			log.Printf("Protocol error: join after %s from %s", client.Type, client.ID)
			return true
		}
		var req proto.JoinRequest
		if err := proto.DecodePayload(msg, &req); err != nil {
			// 畸形 payload 不走 rejectJoin：按状态机直接关闭连接。
			log.Printf("Malformed join payload from %s: PROTO_DECODE", client.ID)
			return true
		}
		client.Version = sanitizePeerString(req.Version)
		client.Host = sanitizePeerString(req.Host)
		return s.handleJoin(client, req.Code)
	case proto.MsgTunnelData, proto.MsgHeartbeat, proto.MsgPeerAddrAdvertise, proto.MsgP2PConnected,
		proto.MsgToolHello, proto.MsgToolHelloAck, proto.MsgToolReq, proto.MsgToolResp,
		proto.MsgToolStream, proto.MsgToolCancel:
		// 业务消息必须在注册后(share 或 help)
		if client.Type == connStateNew {
			log.Printf("Protocol error: %s before register/join from %s", msg.Type, client.ID)
			return true
		}
		// 分派到各业务 handler
		switch msg.Type {
		case proto.MsgTunnelData:
			if !s.allowDataMessage(client) {
				return false
			}
			s.handleTunnelData(client, msg.Payload)
		case proto.MsgHeartbeat:
			// Heartbeat 限流：per-IP + global
			host := clientHost(client)
			if !s.sourceIPLimitsDisabled() && s.heartbeatLimiterPerIP != nil && !s.heartbeatLimiterPerIP.Allow(host) {
				s.logSampled("Heartbeat rate limited (per-IP) from %s", host)
				return false
			}
			if s.heartbeatLimiterGlobal != nil && !s.heartbeatLimiterGlobal.Allow() {
				s.logSampled("Heartbeat rate limited (global) from %s", host)
				return false
			}
			s.sendHeartbeat(client)
		case proto.MsgPeerAddrAdvertise:
			if !s.allowControlMessage(client) {
				return false
			}
			s.handlePeerAddrAdvertise(client, msg)
		case proto.MsgP2PConnected:
			if !s.allowControlMessage(client) {
				return false
			}
		case proto.MsgToolHello, proto.MsgToolHelloAck,
			proto.MsgToolReq, proto.MsgToolResp,
			proto.MsgToolStream, proto.MsgToolCancel:
			// 工具通道的帧一条都不能悄悄丢：ToolResp 丢了调用方只能干等到兜底超时，
			// ToolStream 丢了输出中间被挖掉一段而两端都察觉不到（payload 是 AEAD 的，
			// 每帧独立 nonce，丢帧不影响后续解密）。这里超限就回 RATE_LIMITED 并断连，
			// 把"静默的数据损坏"换成"一次看得见的失败"。
			if !s.allowToolMessage(client, len(msg.Payload)) {
				s.sendError(client, "RATE_LIMITED", "tool channel rate limit exceeded")
				return true
			}
			s.forwardToPeer(client, msg)
		}
	default:
		log.Printf("Unknown message type: %s", msg.Type)
		return true
	}
	return false
}

// forwardToPeer 把整条消息原样转给会话对端（用于工具通道：内容已 AEAD，relay 看不见也不需要看）
func (s *Server) forwardToPeer(client *ClientConn, msg *proto.Message) {
	target := s.sessions.FindPeer(client.ID)
	if target != nil {
		sendMsg(target, msg)
	}
}

// handleRegister 处理注册请求。返回 closeConn=true 时读循环应关闭连接。
// 状态前置条件：client.Type == connStateNew（已由 handleMessage 守卫）。
func (s *Server) handleRegister(client *ClientConn) (closeConn bool) {
	host := clientHost(client)

	// per-IP create 限流（可旁路）
	if !s.sourceIPLimitsDisabled() && s.createLimiterPerIP != nil && !s.createLimiterPerIP.Allow(host) {
		s.sendError(client, "RATE_LIMITED", "too many create attempts")
		return true
	}
	// 全局 create 限流兜底
	if s.createLimiterGlobal != nil && !s.createLimiterGlobal.Allow() {
		s.sendError(client, "RATE_LIMITED", "server busy")
		return true
	}

	var code string
	var expiresAt time.Time
	var reused bool
	var sessionID string
	var reuseResult *ReuseSessionResult

	// 如果有 ClientID，尝试原子复用现有会话
	if client.ClientID != "" {
		if result, ok := s.sessions.ReuseSessionByClientID(client.ClientID, client); ok {
			// 复用成功
			code = result.Code
			expiresAt = result.ExpiresAt
			reused = true
			sessionID = result.SessionID
			reuseResult = result
			// Share 换绑后旧数据面已置 false，主动失效旧 UDP relay entry（在 SessionManager 解锁后）。
			if s.stunServer != nil {
				s.stunServer.InvalidateRelaySession(result.SessionID)
			}
			if result.OldShare != nil && result.OldShare != client && result.OldShare.Conn != nil {
				result.OldShare.Conn.Close()
			}
			log.Printf("Reusing existing session for client_fp=%s, code=%s (version: %s, host: %s)", logger.CodeFingerprint(client.ClientID), logger.MaskCode(code), client.Version, client.Host)
		}
	}

	// 如果没有复用到，生成新的
	if !reused {
		if s.config.NoAuth {
			// --no-auth 模式：使用固定 code，省掉 code 交换
			code = proto.NoAuthCode
		} else {
			var err error
			code, err = s.codes.Generate()
			if err != nil {
				s.sendError(client, "CODE_GEN_FAILED", err.Error())
				return true
			}
		}
		maxPerIP := s.limits.MaxActiveSessionsPerIP
		if s.sourceIPLimitsDisabled() {
			maxPerIP = 0
		}
		session, err := s.sessions.createPendingSession(code, client, s.config.CodeTTL, client.ClientID, host, maxPerIP, s.limits.MaxActiveSessionsTotal)
		if err != nil {
			s.sendError(client, "SESSION_LIMIT", err.Error())
			return true
		}
		expiresAt = session.ExpiresAt
		sessionID = session.ID
		logger.LogCodeGenerated(code, client.ID, session.ExpiresAt)
		log.Printf("Share client registered, code=%s (version: %s, host: %s)", logger.MaskCode(code), client.Version, client.Host)
	}

	// 注册成功，设置状态为 share
	client.Type = connStateShare

	resp := &proto.RegisterResponse{
		Code:      code,
		ExpiresAt: expiresAt.Unix(),
	}

	msg, _ := proto.NewMessage(proto.MsgRegisterResponse, resp)
	sendMsg(client, msg)
	if !s.sessions.MarkShareReady(sessionID, client.ID) {
		return true
	}

	if reuseResult != nil && reuseResult.OldHelp != nil && reuseResult.OldHelp.Conn != nil {
		// 现有 Help 客户端无法在已建立的 relay/P2P 流程中处理换代通知。
		// 注册响应发布后再要求其重新 Join，避免 SessionReady 抢在 RegisterResponse 前到达。
		s.sendError(reuseResult.OldHelp, proto.ErrCodePeerReconnected, "被协助端连接已更新，请重新加入")
		reuseResult.OldHelp.Conn.Close()
	}

	return false
}

// handleJoin 处理加入请求。执行顺序：per-IP 限流 -> 全局限流 -> JoinSession -> 失效旧
// UDP entry -> 激活数据面 -> 下发响应。任何失败都走统一出口 rejectJoin，对外不泄露内部原因。
// 返回 closeConn=true 时读循环应关闭连接（第 MaxJoinFailures 次失败或畸形）。
func (s *Server) handleJoin(client *ClientConn, code string) (closeConn bool) {
	code = normalizeCode(code)
	host := clientHost(client)

	// per-IP 限流（可旁路）：拒绝后不扣全局令牌，但仍计一次失败。
	if !s.sourceIPLimitsDisabled() && s.joinLimiterPerIP != nil && !s.joinLimiterPerIP.Allow(host) {
		return s.rejectJoin(client, code, "rate_limited_ip", true)
	}
	// 全局限流兜底。
	if s.joinLimiterGlobal != nil && !s.joinLimiterGlobal.Allow() {
		return s.rejectJoin(client, code, "rate_limited_global", true)
	}

	joinResult, err := s.sessions.JoinSession(code, client)
	if err != nil {
		return s.rejectJoin(client, code, joinRejectReason(err), false)
	}

	// 数据面换代：先失效旧 UDP relay entry，再激活新配对，最后才允许两端开始 P2P。
	// 保证「失效→激活」窗口内 relay 准入恒为 false，旧参与方后台包无法占槽。
	if s.stunServer != nil {
		s.stunServer.InvalidateRelaySession(joinResult.SessionID)
	}
	if !s.sessions.ActivateDataPlane(joinResult.SessionID, joinResult.ShareID, client.ID) {
		// 激活窗口内会话状态已变化（如 Share 又断开），回滚 Help 绑定。
		s.sessions.RollbackJoin(joinResult.SessionID, client.ID)
		return s.rejectJoin(client, code, "activate_failed", false)
	}

	// 只有真正 Join 成功才清零失败计数并进入 help 角色。
	client.joinFailures = 0
	client.Type = connStateHelp

	// JoinResponse 附带 share 端版本与身份串（已在 JoinSession 内快照，无竞态）
	resp := &proto.JoinResponse{
		Success:     true,
		SessionID:   joinResult.SessionID,
		PeerVersion: joinResult.ShareVersion,
		PeerHost:    joinResult.ShareHost,
	}
	msg, _ := proto.NewMessage(proto.MsgJoinResponse, resp)
	sendMsg(client, msg)

	// SessionReady 附带 help 端版本与身份串
	readyMsg, _ := proto.NewMessage(proto.MsgSessionReady, &proto.SessionReady{SessionID: joinResult.SessionID, PeerVersion: client.Version, PeerHost: client.Host})
	sendMsg(joinResult.Share, readyMsg)

	logger.LogSessionEstablished(joinResult.SessionID, code, client.ID, joinResult.ShareID)
	log.Printf("Session established: %s (share: %s %s, help: %s %s)", joinResult.SessionID, joinResult.ShareVersion, joinResult.ShareHost, client.Version, client.Host)
	return false
}

// rejectJoin 是所有 Join 失败（含限流拒绝）的唯一出口。对外始终返回相同的通用响应，
// 不区分内部原因，防协助码枚举；内部原因（code 指纹、来源 IP、reason 枚举）只进审计日志。
// 所有拒绝统一按 RejectAuditSampleEvery 采样写审计，避免攻击下刷爆日志。
// 累计 joinFailures，达到 MaxJoinFailures 返回 closeConn=true。
func (s *Server) rejectJoin(client *ClientConn, code, reason string, _ bool) (closeConn bool) {
	client.joinFailures++

	n := atomic.AddUint64(&s.rejectSampleCtr, 1)
	if sampleHit(n, s.limits.RejectAuditSampleEvery) {
		logger.Log(logger.AuditLevelWarn, "join_rejected", "join failed", map[string]interface{}{
			"code_fp":      logger.CodeFingerprint(code),
			"client_ip":    clientHost(client),
			"reason":       reason,
			"failures":     client.joinFailures,
			"sample_total": n,
			"sample_every": s.limits.RejectAuditSampleEvery,
		})
	}

	resp := &proto.JoinResponse{Success: false, Error: "join failed"}
	msg, _ := proto.NewMessage(proto.MsgJoinResponse, resp)
	sendMsg(client, msg)

	return client.joinFailures >= s.limits.MaxJoinFailures
}

// joinRejectReason 把内部 Join 错误映射为审计用的稳定 reason 枚举，绝不外泄给客户端。
func joinRejectReason(err error) string {
	switch err {
	case ErrCodeInvalid:
		return "code_invalid"
	case ErrCodeExpired:
		return "code_expired"
	case ErrSessionHasHelper:
		return "has_helper"
	case ErrSessionNotFound:
		return "not_found"
	default:
		return "unknown"
	}
}

// clientHost 提取客户端来源 IP（去端口），用于限流 key 与审计。
func clientHost(client *ClientConn) string {
	if client == nil || client.Conn == nil {
		return ""
	}
	addr := client.Conn.RemoteAddr()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// handlePeerAddrAdvertise 处理对等端地址通告
func (s *Server) handlePeerAddrAdvertise(client *ClientConn, msg *proto.Message) {
	var advert proto.PeerAddrAdvertise
	if err := proto.DecodePayload(msg, &advert); err != nil {
		return
	}

	// 当 STUN 失败导致公网地址为空时，使用 TCP 连接的源 IP 作为回退
	// 注意：TCP 源 IP 是正确的，但 UDP 端口未知，所以用对端的 STUN 端口（如果有）
	if advert.PublicAddr == "" && client.Conn != nil {
		remoteAddr := client.Conn.RemoteAddr()
		if host, _, err := net.SplitHostPort(remoteAddr); err == nil && host != "" {
			// 使用 TCP 源 IP + 客户端私网端口（同一 NAT 下 UDP 端口可能一致）
			_, privPort, _ := net.SplitHostPort(advert.PrivateAddr)
			if privPort != "" && privPort != "0" {
				advert.PublicAddr = net.JoinHostPort(host, privPort)
				s.logP2PSampled("Using TCP source IP as public address fallback for %s", client.ID)
			} else {
				s.logP2PSampled("STUN fallback unavailable for %s", client.ID)
			}
		}
	}

	update := s.sessions.UpdatePeerAddr(client.ID, advert.PublicAddr, advert.PrivateAddr, advert.NATType)
	if update != nil {
		s.sendPeerAddrReady(update.Peer, advert.PublicAddr, advert.PrivateAddr, update.IsShareSide, update.SameNetwork, update.NATType)
	}
}

// sendPeerAddrReady 发送对等端地址就绪消息
func (s *Server) sendPeerAddrReady(client *ClientConn, publicAddr, privateAddr string, isShare bool, sameNetwork bool, peerNATType string) {
	ready := &proto.PeerAddrReady{
		PeerPublicAddr:  publicAddr,
		PeerPrivateAddr: privateAddr,
		IsShare:         isShare,
		SameNetwork:     sameNetwork,
		PeerNATType:     peerNATType,
	}
	msg, _ := proto.NewMessage(proto.MsgPeerAddrReady, ready)
	sendMsg(client, msg)
	s.logP2PSampled("Sent peer addresses to %s", client.ID)
}

// handleTunnelData 处理隧道数据
func (s *Server) handleTunnelData(client *ClientConn, payload json.RawMessage) {
	target := s.sessions.FindPeer(client.ID)
	if target != nil {
		msg, _ := proto.NewMessage(proto.MsgTunnelData, nil)
		msg.Payload = payload
		sendMsg(target, msg)
	}
}

// sendHeartbeat 发送心跳
func (s *Server) sendHeartbeat(client *ClientConn) {
	resp := &proto.Heartbeat{Timestamp: time.Now().Unix()}
	msg, _ := proto.NewMessage(proto.MsgHeartbeat, resp)
	sendMsg(client, msg)
}

// sendError 发送错误
func (s *Server) sendError(client *ClientConn, code, message string) {
	resp := &proto.ErrorMessage{Code: code, Message: message}
	msg, _ := proto.NewMessage(proto.MsgError, resp)
	sendMsg(client, msg)
}

// sendMsg 发送消息
func sendMsg(client *ClientConn, msg *proto.Message) {
	if client == nil || client.Conn == nil {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal message: %v", err)
		return
	}
	data = append(data, '\n')
	// per-client 写锁：sendMsg 可能被多个 goroutine 并发调用（对端读循环转发、
	// 心跳 echo、P2P 地址推送…），裸 Conn.Write 并发会交错撕裂帧，对端 json 解码
	// 读到半条垃圾而丢消息。锁住「设写超时 + Write」全过程串行化。
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	// 写超时：对端慢读/半死连接撑满发送缓冲会让 Write 无限阻塞、卡死转发方读循环 goroutine
	// （写端 slowloris）。设 writeTimeout 兜底，超时按写失败关闭连接。
	if dl, ok := client.Conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = dl.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	if _, err := client.Conn.Write(data); err != nil {
		log.Printf("Write failed to %s: %v, closing connection", client.ID, err)
		client.Conn.Close()
	}
}

// cleanupLoop 定期清理
func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired := s.sessions.CleanupExpired()
			for _, id := range expired {
				// 过期清理时主动失效对应 UDP relay entry。
				if s.stunServer != nil {
					s.stunServer.InvalidateRelaySession(id)
				}
				logger.LogSessionClosed(id, "expired")
			}
			if len(expired) > 0 {
				log.Printf("Cleaned up %d expired sessions", len(expired))
			}
		}
	}
}

func generateClientID() string {
	return "cli_" + time.Now().Format("20060102150405") + "_" + randomString(6)
}

// connWrapper 包装net.Conn
type connWrapper struct {
	net.Conn
}

func (w *connWrapper) RemoteAddr() string {
	return w.Conn.RemoteAddr().String()
}
