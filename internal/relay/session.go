package relay

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

var (
	ErrSessionNotFound  = errors.New("session not found")
	ErrCodeExpired      = errors.New("code expired")
	ErrCodeInvalid      = errors.New("invalid code")
	ErrSessionHasHelper = errors.New("session already has helper")
)

// helpDisconnectDebounce Help 断连去抖窗口：这段时间内重连回来的算网络抖动，不清 Help
// 槽也不惊动 share。窗口过完还没回来才判定为真断开。
// 用 var 而非 const 只为让测试调到毫秒级；运行期不改。
var helpDisconnectDebounce = 5 * time.Second

// Conn 连接接口
type Conn interface {
	io.ReadWriteCloser
	RemoteAddr() string
}

// 连接状态：确保每连接只执行一次注册(register 或 join)，业务消息必须在注册后。
const (
	connStateNew   = ""      // 初始状态，等待首次 register 或 join
	connStateShare = "share" // 已注册为 Share
	connStateHelp  = "help"  // 已注册为 Help
)

// ClientConn 客户端连接
type ClientConn struct {
	ID       string
	Type     string // "share" or "help"
	ClientID string // 持久化客户端ID
	Version  string // 客户端版本（已 sanitizePeerString）
	Host     string // 客户端身份串 "user@host 系统 架构"（已 sanitizePeerString；旧客户端为空）
	Conn     Conn
	Send     chan []byte
	writeMu  sync.Mutex // 串行化对 Conn 的并发写：转发对端消息、心跳 echo、P2P 推送等可能来自不同 goroutine，无锁并发 Write 会交错撕裂单行 JSON 帧

	// joinFailures 本连接累计的 Join 失败次数（含限流拒绝）。仅由该连接的读循环 goroutine
	// 单线程访问，无需加锁。达到 maxJoinFailures 即关闭连接；一次成功 Join 后清零。
	joinFailures int
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
	CreatorIP        string // 创建会话时的计费 IP；Share 断开后仍用于释放 per-IP 配额
	countedByIP      bool   // 是否实际占用了 per-IP 配额（DisableSourceIPLimits 时为 false）
	SharePublicAddr  string // Share 端公网地址
	SharePrivateAddr string // Share 端内网地址
	ShareNATType     string // Share 端 NAT 类型
	HelpPublicAddr   string // Help 端公网地址
	HelpPrivateAddr  string // Help 端内网地址
	HelpNATType      string // Help 端 NAT 类型
	P2PEnabled       bool   // 是否启用 P2P
	closed           bool
	mu               sync.Mutex
	shareReady       bool // RegisterResponse 已发送后才允许 Help Join，防 SessionReady 抢在注册响应前到达

	// dataPlaneReady 数据面（UDP relay）是否就绪。仅 ActivateDataPlane 置 true；
	// 任何断连、Share 复用、新 Help 换代、过期/关闭都必须先置 false，配合控制面
	// 主动失效旧 UDP relay entry，保证「失效到重新激活」窗口内 relay 准入恒为 false。
	dataPlaneReady bool

	// Help 断连去抖
	helpDisconnectTimer *time.Timer // Help 断连延迟计时器
	pendingHelpID       string      // 待断连的 Help ID
}

// SessionManager 会话管理器
type SessionManager struct {
	sessions   map[string]*TunnelSession
	byCode     map[string]*TunnelSession
	byClientID map[string]*TunnelSession
	byConnID   map[string]*TunnelSession
	mu         sync.RWMutex

	// 活跃会话计数：防单 IP 或全局会话耗尽。仅在 CreateSession/CloseSession/CleanupExpired 修改。
	sessionCountPerIP map[string]int // 每 IP 当前活跃会话数（仅 Share 端 IP 计数）

	// onHelpCleared 在 Help 去抖计时器真正清空 Help 槽后回调，参数是同会话的 share
	// （可能为 nil 时不调）。回调一律在 sm.mu 之外执行，实现里可以做网络写。
	// 由 NewServer 在 Start 前一次性装配，此后只读。
	onHelpCleared func(share *ClientConn)
}

