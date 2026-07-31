package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/agent/tools"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/sysinfo"
	"github.com/remote-assist/tool/internal/version"
)

// ErrPeerDisconnected 协助端断开连接（可恢复）
var ErrPeerDisconnected = errors.New("peer disconnected")

// ShareMode 被协助模式
type ShareMode struct {
	client         *Client
	sshAddr        string
	newInstance    bool
	clientID       string
	codeFile       string
	mirrorCodeFile string
	code           string
	expiresAt      time.Time
	sbCfg          agent.SandboxConfig
	daemon         *agent.Daemon
	daemonOnce     sync.Once
}

// NewShareMode 创建被协助模式。codeFile 非空时，注册成功后把协助码与有效期
// 以 JSON 原子写入该文件，供宿主程序（如管家）稳定读取，无需解析 stdout。
// mirrorCodeFile 非空时再额外写一份到该路径：通道内热升级会用它，让 old share
// 原来的 --code-file 路径在升级后仍由 new share 继续刷新（见 upgradeflags.HostCodeFile）。
// newInstance 为 true 时使用进程内 ClientID，允许 CLI 额外启动独立 share。
func NewShareMode(cfg *Config, sshAddr string, newInstance bool, sbCfg agent.SandboxConfig, codeFile, mirrorCodeFile string) *ShareMode {
	share := &ShareMode{
		client:         NewClient(cfg),
		sshAddr:        sshAddr,
		newInstance:    newInstance,
		codeFile:       codeFile,
		mirrorCodeFile: mirrorCodeFile,
		sbCfg:          sbCfg,
	}
	if newInstance {
		share.clientID, _ = generateClientID()
	}
	return share
}

// Run 运行被协助模式，协助端断开时自动等待新连接
func (s *ShareMode) Run() (string, time.Time, error) {
	defer s.client.Close()

	// 持久守护：连接/注册/隧道任何环节出错（EOF、relay 重启、网络抖动、协助码过期…）
	// 都【不退出】，一律退避重连重注册，只能由信号（Ctrl+C / systemctl stop）终止。
	// 初次连接+注册也走无限退避：relay 暂时不可达不应让 share 直接退出。
	s.reconnectWithBackoff()

	// rapidFails：连续"建立即断"的会话数，用于防热循环退避（见 rapidReconnectBackoff）。
	rapidFails := 0
	for {
		start := time.Now()
		tunnelErr := s.waitAndHandleTunnel()
		sessionDur := time.Since(start)

		isPeerDisconnect := errors.Is(tunnelErr, ErrPeerDisconnected)
		codeExpired := time.Now().After(s.expiresAt)

		// 对端断开 + 协助码仍有效 → 在当前连接上等待新的协助端（不重连不退出）
		if isPeerDisconnect && !codeExpired {
			rapidFails = 0
			fmt.Printf("\n协助端已断开连接，协助码仍有效: %s\n", formatCode(s.code))
			fmt.Println("等待新的协助端连接...")
			continue
		}

		// 其余一切情况都【不退出】：含 EOF/隧道正常关闭(tunnelErr==nil，多为 relay 重启
		// 或网络中断)、连接异常、协助码过期。统一退避重连重注册。
		switch {
		case tunnelErr == nil:
			fmt.Println("\n隧道已关闭（relay 重启或网络中断），正在重连...")
		case codeExpired:
			fmt.Println("\n连接中断且协助码已过期，正在重连获取新协助码...")
		default:
			fmt.Printf("\n连接中断: %v，协助码仍有效: %s，正在重连...\n", tunnelErr, formatCode(s.code))
		}

		// 防热循环：若隧道"建立后极短时间内即断"，reconnectWithBackoff 可能每次第 1 次
		// 就成功（不退避），连续多次就变成无间隔的疯狂重连、空转 CPU + 锤 relay。这里按
		// 连续快速失败次数线性退避；正常时长的会话重置计数、立即重连（不拖慢正常恢复）。
		var floor time.Duration
		rapidFails, floor = rapidReconnectBackoff(sessionDur, rapidFails)
		if floor > 0 {
			fmt.Printf("（隧道存活仅 %v，连续 %d 次快速断开，%v 后再重连以防空转）\n",
				sessionDur.Round(time.Millisecond), rapidFails, floor)
			time.Sleep(floor)
		}
		s.reconnectWithBackoff()
	}
}

