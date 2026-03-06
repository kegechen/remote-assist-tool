package relay

import (
	"crypto/rand"
	"math/big"
	"strings"
)

const (
	charset           = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	defaultCodeLength = 10
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
