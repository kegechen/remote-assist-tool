package relay

import (
	"crypto/rand"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	charset           = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	defaultCodeLength = 10
)

// CodeManager 协助码管理器
type CodeManager struct {
	codes      map[string]*CodeInfo
	mu         sync.RWMutex
	ttl        time.Duration
	codeLength int
}

// CodeInfo 协助码信息
type CodeInfo struct {
	Code      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewCodeManager 创建协助码管理器
func NewCodeManager(ttl time.Duration, codeLength int) *CodeManager {
	if codeLength <= 0 {
		codeLength = defaultCodeLength
	}
	cm := &CodeManager{
		codes:      make(map[string]*CodeInfo),
		ttl:        ttl,
		codeLength: codeLength,
	}
	go cm.cleanupLoop()
	return cm
}

// Generate 生成协助码
func (cm *CodeManager) Generate() (string, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	var code string
	var err error
	for i := 0; i < 100; i++ {
		code, err = generateCode(cm.codeLength)
		if err != nil {
			return "", err
		}
		if _, exists := cm.codes[code]; !exists {
			break
		}
	}

	if _, exists := cm.codes[code]; exists {
		return "", ErrCodeGenerateFailed
	}

	now := time.Now()
	cm.codes[code] = &CodeInfo{
		Code:      code,
		CreatedAt: now,
		ExpiresAt: now.Add(cm.ttl),
	}

	return code, nil
}

// Validate 验证协助码
func (cm *CodeManager) Validate(code string) (*CodeInfo, bool) {
	code = normalizeCode(code)

	cm.mu.RLock()
	defer cm.mu.RUnlock()

	info, exists := cm.codes[code]
	if !exists {
		return nil, false
	}

	if time.Now().After(info.ExpiresAt) {
		return nil, false
	}

	return info, true
}

// Invalidate 使协助码失效
func (cm *CodeManager) Invalidate(code string) {
	code = normalizeCode(code)

	cm.mu.Lock()
	defer cm.mu.Unlock()

	delete(cm.codes, code)
}

func (cm *CodeManager) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		cm.cleanup()
	}
}

func (cm *CodeManager) cleanup() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	now := time.Now()
	for code, info := range cm.codes {
		if now.After(info.ExpiresAt) {
			delete(cm.codes, code)
		}
	}
}

func generateCode(length int) (string, error) {
	result := make([]byte, length)
	for i := range result {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		result[i] = charset[num.Int64()]
	}
	return string(result), nil
}

func normalizeCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '_' {
			return -1
		}
		return r
	}, code)
}

// FormatCode 格式化显示协助码
func FormatCode(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}