// NewSessionManager 创建会话管理器
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions:          make(map[string]*TunnelSession),
		byCode:            make(map[string]*TunnelSession),
		byClientID:        make(map[string]*TunnelSession),
		byConnID:          make(map[string]*TunnelSession),
		sessionCountPerIP: make(map[string]int),
	}
}

// CreateSession 创建会话。返回 (session, error)；超出 per-IP 或全局活跃会话上限时返回 error。
// 调用方须在锁外提取 Share IP 并传入 shareIP，避免锁内调用 Conn（可能阻塞）。
func (sm *SessionManager) CreateSession(code string, share *ClientConn, ttl time.Duration, clientID, shareIP string, maxPerIP, maxTotal int) (*TunnelSession, error) {
	return sm.createSession(code, share, ttl, clientID, shareIP, maxPerIP, maxTotal, true)
}

// createPendingSession 供 Server 注册流程使用；调用 MarkShareReady 前 Help 不得 Join。
func (sm *SessionManager) createPendingSession(code string, share *ClientConn, ttl time.Duration, clientID, shareIP string, maxPerIP, maxTotal int) (*TunnelSession, error) {
	return sm.createSession(code, share, ttl, clientID, shareIP, maxPerIP, maxTotal, false)
}

func (sm *SessionManager) createSession(code string, share *ClientConn, ttl time.Duration, clientID, shareIP string, maxPerIP, maxTotal int, shareReady bool) (*TunnelSession, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 活跃会话上限检查
	total := len(sm.sessions)
	if total >= maxTotal {
		return nil, errors.New("max total sessions reached")
	}
	if shareIP != "" && maxPerIP > 0 {
		perIP := sm.sessionCountPerIP[shareIP]
		if perIP >= maxPerIP {
			return nil, errors.New("max sessions per IP reached")
		}
	}

	now := time.Now()
	// 唯一性由插入点保证，而不是靠"128 bit 不会撞"这句断言：sessions/byCode/byConnID
	// 三张表都是覆盖写，一旦撞键就是把另一条在线会话悄悄挤掉，没有任何日志。
	sessionID := generateSessionID()
	for {
		if _, taken := sm.sessions[sessionID]; !taken {
			break
		}
		log.Printf("Session ID collision on %s, regenerating", sessionID)
		sessionID = generateSessionID()
	}
	if prev, taken := sm.byCode[code]; taken {
		// --no-auth 下 code 是固定常量，后注册顶掉前一个是已知限制（见 proto.NoAuthCode
		// 的注释）。但至少要留一行日志，否则被顶掉的那端只会表现为莫名其妙的 invalid code。
		log.Printf("Session code %s already registered (session %s), overwriting with %s",
			code, prev.ID, sessionID)
	}
	session := &TunnelSession{
		ID:          sessionID,
		Code:        code,
		Share:       share,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
		ClientID:    clientID,
		CreatorIP:   shareIP,
		countedByIP: shareIP != "" && maxPerIP > 0,
		shareReady:  shareReady,
	}

	sm.sessions[session.ID] = session
	sm.byCode[code] = session
	if clientID != "" {
		sm.byClientID[clientID] = session
	}
	if share != nil {
		sm.byConnID[share.ID] = session
	}
	// 计数：仅 Share 端 IP 计入（Help 可能很多，Share 是会话拥有者）
	if session.countedByIP {
		sm.sessionCountPerIP[shareIP]++
	}
	return session, nil
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

// ReuseSessionResult 是 ClientID 复用完成后的不可变快照。
type ReuseSessionResult struct {
	SessionID string
	Code      string
	ExpiresAt time.Time
	OldShare  *ClientConn
	OldHelp   *ClientConn
}

// ReuseSessionByClientID 原子查询并复用会话。调用方只能使用返回快照，不能持有内部 session 指针。
func (sm *SessionManager) ReuseSessionByClientID(clientID string, newShare *ClientConn) (*ReuseSessionResult, bool) {
	if clientID == "" {
		return nil, false
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.byClientID[clientID]
	if !exists {
		return nil, false
	}
	if time.Now().After(session.ExpiresAt) || session.closed {
		return nil, false
	}

	result := &ReuseSessionResult{
		SessionID: session.ID,
		Code:      session.Code,
		ExpiresAt: session.ExpiresAt,
		OldShare:  session.Share,
		OldHelp:   session.Help,
	}

	if session.Share != nil {
		delete(sm.byConnID, session.Share.ID)
	}
	if session.Help != nil {
		delete(sm.byConnID, session.Help.ID)
	}
	if session.helpDisconnectTimer != nil {
		session.helpDisconnectTimer.Stop()
		session.helpDisconnectTimer = nil
	}
	session.Help = nil
	session.pendingHelpID = ""
	session.Share = newShare
	// 端点地址是上一代连接通过 STUN/PeerAddrAdvertise 发布的身份凭据，不能跨
	// 网络复用到新 Share。否则旧端点可以在新会话重新激活前抢先注册 UDP relay。
	session.SharePublicAddr = ""
	session.SharePrivateAddr = ""
	session.ShareNATType = ""
	session.HelpPublicAddr = ""
	session.HelpPrivateAddr = ""
	session.HelpNATType = ""
	session.shareReady = false
	session.dataPlaneReady = false
	sm.byConnID[newShare.ID] = session

	return result, true
}

// MarkShareReady 在 RegisterResponse 写出后发布 Share。expectedShareID 防并发复用误发布旧连接。
func (sm *SessionManager) MarkShareReady(sessionID, expectedShareID string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	session, ok := sm.sessions[sessionID]
	if !ok || session.closed || session.Share == nil || session.Share.ID != expectedShareID {
		return false
	}
	session.shareReady = true
	return true
}

// JoinSessionResult 是 Join 写入 Help 后的不可变 Share 快照。
type JoinSessionResult struct {
	SessionID    string
	Share        *ClientConn
	ShareID      string
	ShareVersion string
	ShareHost    string
}

// JoinSession 协助端加入会话。
func (sm *SessionManager) JoinSession(code string, help *ClientConn) (*JoinSessionResult, error) {
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
	if session.Share == nil || !session.shareReady {
		return nil, ErrSessionNotFound
	}

	// 取消挂起的断连计时器
	if session.helpDisconnectTimer != nil {
		session.helpDisconnectTimer.Stop()
		session.helpDisconnectTimer = nil
		session.pendingHelpID = ""
		log.Printf("Help disconnect debounce cancelled for session %s (new help joined)", session.ID)
	}

	if session.Help != nil {
		delete(sm.byConnID, session.Help.ID)
	}
	session.Help = help
	sm.byConnID[help.ID] = session
	// 新 Help 换代：数据面保持未就绪，待调用方失效旧 UDP entry 后再 ActivateDataPlane。
	session.dataPlaneReady = false
	return &JoinSessionResult{
		SessionID:    session.ID,
		Share:        session.Share,
		ShareID:      session.Share.ID,
		ShareVersion: session.Share.Version,
		ShareHost:    session.Share.Host,
	}, nil
}

// RollbackJoin 回滚 JoinSession 的副作用：清除 session.Help 绑定。
// 用于 ActivateDataPlane 失败后清理已绑定的 Help，避免留下不一致状态。
func (sm *SessionManager) RollbackJoin(sessionID string, helpID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessions[sessionID]
	if !exists {
		return
	}
	// 只清除与预期 helpID 匹配的 Help（避免误清已被新 Join 替换的 Help）
	if session.Help != nil && session.Help.ID == helpID {
		delete(sm.byConnID, helpID)
		session.Help = nil
		session.dataPlaneReady = false
	}
}

// ActivateDataPlane 在调用方已失效旧 UDP relay entry 后，重新确认会话仍为期望的
// Share/Help 配对且无 pending 断连，然后标记数据面就绪。必须「先失效旧 entry，再激活」。
// 返回 false 表示激活窗口内会话状态已变化，调用方应放弃本次配对。
func (sm *SessionManager) ActivateDataPlane(sessionID, expectedShareID, expectedHelpID string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionID]
	if !ok || session.closed {
		return false
	}
	if session.Share == nil || session.Help == nil {
		return false
	}
	if session.Share.ID != expectedShareID || session.Help.ID != expectedHelpID {
		return false
	}
	if session.pendingHelpID != "" {
		return false
	}
	session.dataPlaneReady = true
	return true
}

// IsActiveDataSession 报告某会话的数据面（UDP relay）当前是否就绪，供 STUN relay 层做准入校验。
// 只有控制面两端在线、无 pending 断连且已 ActivateDataPlane 时才返回 true。
func (sm *SessionManager) IsActiveDataSession(sessionID string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return false
	}
	return !session.closed &&
		session.Share != nil &&
		session.shareReady &&
		session.Help != nil &&
		session.pendingHelpID == "" &&
		session.dataPlaneReady
}