// 重连退避参数
const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
	// rapidReconnectFloor 判定"建立即断"的会话时长阈值：隧道存活短于它即认为陷入
	// 快速失败循环，触发防热循环退避（见 rapidReconnectBackoff）。
	rapidReconnectFloor = 2 * time.Second
)

// reconnectWithBackoff 连接 relay 并注册（register 会按 ClientID 复用会话，或在协助码
// 过期时换新码）。失败按指数退避（1s,2s,4s,…，封顶 30s）【无限重试，永不放弃】——
// share 是持久守护进程，relay 重启 / 弱网瞬断 / 协助码过期都不应让它退出，只能由信号
// 终止。也用于初次连接+注册。
func (s *ShareMode) reconnectWithBackoff() {
	delay := reconnectBaseDelay
	for attempt := 1; ; attempt++ {
		s.client.Close()
		err := s.client.Connect()
		if err == nil {
			err = s.register()
		}
		if err == nil {
			if attempt > 1 {
				fmt.Printf("重连成功（第 %d 次尝试）\n", attempt)
			}
			// 心跳保活绑定 relay 连接生命周期：register 成功即启动，覆盖「等待协助端 /
			// relay 中转 / P2P 转发」全程。relay 对每个 client 有 readIdleTimeout(2min)，
			// 任一阶段静默不发消息都会被判掉线 → DisconnectClient 置 session.Share=nil →
			// 协助端无法 join（standalone 下还会自连内嵌 relay 每 2min 断一次、刷屏重连）。
			// 上一连接的心跳 goroutine 会在 Close() 后经 IsClosed() 退出，故每连接只多一个。
			s.client.StartHeartbeatLoop(30 * time.Second)
			return
		}
		fmt.Printf("连接失败(第 %d 次): %v；%s 后重试...\n", attempt, err, delay)
		time.Sleep(delay)
		delay *= 2
		if delay > reconnectMaxDelay {
			delay = reconnectMaxDelay
		}
	}
}

// rapidReconnectBackoff 根据上一会话存活时长与连续快速失败次数，决定重连前的退避。
// 会话存活 ≥ rapidReconnectFloor（正常时长）→ 重置计数、不退避，保证正常断连快速恢复；
// 短于阈值（建立即断）→ 计数 +1，按次数线性退避（1s,2s,…，封顶 reconnectMaxDelay），
// 避免无退避地疯狂重连。返回新计数与应 sleep 的时长。
func rapidReconnectBackoff(sessionDur time.Duration, rapidFails int) (int, time.Duration) {
	if sessionDur >= rapidReconnectFloor {
		return 0, 0
	}
	rapidFails++
	delay := time.Duration(rapidFails) * time.Second
	if delay > reconnectMaxDelay {
		delay = reconnectMaxDelay
	}
	return rapidFails, delay
}

// register 向 relay 注册并获取协助码
func (s *ShareMode) register() error {
	clientID, err := s.registrationClientID()
	if err != nil {
		return err
	}
	hostInfo := sysinfo.Summary()
	if err := s.client.SendMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{ClientID: clientID, Version: version.Info(), Host: hostInfo}); err != nil {
		return err
	}

	msg, err := s.client.ReadMessage()
	if err != nil {
		return err
	}

	if msg.Type != proto.MsgRegisterResponse {
		return fmt.Errorf("unexpected response: %s", msg.Type)
	}

	var resp proto.RegisterResponse
	if err := proto.DecodePayload(msg, &resp); err != nil {
		return err
	}
	s.code = resp.Code
	s.expiresAt = time.Unix(resp.ExpiresAt, 0)
	fmt.Printf("\n协助码: %s\n", formatCode(resp.Code))
	fmt.Printf("本机标识: %s\n", hostInfo)
	fmt.Printf("中转服务: %s\n", relayDesc(s.client.config))
	fmt.Printf("有效期至: %s\n\n", s.expiresAt.Local().Format("2006-01-02 15:04:05"))
	// 复制到系统剪贴板，方便直接粘贴给协助端（尽力而为：失败静默，不影响协助流程）。
	// 首次注册与重连刷新 code 都走到这里，剪贴板始终是最新协助码。
	if err := copyToClipboard(formatCode(resp.Code)); err == nil {
		fmt.Println("（协助码已复制到剪贴板）")
	}
	fmt.Println("等待协助端连接...")
	s.writeCodeFile()
	return nil
}

