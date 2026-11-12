package relay

import (
	"crypto/rand"
	"math/big"
	"strings"
)

const (
	// 协助码只含大写字母和数字，并去掉易混淆的 I L O 0 1（共 31 个字符）。
	// 字符集比旧版（54 个，大小写混合）小，所以默认长度由 10 提到 12，熵约 59 bit，不低于旧版的约 57 bit。
	charset           = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	defaultCodeLength = 12
)

// CodeManager generates assistance codes
type CodeManager struct {
	codeLength int
}

// NewCodeManager creates a code manager
func NewCodeManager(codeLength int) *CodeManager {
	if codeLength <= 0 {
		codeLength = defaultCodeLength
	}
	return &CodeManager{
		codeLength: codeLength,
	}
}

// Generate generates a new assistance code
func (cm *CodeManager) Generate() (string, error) {
	return generateCode(cm.codeLength)
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

// FormatCode formats a code for display
func FormatCode(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}