// IsActiveDataSource 在 IsActiveDataSession 之上追加来源校验：srcIP 必须是该会话某一端
// 已知的地址。供 STUN/UDP relay 做准入。
//
// 为什么需要：只问「会话是否活跃」等于把 UDP relay 的槽位做成先到先得——任何拿到
// sessionID 的第三方抢先发一个包就能占住一端，之后合法端的数据全被转给它（中间人），
// 或者占满两槽把会话变黑洞。sessionID 会被主动喷洒到对端公网 IP 的一批端口上，并非秘密。
//
// 只比 IP 不比端口：对称 NAT 下每个目的地对应不同的外部端口，45f72ff 里「按 IP 重匹配、
// 更新端口」正是为此而加，比端口会把它打回原形。收紧到 IP 已经挡掉全部离路径攻击者，
// 剩下的同 IP（同一 NAT 出口）攻击面需要 relay-token 才能覆盖，那要改 relay 头部格式。
func (sm *SessionManager) IsActiveDataSource(sessionID string, srcIP net.IP) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return false
	}
	if session.closed ||
		session.Share == nil || !session.shareReady ||
		session.Help == nil || session.pendingHelpID != "" ||
		!session.dataPlaneReady {
		return false
	}
	if srcIP == nil {
		return false
	}
	return sessionKnowsIPLocked(session, srcIP)
}

