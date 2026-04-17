package relay

import (
	"crypto/rand"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"sync"
	"time"
)

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrCodeExpired        = errors.New("code expired")
	ErrCodeInvalid        = errors.New("invalid code")
	ErrSessionHasHelper   = errors.New("session already has helper")
)

// Conn 连接接口
type Conn interface {
	io.ReadWriteCloser
	RemoteAddr() string
}

// ClientConn 客户端连接
type ClientConn struct {
	ID       string
	Type     string // "share" or "help"
	ClientID string // 持久化客户端ID
	Version  string // 客户端版本
	Conn     Conn
	Send     chan []byte
}

// TunnelSession 隧道会话
type TunnelSession struct {
	ID               string
	Code             string
	Share            *ClientConn
	Help             *ClientConn
	CreatedAt        time.Time
	ExpiresAt        time.Time
	ClientID         string // 持久化客户端ID
	SharePublicAddr  string // Share 端公网地址
	SharePrivateAddr string // Share 端内网地址
	ShareNATType     string // Share 端 NAT 类型
	HelpPublicAddr   string // Help 端公网地址
	HelpPrivateAddr  string // Help 端内网地址
	HelpNATType      string // Help 端 NAT 类型
	P2PEnabled       bool   // 是否启用 P2P
	closed           bool
	mu               sync.Mutex

	// Help 断连去抖
	helpDisconnectTimer *time.Timer // Help 断连延迟计时器
	pendingHelpID       string      // 待断连的 Help ID
}

// SessionManager 会话管理器
type SessionManager struct {
	sessions    map[string]*TunnelSession
	byCode      map[string]*TunnelSession
	byClientID  map[string]*TunnelSession
	mu          sync.RWMutex
}

// NewSessionManager 创建会话管理器
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions:   make(map[string]*TunnelSession),
		byCode:     make(map[string]*TunnelSession),
		byClientID: make(map[string]*TunnelSession),
	}
}

// CreateSession 创建会话
func (sm *SessionManager) CreateSession(code string, share *ClientConn, ttl time.Duration, clientID string) *TunnelSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	session := &TunnelSession{
		ID:        generateSessionID(),
		Code:      code,
		Share:     share,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		ClientID:  clientID,
	}

	sm.sessions[session.ID] = session
	sm.byCode[code] = session
	if clientID != "" {
		sm.byClientID[clientID] = session
	}
	return session
}

// ReuseSession 复用现有会话（更新Share客户端连接）
func (sm *SessionManager) ReuseSession(session *TunnelSession, newShare *ClientConn) *TunnelSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 关闭旧连接
	if session.Share != nil && session.Share.Conn != nil {
		session.Share.Conn.Close()
	}

	// 更新Share客户端
	session.Share = newShare
	session.closed = false

	return session
}

// GetSessionByCode 通过协助码获取会话
func (sm *SessionManager) GetSessionByCode(code string) (*TunnelSession, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, exists := sm.byCode[code]
	if !exists {
		return nil, ErrCodeInvalid
	}
	if time.Now().After(session.ExpiresAt) {
		return nil, ErrCodeExpired
	}
	if session.closed {
		return nil, ErrSessionNotFound
	}
	return session, nil
}

// GetSessionByClientID 通过客户端ID获取未过期的会话
func (sm *SessionManager) GetSessionByClientID(clientID string) (*TunnelSession, bool) {
	if clientID == "" {
		return nil, false
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, exists := sm.byClientID[clientID]
	if !exists {
		return nil, false
	}
	if time.Now().After(session.ExpiresAt) || session.closed {
		return nil, false
	}
	return session, true
}

// JoinSession 协助端加入会话
func (sm *SessionManager) JoinSession(code string, help *ClientConn) (*TunnelSession, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.byCode[code]
	if !exists {
		return nil, ErrCodeInvalid
	}
	if time.Now().After(session.ExpiresAt) {
		return nil, ErrCodeExpired
	}
	if session.closed {
		return nil, ErrSessionNotFound
	}
	// 如果当前 Help 正在 pending 断连中，允许新 Help 替换
	if session.Help != nil && session.pendingHelpID == "" {
		return nil, ErrSessionHasHelper
	}
	if session.Share == nil {
		return nil, ErrSessionNotFound
	}

	// 取消挂起的断连计时器
	if session.helpDisconnectTimer != nil {
		session.helpDisconnectTimer.Stop()
		session.helpDisconnectTimer = nil
		session.pendingHelpID = ""
		log.Printf("Help disconnect debounce cancelled for session %s (new help joined)", session.ID)
	}

	session.Help = help
	return session, nil
}

// CloseSession 关闭会话
func (sm *SessionManager) CloseSession(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if session, exists := sm.sessions[sessionID]; exists {
		session.mu.Lock()
		session.closed = true
		session.mu.Unlock()

		if session.Share != nil && session.Share.Conn != nil {
			session.Share.Conn.Close()
		}
		if session.Help != nil && session.Help.Conn != nil {
			session.Help.Conn.Close()
		}

		delete(sm.sessions, sessionID)
		delete(sm.byCode, session.Code)
		if session.ClientID != "" {
			delete(sm.byClientID, session.ClientID)
		}
	}
}

// GetActiveSessions 获取活跃会话数
func (sm *SessionManager) GetActiveSessions() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// DisconnectResult contains info about a disconnected client's session
type DisconnectResult struct {
	PeerToNotify *ClientConn // The other side to notify (if any)
	SessionID    string
	WasShare     bool // true if the disconnected client was the share side
}

// DisconnectClient clears a client from its session when the connection drops.
// For help clients: clears session.Help so a new helper can rejoin.
// For share clients: returns the help client so the server can notify it.
func (sm *SessionManager) DisconnectClient(clientID string) *DisconnectResult {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, session := range sm.sessions {
		if session.Help != nil && session.Help.ID == clientID {
			// Help 断连去抖：延迟 5 秒再清除，防止网络抖动导致不必要的重连
			session.pendingHelpID = clientID
			session.helpDisconnectTimer = time.AfterFunc(5*time.Second, func() {
				sm.mu.Lock()
				defer sm.mu.Unlock()
				if session.Help != nil && session.Help.ID == session.pendingHelpID {
					session.Help = nil
					session.pendingHelpID = ""
					log.Printf("Cleared helper from session %s (after debounce)", session.ID)
				}
			})
			log.Printf("Help disconnect debounce started for session %s (5s)", session.ID)
			return nil
		}
		if session.Share != nil && session.Share.ID == clientID {
			help := session.Help
			session.Share = nil
			log.Printf("Share disconnected from session %s", session.ID)
			if help != nil {
				return &DisconnectResult{
					PeerToNotify: help,
					SessionID:    session.ID,
					WasShare:     true,
				}
			}
			return nil
		}
	}
	return nil
}

// CleanupExpired 清理过期会话
// 如果会话有活跃的 help 连接（正在使用中），跳过清理，只移除协助码映射防止新连接加入
func (sm *SessionManager) CleanupExpired() []string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	var expired []string
	for id, session := range sm.sessions {
		if now.After(session.ExpiresAt) {
			// 移除协助码映射，防止新的 help 通过过期码加入
			delete(sm.byCode, session.Code)

			// 如果会话正在使用中（share 和 help 都在），保持连接
			if session.Share != nil && session.Help != nil {
				continue
			}

			// 非活跃会话（缺少一方或双方已断开），清理
			expired = append(expired, id)
			session.closed = true
			if session.Share != nil && session.Share.Conn != nil {
				session.Share.Conn.Close()
			}
			if session.Help != nil && session.Help.Conn != nil {
				session.Help.Conn.Close()
			}
			delete(sm.sessions, id)
			if session.ClientID != "" {
				delete(sm.byClientID, session.ClientID)
			}
		}
	}
	return expired
}

