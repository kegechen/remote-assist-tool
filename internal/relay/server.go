package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/logger"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
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
}

// Server 中转服务器
type Server struct {
	config      *Config
	sessions    *SessionManager
	codes       *CodeManager
	clients     map[string]*ClientConn
	clientsMu   sync.RWMutex
	stunServer  *p2p.STUNServer
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

	return &Server{
		config:   cfg,
		sessions: NewSessionManager(),
		codes:    NewCodeManager(cfg.CodeLength),
		clients:  make(map[string]*ClientConn),
	}, nil
}

// Start starts the server (backward compatible)
func (s *Server) Start() error {
	return s.StartWithContext(context.Background())
}

// StartWithContext starts the server with context for graceful shutdown
func (s *Server) StartWithContext(ctx context.Context) error {
	// Start STUN server if configured
	if s.config.STUNListenAddr != "" {
		var err error
		s.stunServer, err = p2p.NewSTUNServer(s.config.STUNListenAddr)
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
	go s.cleanupLoop(ctx)

	// Close listener when context is cancelled
	go func() {
		<-ctx.Done()
		log.Printf("Shutting down server...")
		listener.Close()
	}()

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

// handleConn 处理连接
func (s *Server) handleConn(conn net.Conn) {
	clientID := generateClientID()
	clientIP := conn.RemoteAddr().String()

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
		if result != nil && result.PeerToNotify != nil {
			s.sendError(result.PeerToNotify, "PEER_DISCONNECTED", "被协助端已断开连接")
		}
		conn.Close()
		log.Printf("Connection closed: %s", clientID)
	}()

	// 读循环
	dec := json.NewDecoder(conn)
	for {
		var msg proto.Message
		if err := dec.Decode(&msg); err != nil {
			if err != io.EOF {
				log.Printf("Read error: %v", err)
			}
			return
		}
		s.handleMessage(client, &msg)
	}
}

// handleMessage 处理消息
func (s *Server) handleMessage(client *ClientConn, msg *proto.Message) {
	switch msg.Type {
	case proto.MsgRegisterRequest:
		var req proto.RegisterRequest
		if proto.DecodePayload(msg, &req) == nil {
			client.ClientID = req.ClientID
			client.Version = req.Version
		}
		s.handleRegister(client)
	case proto.MsgJoinRequest:
		var req proto.JoinRequest
		if err := proto.DecodePayload(msg, &req); err == nil {
			client.Version = req.Version
			s.handleJoin(client, req.Code)
		}
	case proto.MsgTunnelData:
		s.handleTunnelData(client, msg.Payload)
	case proto.MsgHeartbeat:
		s.sendHeartbeat(client)
	case proto.MsgPeerAddrAdvertise:
		s.handlePeerAddrAdvertise(client, msg)
	case proto.MsgP2PConnected:
		log.Printf("Client %s reports P2P connected", client.ID)
	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// handleRegister 处理注册请求
func (s *Server) handleRegister(client *ClientConn) {
	client.Type = "share"

	var code string
	var expiresAt time.Time
	var reused bool

	// 如果有 ClientID，尝试复用现有会话
	if client.ClientID != "" {
		if existingSession, ok := s.sessions.GetSessionByClientID(client.ClientID); ok {
			// 复用现有会话
			code = existingSession.Code
			expiresAt = existingSession.ExpiresAt
			s.sessions.ReuseSession(existingSession, client)
			reused = true
			log.Printf("Reusing existing session for client %s, code: %s (version: %s)", client.ClientID, FormatCode(code), client.Version)
		}
	}

	// 如果没有复用到，生成新的
	if !reused {
		var err error
		code, err = s.codes.Generate()
		if err != nil {
			s.sendError(client, "CODE_GEN_FAILED", err.Error())
			return
		}
		session := s.sessions.CreateSession(code, client, s.config.CodeTTL, client.ClientID)
		expiresAt = session.ExpiresAt
		logger.LogCodeGenerated(code, client.ID, session.ExpiresAt)
		log.Printf("Share client registered, code: %s (version: %s)", FormatCode(code), client.Version)
	}

	resp := &proto.RegisterResponse{
		Code:      code,
		ExpiresAt: expiresAt.Unix(),
	}

	msg, _ := proto.NewMessage(proto.MsgRegisterResponse, resp)
	sendMsg(client, msg)
}

// handleJoin 处理加入请求
func (s *Server) handleJoin(client *ClientConn, code string) {
	code = normalizeCode(code)

	session, err := s.sessions.JoinSession(code, client)
	if err != nil {
		resp := &proto.JoinResponse{
			Success: false,
			Error:   err.Error(),
		}
		msg, _ := proto.NewMessage(proto.MsgJoinResponse, resp)
		sendMsg(client, msg)
		return
	}

	client.Type = "help"

	// Snapshot Share into local var to avoid TOCTOU race with DisconnectClient
	share := session.Share
	if share == nil {
		resp := &proto.JoinResponse{
			Success: false,
			Error:   "share client disconnected",
		}
		msg, _ := proto.NewMessage(proto.MsgJoinResponse, resp)
		sendMsg(client, msg)
		return
	}

	// JoinResponse 附带 share 端版本
	resp := &proto.JoinResponse{
		Success:     true,
		SessionID:   session.ID,
		PeerVersion: share.Version,
	}
	msg, _ := proto.NewMessage(proto.MsgJoinResponse, resp)
	sendMsg(client, msg)

	// SessionReady 附带 help 端版本
	readyMsg, _ := proto.NewMessage(proto.MsgSessionReady, &proto.SessionReady{SessionID: session.ID, PeerVersion: client.Version})
	sendMsg(share, readyMsg)

	logger.LogSessionEstablished(session.ID, code, client.ID, share.ID)
	log.Printf("Session established: %s (share version: %s, help version: %s)", session.ID, share.Version, client.Version)
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
				log.Printf("Using TCP source IP as public address fallback for %s: %s (port from private addr)", client.ID, advert.PublicAddr)
			} else {
				log.Printf("STUN failed for %s, TCP source IP=%s but no usable port, skipping public addr fallback", client.ID, host)
			}
		}
	}

	log.Printf("Received peer address from %s: public=%s, private=%s", client.ID, advert.PublicAddr, advert.PrivateAddr)

	update := s.sessions.UpdatePeerAddr(client.ID, advert.PublicAddr, advert.PrivateAddr)
	if update != nil {
		s.sendPeerAddrReady(update.Peer, advert.PublicAddr, advert.PrivateAddr, update.IsShareSide, update.SameNetwork)
	}
}

// sendPeerAddrReady 发送对等端地址就绪消息
func (s *Server) sendPeerAddrReady(client *ClientConn, publicAddr, privateAddr string, isShare bool, sameNetwork bool) {
	ready := &proto.PeerAddrReady{
		PeerPublicAddr:  publicAddr,
		PeerPrivateAddr: privateAddr,
		IsShare:         isShare,
		SameNetwork:     sameNetwork,
	}
	msg, _ := proto.NewMessage(proto.MsgPeerAddrReady, ready)
	sendMsg(client, msg)
	log.Printf("Sent peer addresses to %s", client.ID)
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