// sessionKnowsIPLocked 判断 srcIP 是否属于会话任一端。调用方须持 sm.mu（读锁即可）。
//
// 三个来源都算数：STUN 反射得到的公网地址、对端自报的私网地址（两端同 LAN 时 relay
// 看到的就是它），以及 relay 自己观测到的 TCP 源 IP。最后一个不依赖对端自报，是通告
// 尚未到达时唯一可用的凭据，handlePeerAddrAdvertise 的公网地址回退用的也是它。
func sessionKnowsIPLocked(session *TunnelSession, srcIP net.IP) bool {
	candidates := []string{
		session.SharePublicAddr, session.SharePrivateAddr,
		session.HelpPublicAddr, session.HelpPrivateAddr,
	}
	if session.Share != nil && session.Share.Conn != nil {
		candidates = append(candidates, session.Share.Conn.RemoteAddr())
	}
	if session.Help != nil && session.Help.Conn != nil {
		candidates = append(candidates, session.Help.Conn.RemoteAddr())
	}
	for _, c := range candidates {
		if hostMatchesIP(c, srcIP) {
			return true
		}
	}
	return false
}

// hostMatchesIP 比较 "host:port"（或裸 host）里的 IP 与 srcIP。解析不出 IP 的一律不匹配：
// 通告字段是对端自报的，允许域名只会把校验变成可绕过的摆设。
func hostMatchesIP(hostPort string, srcIP net.IP) bool {
	if hostPort == "" {
		return false
	}
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.Equal(srcIP)
}