func (s *ShareMode) registrationClientID() (string, error) {
	if s.newInstance {
		return s.clientID, nil
	}
	return GetOrCreateClientID()
}

// writeCodeFile 把协助码、中转服务地址与有效期原子写入 codeFile（先写 .tmp 再 rename），
// 供宿主程序读取。失败仅记日志，不影响协助流程。重连刷新 code 时会覆盖写入。
// mirrorCodeFile 非空时再写一份（升级后保持 old 原路径继续刷新）。
//
// server 必须一起给：光有协助码，协助端并不知道该去哪台 relay 找它——而实际连的那台是
// 「编译期默认值 → REMOTE_RELAY_SERVER → --server → 补默认端口 → --standalone 改写」
// 一路算出来的，宿主程序无从推断（standalone 下更是跟命令行上写的完全不是一回事）。
func (s *ShareMode) writeCodeFile() {
	payload := struct {
		Code      string `json:"code"`
		Server    string `json:"server"`
		ExpiresAt int64  `json:"expiresAt"`
	}{Code: s.code, Server: s.client.config.ServerAddr, ExpiresAt: s.expiresAt.Unix()}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("写协助码文件失败(marshal): %v", err)
		return
	}

	for _, path := range []string{s.codeFile, s.mirrorCodeFile} {
		if path != "" {
			writeCodeFileTo(path, data)
		}
	}
}

// writeCodeFileTo 原子写单个协助码文件（.tmp + rename，0600）。失败仅记日志。
func writeCodeFileTo(path string, data []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("写协助码文件失败(write %s): %v", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("写协助码文件失败(rename %s): %v", path, err)
		os.Remove(tmp)
	}
}

// waitAndHandleTunnel 等待协助端连接，然后处理隧道
func (s *ShareMode) waitAndHandleTunnel() error {
	sessionID, err := s.waitSessionReady()
	if err != nil {
		return err
	}

	// 尝试 P2P 直连
	p2pMode := p2p.ParseP2PMode(s.client.config.P2PMode)
	if p2pMode != p2p.P2PModeDisabled {
		tunnel, err := s.negotiateP2P(p2pMode, sessionID)
		if err != nil {
			if p2pMode == p2p.P2PModeRequired {
				return fmt.Errorf("P2P 连接失败: %w", err)
			}
			log.Printf("P2P negotiation failed, falling back to relay: %v", err)
		}
		if tunnel != nil {
			fmt.Println("开始 P2P 直连转发SSH流量...")
			return s.handleTunnelP2P(tunnel)
		}
	}

	s.client.ResetDecoder() // P2P 协商超时会导致 json.Decoder 缓存错误
	fmt.Println("开始中转转发SSH流量...")
	return s.handleTunnel()
}

// waitSessionReady 等待会话就绪
func (s *ShareMode) waitSessionReady() (string, error) {
	for {
		msg, err := s.client.ReadMessage()
		if err != nil {
			return "", err
		}

		switch msg.Type {
		case proto.MsgSessionReady:
			var ready proto.SessionReady
			if err := proto.DecodePayload(msg, &ready); err != nil {
				return "", err
			}
			fmt.Println("协助端已连接！")
			if ready.PeerVersion != "" {
				fmt.Printf("对端版本: %s\n", ready.PeerVersion)
			}
			if ready.PeerHost != "" {
				fmt.Printf("对端标识: %s\n", ready.PeerHost)
			}
			return ready.SessionID, nil
		case proto.MsgHeartbeat:
		case proto.MsgToolHello, proto.MsgToolReq, proto.MsgToolCancel:
			// 会话重同步窗口（协助端重连 / session 切换）可能收到工具通道消息。
			// 旧实现把它们当 "Unexpected message" 丢弃，导致对应 tool_req 永远收不到
			// ToolResp、help 端 CallTool 干等兜底超时。这里投递给 daemon（若在运行）。
			if s.daemon != nil {
				s.daemon.Inject(msg)
			}
		case proto.MsgError:
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			return "", fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		default:
			log.Printf("Unexpected message: %s", msg.Type)
		}
	}
}

