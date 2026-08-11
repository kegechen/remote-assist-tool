package logger

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

var (
	auditKey     [32]byte
	auditKeyOnce sync.Once
)

// ensureAuditKey 首次调用时用 crypto/rand 填充审计密钥。
// 密钥仅存于内存、随进程重启轮换，不暴露可变 setter。
func ensureAuditKey() {
	auditKeyOnce.Do(func() {
		if _, err := rand.Read(auditKey[:]); err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
	})
}

// hmacSum 计算 key 对 s 的 HMAC-SHA256 原始摘要。
func hmacSum(key [32]byte, s string) [32]byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(s))
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

// CodeFingerprint 返回协助码的不可逆指纹，用于跨事件关联同一会话。
// 取 HMAC-SHA256 原始摘要前 12 字节再 hex 编码，恒为 24 个十六进制字符。
func CodeFingerprint(s string) string {
	ensureAuditKey()
	sum := hmacSum(auditKey, s)
	return hex.EncodeToString(sum[:12])
}

// MaskCode 返回用于排障日志的固定长度掩码，不泄露任何码字符。
func MaskCode(s string) string {
	return "********"
}

// AuditLevel 审计级别
type AuditLevel string

const (
	AuditLevelInfo  AuditLevel = "INFO"
	AuditLevelWarn  AuditLevel = "WARN"
	AuditLevelError AuditLevel = "ERROR"
)

// AuditEvent 审计事件
type AuditEvent struct {
	Timestamp time.Time              `json:"timestamp"`
	Level     AuditLevel             `json:"level"`
	Event     string                 `json:"event"`
	SessionID string                 `json:"session_id,omitempty"`
	ClientID  string                 `json:"client_id,omitempty"`
	ClientIP  string                 `json:"client_ip,omitempty"`
	Code      string                 `json:"code,omitempty"`
	Details   map[string]interface{} `json:"details,omitempty"`
	Message   string                 `json:"message"`
}

// AuditLogger 审计日志记录器
type AuditLogger struct {
	file    *os.File
	mu      sync.Mutex
	encoder *json.Encoder
}

var (
	defaultLogger *AuditLogger
	once          sync.Once
)

// InitAuditLogger 初始化审计日志
func InitAuditLogger(filename string) error {
	var err error
	once.Do(func() {
		defaultLogger, err = NewAuditLogger(filename)
	})
	return err
}

// NewAuditLogger 创建审计日志记录器
func NewAuditLogger(filename string) (*AuditLogger, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}

	return &AuditLogger{
		file:    file,
		encoder: json.NewEncoder(file),
	}, nil
}

// Close 关闭日志文件
func (l *AuditLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Log 记录审计事件
func (l *AuditLogger) Log(event AuditEvent) {
	l.mu.Lock()
	event.Timestamp = time.Now().UTC()
	_ = l.encoder.Encode(event)
	l.mu.Unlock()

	// Keep console/Event Log output outside the file lock. A blocked output sink
	// must not stop other audit records from reaching disk.
	log.Printf("[%s] %s: %s", event.Level, event.Event, event.Message)
}

// Log 便捷方法
func Log(level AuditLevel, eventName, message string, details map[string]interface{}) {
	if defaultLogger != nil {
		defaultLogger.Log(AuditEvent{
			Level:   level,
			Event:   eventName,
			Message: message,
			Details: details,
		})
	}
}

// LogConnection 记录连接事件
func LogConnection(clientIP, clientID string, success bool, message string) {
	event := "connection_attempt"
	if success {
		event = "connection_success"
	}
	level := AuditLevelInfo
	if !success {
		level = AuditLevelWarn
	}
	Log(level, event, message, map[string]interface{}{
		"client_ip": clientIP,
		"client_id": clientID,
		"success":   success,
	})
}

// LogCodeGenerated 记录协助码生成
func LogCodeGenerated(code string, clientID string, expiresAt time.Time) {
	Log(AuditLevelInfo, "code_generated", "协助码已生成", map[string]interface{}{
		"code_fp":    CodeFingerprint(code),
		"client_id":  clientID,
		"expires_at": expiresAt,
	})
}

// LogSessionEstablished 记录会话建立
func LogSessionEstablished(sessionID, code, helperID, targetID string) {
	Log(AuditLevelInfo, "session_established", "会话已建立", map[string]interface{}{
		"session_id": sessionID,
		"code_fp":    CodeFingerprint(code),
		"helper_id":  helperID,
		"target_id":  targetID,
	})
}

// LogSessionClosed 记录会话关闭
func LogSessionClosed(sessionID, reason string) {
	Log(AuditLevelInfo, "session_closed", reason, map[string]interface{}{
		"session_id": sessionID,
		"reason":     reason,
	})
}