// decrementSessionCountLocked 减少 shareIP 的会话计数。调用方须持 sm.mu。
func (sm *SessionManager) decrementSessionCountLocked(shareIP string) {
	if shareIP == "" {
		return
	}
	if cur, ok := sm.sessionCountPerIP[shareIP]; ok {
		if cur <= 1 {
			delete(sm.sessionCountPerIP, shareIP)
		} else {
			sm.sessionCountPerIP[shareIP] = cur - 1
		}
	}
}

// deleteSessionLocked 从所有索引移除 session，并且只释放一次配额。调用方须持 sm.mu。
func (sm *SessionManager) deleteSessionLocked(sessionID string, expected *TunnelSession) (share, help *ClientConn, deleted bool) {
	session, ok := sm.sessions[sessionID]
	if !ok || session != expected {
		return nil, nil, false
	}
	session.closed = true
	session.dataPlaneReady = false
	if session.helpDisconnectTimer != nil {
		session.helpDisconnectTimer.Stop()
		session.helpDisconnectTimer = nil
	}
	if session.Share != nil {
		share = session.Share
		if sm.byConnID[share.ID] == session {
			delete(sm.byConnID, share.ID)
		}
	}
	if session.Help != nil {
		help = session.Help
		if sm.byConnID[help.ID] == session {
			delete(sm.byConnID, help.ID)
		}
	}
	delete(sm.sessions, sessionID)
	if sm.byCode[session.Code] == session {
		delete(sm.byCode, session.Code)
	}
	if session.ClientID != "" && sm.byClientID[session.ClientID] == session {
		delete(sm.byClientID, session.ClientID)
	}
	if session.countedByIP {
		sm.decrementSessionCountLocked(session.CreatorIP)
		session.countedByIP = false
	}
	return share, help, true
}