// negotiateP2P 尝试 P2P 直连协商
func (s *ShareMode) negotiateP2P(mode p2p.P2PMode, sessionID string) (*p2p.UDPTunnel, error) {
	mgr := p2p.NewP2PManager(mode, s.client.config.STUNServer, s.client.config.BindIP)
	mgr.SetRelayConn(s.client)

	resultCh, err := mgr.Start(sessionID, true)
	if err != nil {
		return nil, err
	}

	select {
	case result := <-resultCh:
		return result.Tunnel, result.Err
	default:
	}

	fmt.Println("正在尝试 P2P 直连...")

	negotiationTimeout := 12 * time.Second
	if mode == p2p.P2PModeRequired {
		negotiationTimeout = 32 * time.Second
	}
	s.client.SetReadDeadline(time.Now().Add(negotiationTimeout))

	peerReady := false
	for !peerReady {
		msg, err := s.client.ReadMessage()
		if err != nil {
			s.client.SetReadDeadline(time.Time{})
			if isNetTimeout(err) {
				mgr.Close()
				if mode == p2p.P2PModeRequired {
					return nil, fmt.Errorf("P2P 协商超时：对端未响应")
				}
				fmt.Println("P2P 协商超时，回退到中转模式")
				return nil, nil
			}
			mgr.Close()
			return nil, err
		}
		switch msg.Type {
		case proto.MsgPeerAddrReady:
			var ready proto.PeerAddrReady
			if err := proto.DecodePayload(msg, &ready); err == nil {
				mgr.HandlePeerAddrReady(&ready)
			}
			peerReady = true
		case proto.MsgHeartbeat:
		case proto.MsgToolHello:
			// 工具通道握手可能在 P2P 协商窗口期到达，必须就地处理否则消息会被丢
			var hello proto.Hello
			proto.DecodePayload(msg, &hello)
			ack, key := buildHelloAck(hello, s.code)
			s.client.SendMessage(proto.MsgToolHelloAck, &ack)
			if ack.Accept {
				s.ensureDaemon(key)
			}
		case proto.MsgToolReq, proto.MsgToolCancel:
			if s.daemon != nil {
				s.daemon.Inject(msg)
			}
		case proto.MsgError:
			s.client.SetReadDeadline(time.Time{})
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			mgr.Close()
			return nil, fmt.Errorf("server error: %s", errMsg.Message)
		}
	}

	s.client.SetReadDeadline(time.Time{})

	result := <-resultCh
	if result.Tunnel != nil {
		fmt.Println("P2P 直连已建立！")
	} else if result.Err == nil {
		fmt.Println("P2P 打洞超时，回退到中转模式")
		mgr.Close()
	} else {
		mgr.Close()
	}
	return result.Tunnel, result.Err
}

// handleTunnelP2P 通过 P2P 隧道处理流量（SSH 或 MCP 工具通道）。
// 通过首 2 字节判断模式：[0x00,'T'] = 工具通道，其他 = SSH（原行为）。
func (s *ShareMode) handleTunnelP2P(tunnel *p2p.UDPTunnel) error {
	// Detect mode from first bytes on tunnel
	toolMode, consumed, err := ReadModeHeader(tunnel)
	if err != nil {
		tunnel.Close()
		return ErrPeerDisconnected
	}
	if toolMode {
		log.Printf("P2P tunnel: tool mode detected, starting tool-over-P2P")
		return s.handleToolOverP2P(tunnel)
	}
	// SSH mode: the consumed bytes are part of the SSH stream, write them
	// into the first SSH read iteration (handled by prefixing to the buffer)
	log.Printf("P2P tunnel: SSH mode (first bytes: %x)", consumed)
	return s.handleSSHTunnelP2P(tunnel, consumed[:])
}

// handleToolOverP2P runs the MCP tool channel over a P2P tunnel.
// The help side has already sent the tool-mode header; next will be ToolHello.
func (s *ShareMode) handleToolOverP2P(tunnel *p2p.UDPTunnel) error {
	defer tunnel.Close()

	pc := NewP2PConn(tunnel)

	// NOTE: no heartbeat start here — handleTunnel (relay path) already started
	// it during the P2P negotiation window if tool messages arrived, and
	// waitAndHandleTunnel's caller handles relay keepalive. Avoid double-start.

	// Tool message loop over P2P — mirrors handleTunnel's tool message handling
	for {
		msg, err := pc.ReadMessage()
		if err != nil {
			log.Printf("P2P tool channel read error: %v", err)
			return ErrPeerDisconnected
		}
		switch msg.Type {
		case proto.MsgToolHello:
			var hello proto.Hello
			proto.DecodePayload(msg, &hello)
			ack, key := buildHelloAck(hello, s.code)
			pc.SendMessage(proto.MsgToolHelloAck, &ack)
			if ack.Accept {
				// Reuse existing daemon (if relay handshake created one) by
				// swapping its conn to P2PConn. Otherwise create a fresh one.
				s.ensureDaemon(key)
				s.daemon.SwapConn(pc, key)
			}
		case proto.MsgToolReq, proto.MsgToolCancel:
			if s.daemon != nil {
				s.daemon.Inject(msg)
			}
		case proto.MsgHeartbeat:
			// ignore
		}
	}
}

