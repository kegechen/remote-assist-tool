package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

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
	defer l.mu.Unlock()

	event.Timestamp = time.Now().UTC()
	_ = l.encoder.Encode(event)

	fmt.Printf("[%s] %s: %s\n", event.Level, event.Event, event.Message)
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
		"code":       code,
		"client_id":  clientID,
		"expires_at": expiresAt,
	})
}

// LogSessionEstablished 记录会话建立
func LogSessionEstablished(sessionID, code, helperID, targetID string) {
	Log(AuditLevelInfo, "session_established", "会话已建立", map[string]interface{}{
		"session_id": sessionID,
		"code":       code,
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