// generateSessionID 生成会话ID
func generateSessionID() string {
	return "ses_" + time.Now().Format("20060102150405") + "_" + randomString(8)
}

// PeerAddrUpdate contains the result of a peer address update
type PeerAddrUpdate struct {
	Peer        *ClientConn
	IsShareSide bool
	SameNetwork bool   // 两端是否在同一网络
	NATType     string // 本端 NAT 类型（透传给对端）
}

// UpdatePeerAddr updates a client's peer addresses and returns the paired client info
func (sm *SessionManager) UpdatePeerAddr(clientID string, publicAddr, privateAddr, natType string) *PeerAddrUpdate {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, session := range sm.sessions {
		if session.Share != nil && session.Share.ID == clientID {
			session.SharePublicAddr = publicAddr
			session.SharePrivateAddr = privateAddr
			session.ShareNATType = natType
			if session.Help != nil {
				sameNet := detectSameNetwork(
					session.Share.Conn.RemoteAddr(), session.Help.Conn.RemoteAddr(),
					session.SharePrivateAddr, session.HelpPrivateAddr,
				)
				return &PeerAddrUpdate{Peer: session.Help, IsShareSide: true, SameNetwork: sameNet, NATType: natType}
			}
			return nil
		}
		if session.Help != nil && session.Help.ID == clientID {
			session.HelpPublicAddr = publicAddr
			session.HelpPrivateAddr = privateAddr
			session.HelpNATType = natType
			if session.Share != nil {
				sameNet := detectSameNetwork(
					session.Share.Conn.RemoteAddr(), session.Help.Conn.RemoteAddr(),
					session.SharePrivateAddr, session.HelpPrivateAddr,
				)
				return &PeerAddrUpdate{Peer: session.Share, IsShareSide: false, SameNetwork: sameNet, NATType: natType}
			}
			return nil
		}
	}
	return nil
}

// detectSameNetwork 检测两端是否在同一网络
func detectSameNetwork(shareRemote, helpRemote string, sharePrivate, helpPrivate string) bool {
	// Check 1: 同一公网 IP（同一 NAT 出口）
	shareHost, _, _ := net.SplitHostPort(shareRemote)
	helpHost, _, _ := net.SplitHostPort(helpRemote)
	if shareHost != "" && shareHost == helpHost {
		log.Printf("Same network detected: same public IP %s", shareHost)
		return true
	}
	// Check 2: 私网 IP 同一 /16 子网
	if sharePrivate != "" && helpPrivate != "" {
		sIP, _, _ := net.SplitHostPort(sharePrivate)
		hIP, _, _ := net.SplitHostPort(helpPrivate)
		sNetIP := net.ParseIP(sIP)
		hNetIP := net.ParseIP(hIP)
		if sNetIP != nil && hNetIP != nil {
			s4 := sNetIP.To4()
			h4 := hNetIP.To4()
			if s4 != nil && h4 != nil && s4[0] == h4[0] && s4[1] == h4[1] {
				log.Printf("Same network detected: private IPs %s and %s in same /16 subnet", sIP, hIP)
				return true
			}
		}
	}
	return false
}

// FindPeer finds the paired client for a given client ID
func (sm *SessionManager) FindPeer(clientID string) *ClientConn {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, session := range sm.sessions {
		if session.Share != nil && session.Share.ID == clientID {
			return session.Help
		}
		if session.Help != nil && session.Help.ID == clientID {
			return session.Share
		}
	}
	return nil
}

func randomString(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
		b[i] = charset[num.Int64()]
	}
	return string(b)
}