// handleSSHTunnelP2P handles SSH-over-P2P (original behavior), with the first
// consumed bytes from mode detection prepended to the SSH stream.
func (s *ShareMode) handleSSHTunnelP2P(tunnel *p2p.UDPTunnel, prefix []byte) error {
	defer tunnel.Close()

	var sshConn net.Conn
	var connMu sync.Mutex

	connectSSH := func() net.Conn {
		connMu.Lock()
		defer connMu.Unlock()

		if sshConn != nil {
			return sshConn
		}

		conn, err := net.Dial("tcp", s.sshAddr)
		if err != nil {
			log.Printf("Failed to connect to local SSH: %v", err)
			return nil
		}
		sshConn = conn
		fmt.Println("已连接到本地SSH服务 (P2P直连)...")

		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					break
				}
				if _, err := tunnel.Write(buf[:n]); err != nil {
					break
				}
			}
			connMu.Lock()
			if sshConn == conn {
				sshConn = nil
			}
			connMu.Unlock()
			conn.Close()
			fmt.Println("本地SSH连接已断开，等待新的SSH会话...")
		}()

		return conn
	}

	// prefixPending holds the 2 bytes consumed during mode detection. They
	// are prepended to the very first tunnel.Read payload so both go to the
	// same SSH connection in one Write call (no split / lost-prefix risk).
	prefixPending := prefix

	buf := make([]byte, 32*1024)
	for {
		n, err := tunnel.Read(buf)
		if err != nil {
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			// P2P 隧道断开，视为协助端断开
			return ErrPeerDisconnected
		}
		if n == 0 {
			continue
		}

		// Merge prefix (mode-header bytes) with first payload
		data := buf[:n]
		if len(prefixPending) > 0 {
			merged := make([]byte, len(prefixPending)+n)
			copy(merged, prefixPending)
			copy(merged[len(prefixPending):], buf[:n])
			data = merged
			prefixPending = nil
		}

		conn := connectSSH()
		if conn != nil {
			if _, err := conn.Write(data); err != nil {
				connMu.Lock()
				if sshConn == conn {
					sshConn = nil
				}
				connMu.Unlock()
				conn.Close()
			}
		}
	}
}

// handleTunnel 处理隧道（支持多次SSH连接）
func (s *ShareMode) handleTunnel() error {
	// 心跳已由 reconnectWithBackoff 在连接建立时启动（覆盖等待/中转/P2P 全生命周期），此处不再重复

	// Shared state: current SSH connection to local SSH server
	var sshConn net.Conn
	var connMu sync.Mutex

	// connectSSH lazily connects to local SSH and starts a reader goroutine
	connectSSH := func() net.Conn {
		connMu.Lock()
		defer connMu.Unlock()

		if sshConn != nil {
			return sshConn
		}

		conn, err := net.Dial("tcp", s.sshAddr)
		if err != nil {
			log.Printf("Failed to connect to local SSH: %v", err)
			return nil
		}
		sshConn = conn
		fmt.Println("已连接到本地SSH服务...")

		// Start SSH → relay goroutine
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					break
				}
				if err := s.client.SendMessage(proto.MsgTunnelData, &proto.TunnelData{Data: buf[:n]}); err != nil {
					break
				}
			}
			connMu.Lock()
			if sshConn == conn {
				sshConn = nil
			}
			connMu.Unlock()
			conn.Close()
			fmt.Println("本地SSH连接已断开，等待新的SSH会话...")
		}()

		return conn
	}

	// Main loop: read from relay, forward to SSH (connecting on demand)
	for {
		// Set read timeout to detect dead connections (reset after each successful read)
		s.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
		msg, err := s.client.ReadMessage()
		if err != nil {
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			if err.Error() == "EOF" {
				return nil
			}
			return err
		}

		switch msg.Type {
		case proto.MsgToolHello:
			var hello proto.Hello
			proto.DecodePayload(msg, &hello)
			ack, key := buildHelloAck(hello, s.code)
			s.client.SendMessage(proto.MsgToolHelloAck, &ack)
			if ack.Accept {
				s.ensureDaemon(key)
			}
		case proto.MsgToolReq, proto.MsgToolCancel:
			if s.daemon != nil {
				s.daemon.Inject(msg)
			}
		case proto.MsgTunnelData:
			var dataMsg proto.TunnelData
			if err := json.Unmarshal(msg.Payload, &dataMsg); err != nil {
				continue
			}
			conn := connectSSH()
			if conn != nil {
				if _, err := conn.Write(dataMsg.Data); err != nil {
					connMu.Lock()
					if sshConn == conn {
						sshConn = nil
					}
					connMu.Unlock()
					conn.Close()
				}
			}
		case proto.MsgHeartbeat:
			// ignore
		case proto.MsgError:
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			connMu.Lock()
			if sshConn != nil {
				sshConn.Close()
			}
			connMu.Unlock()
			if errMsg.Code == "PEER_DISCONNECTED" {
				return ErrPeerDisconnected
			}
			return fmt.Errorf("server error: %s - %s", errMsg.Code, errMsg.Message)
		}
	}
}

