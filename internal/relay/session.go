package relay

import (
	"crypto/rand"
	"errors"
	"io"
	"math/big"
	"sync"
	"time"
)

var (
	ErrCodeGenerateFailed = errors.New("failed to generate unique code")
	ErrSessionNotFound    = errors.New("session not found")
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
	HelpPublicAddr   string // Help 端公网地址
	HelpPrivateAddr  string // Help 端内网地址
	P2PEnabled       bool   // 是否启用 P2P
	closed           bool
	mu               sync.Mutex
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
	if session.Help != nil {
		return nil, ErrSessionHasHelper
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

// CleanupExpired 清理过期会话
func (sm *SessionManager) CleanupExpired() []string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	var expired []string
	for id, session := range sm.sessions {
		if now.After(session.ExpiresAt) {
			expired = append(expired, id)
			session.closed = true
			if session.Share != nil && session.Share.Conn != nil {
				session.Share.Conn.Close()
			}
			if session.Help != nil && session.Help.Conn != nil {
				session.Help.Conn.Close()
			}
			delete(sm.sessions, id)
			delete(sm.byCode, session.Code)
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
}

// UpdatePeerAddr updates a client's peer addresses and returns the paired client info
func (sm *SessionManager) UpdatePeerAddr(clientID string, publicAddr, privateAddr string) *PeerAddrUpdate {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, session := range sm.sessions {
		if session.Share != nil && session.Share.ID == clientID {
			session.SharePublicAddr = publicAddr
			session.SharePrivateAddr = privateAddr
			if session.Help != nil {
				return &PeerAddrUpdate{Peer: session.Help, IsShareSide: true}
			}
			return nil
		}
		if session.Help != nil && session.Help.ID == clientID {
			session.HelpPublicAddr = publicAddr
			session.HelpPrivateAddr = privateAddr
			if session.Share != nil {
				return &PeerAddrUpdate{Peer: session.Share, IsShareSide: false}
			}
			return nil
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