// CloseSession 关闭会话
func (sm *SessionManager) CloseSession(sessionID string) {
	sm.mu.Lock()
	session := sm.sessions[sessionID]
	share, help, _ := sm.deleteSessionLocked(sessionID, session)
	sm.mu.Unlock()
	if share != nil && share.Conn != nil {
		share.Conn.Close()
	}
	if help != nil && help.Conn != nil {
		help.Conn.Close()
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
	// ResetDataPlane 为 true 时，调用方须在 SessionManager 解锁后失效 SessionID 的 UDP relay entry。
	ResetDataPlane bool
}

// DisconnectClient clears a client from its session when the connection drops.
// For help clients: clears session.Help so a new helper can rejoin.
// For share clients: returns the help client so the server can notify it.
func (sm *SessionManager) DisconnectClient(clientID string) *DisconnectResult {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session := sm.byConnID[clientID]
	if session == nil {
		return nil
	}
	if session.Help != nil && session.Help.ID == clientID {
		delete(sm.byConnID, clientID)
		// 数据面立即失效：Help 断连后旧 UDP relay 状态不得在去抖窗口内残留。
		session.dataPlaneReady = false
		// Help 断连去抖：延迟 5 秒再清除，防止网络抖动导致不必要的重连
		session.pendingHelpID = clientID
		session.helpDisconnectTimer = time.AfterFunc(helpDisconnectDebounce, func() {
			sm.mu.Lock()
			var share *ClientConn
			if session.Help != nil && session.Help.ID == session.pendingHelpID {
				session.Help = nil
				session.pendingHelpID = ""
				// 去抖窗口过完仍没等回协助端，这才是真断开：把 share 从「等隧道数据」
				// 里叫醒，否则它会一直挂在读上，直到 2 分钟读超时或会话过期才回过神。
				share = session.Share
				log.Printf("Cleared helper from session %s (after debounce)", session.ID)
			}
			sm.mu.Unlock()
			// 通知必须在锁外：sendMsg 是带 30s 超时的同步网络写，攥着 sm.mu 写等于
			// 让一个慢读客户端冻住整张会话表。
			if share != nil && sm.onHelpCleared != nil {
				sm.onHelpCleared(share)
			}
		})
		log.Printf("Help disconnect debounce started for session %s (%s)", session.ID, helpDisconnectDebounce)
		return &DisconnectResult{SessionID: session.ID, ResetDataPlane: true}
	}
	if session.Share != nil && session.Share.ID == clientID {
		delete(sm.byConnID, clientID)
		// 数据面先失效再清 Share；调用方解锁后失效旧 UDP entry。
		session.dataPlaneReady = false
		session.shareReady = false
		help := session.Help
		session.Share = nil
		log.Printf("Share disconnected from session %s", session.ID)
		return &DisconnectResult{
			PeerToNotify:   help, // 可能为 nil
			SessionID:      session.ID,
			WasShare:       true,
			ResetDataPlane: true,
		}
	}
	return nil
}

// CleanupExpired 清理过期会话
// 如果会话有活跃的 help 连接（正在使用中），跳过清理，只移除协助码映射防止新连接加入
func (sm *SessionManager) CleanupExpired() []string {
	sm.mu.Lock()

	now := time.Now()
	var expired []string
	var toClose []*ClientConn
	for id, session := range sm.sessions {
		if now.After(session.ExpiresAt) {
			// 移除协助码映射，防止新的 help 通过过期码加入。
			// 与 deleteSessionLocked 一致做指针复核：no-auth 下 code 恒为 NoAuthCode，
			// 后注册的 B 会顶掉 A 在 byCode 里的映射，此时 A 过期若无条件 delete，
			// 抹掉的其实是指向 B 的条目——B 在线且未过期却从此 ErrCodeInvalid，且无日志可循。
			if sm.byCode[session.Code] == session {
				delete(sm.byCode, session.Code)
			}

			// 如果会话正在使用中（share 和 help 都在），保持连接
			if session.Share != nil && session.Help != nil {
				continue
			}

			share, help, deleted := sm.deleteSessionLocked(id, session)
			if deleted {
				expired = append(expired, id)
				if share != nil {
					toClose = append(toClose, share)
				}
				if help != nil {
					toClose = append(toClose, help)
				}
			}
		}
	}
	sm.mu.Unlock()
	for _, client := range toClose {
		if client.Conn != nil {
			client.Conn.Close()
		}
	}
	return expired
}

// generateSessionID 生成会话ID。保留时间戳段便于排障，随机段是 128 bit（见 idRandomBytes）。
func generateSessionID() string {
	return "ses_" + time.Now().Format("20060102150405") + "_" + randomToken(idRandomBytes)
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

	session := sm.byConnID[clientID]
	if session == nil {
		return nil
	}
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
	return nil
}

// detectSameNetwork 检测两端是否在同一网络
func detectSameNetwork(shareRemote, helpRemote string, sharePrivate, helpPrivate string) bool {
	// Check 1: 同一公网 IP（同一 NAT 出口）
	shareHost, _, _ := net.SplitHostPort(shareRemote)
	helpHost, _, _ := net.SplitHostPort(helpRemote)
	if shareHost != "" && shareHost == helpHost {
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

	session := sm.byConnID[clientID]
	if session == nil {
		return nil
	}
	if session.Share != nil && session.Share.ID == clientID {
		return session.Help
	}
	if session.Help != nil && session.Help.ID == clientID {
		return session.Share
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

// idRandomBytes 连接 ID / 会话 ID 随机部分的字节数。
//
// 这两个 ID 不只是日志标识：连接 ID 是 byConnID 的唯一路由键（FindPeer/UpdatePeerAddr
// 全靠它决定把 tunnel_data、tool_resp 投给谁），会话 ID 还兼作 P2P 打洞与 UDP relay 的
// 准入凭据。原先的 randomString(6)/randomString(8) 只有 36^6≈2^31 / 36^8≈2^41，碰撞后
// 两条互不相干的会话会共用一个路由键，数据被投错人。128 bit 把碰撞从"打满几小时就有
// 期望命中"变成不可能。
const idRandomBytes = 16

// randomToken 返回 n 字节 crypto/rand 的小写 base32（无填充）。
// base32 而非 hex：同样的熵少 20% 字符，且字符集与既有 ID 一样是小写字母数字。
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