// GetCode 获取协助码
func (s *ShareMode) GetCode() string {
	return s.code
}

// GetExpiresAt 获取过期时间
func (s *ShareMode) GetExpiresAt() time.Time {
	return s.expiresAt
}

func formatCode(code string) string {
	if len(code) < 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// daemonSink share 端把 Tool 消息转给 agent.Daemon 的契约
type daemonSink interface {
	Inject(msg *proto.Message)
}

// dispatchToolMessage 若 msg 属于工具通道则投递并返回 true，否则 false。
// 注意：Task 13 仅提供 helper；Task 17 才在 handleTunnel 的 dispatch 循环中调用并启动 daemon。
func dispatchToolMessage(msg *proto.Message, d daemonSink) bool {
	switch msg.Type {
	case proto.MsgToolReq, proto.MsgToolCancel, proto.MsgToolHello:
		if d != nil {
			d.Inject(msg)
		}
		return true
	}
	return false
}

// buildHelloAck share 端对 Hello 的应答 + 派生 session_key
func buildHelloAck(hello proto.Hello, code string) (proto.HelloAck, [32]byte) {
	if hello.Version != proto.ToolProtocolVersion {
		return proto.HelloAck{
			Version:  proto.ToolProtocolVersion,
			Accept:   false,
			ErrorMsg: "unsupported tool protocol version: " + hello.Version,
		}, [32]byte{}
	}
	ack := proto.HelloAck{
		Version:      proto.ToolProtocolVersion,
		Capabilities: []string{"exec", "read_file", "write_file", "list_dir", "stat", "glob", "grep", "process_list", "tail_log"},
		NonceB64:     proto.NewHello().NonceB64,
		Accept:       true,
	}
	key := proto.DeriveSessionKey(code, ack.NonceB64, hello.NonceB64)
	return ack, key
}

// ensureDaemon 首次 hello 时构造 daemon；后续 hello 仅 rotate key。
// sync.Once 保证 reg/goroutine 只初始化一次；每次 hello 后都用最新 key 覆盖。
func (s *ShareMode) ensureDaemon(key [32]byte) {
	s.daemonOnce.Do(func() {
		reg := agent.NewRegistry()
		sb := agent.NewSandbox(s.sbCfg)
		reg.Register(tools.NewExec(sb))
		reg.Register(tools.NewReadFile(sb))
		reg.Register(tools.NewWriteFile(sb))
		reg.Register(tools.NewFileMD5(sb))
		reg.Register(tools.NewListDir(sb))
		reg.Register(tools.NewStat(sb))
		reg.Register(tools.NewGlob(sb))
		reg.Register(tools.NewGrep(sb))
		reg.Register(tools.NewProcessList())
		reg.Register(tools.NewTailLog(sb))
		d := agent.NewDaemon(reg, s.client, key)
		d.OnActivity = func(line string) { fmt.Println(line) }
		s.daemon = d
		go d.RunLoop(context.Background())
	})
	// 不管是首次还是续连，都用最新 key 覆盖（首次 RotateKey 等同于设置已有 key，无害）
	s.daemon.RotateKey(key)
}
