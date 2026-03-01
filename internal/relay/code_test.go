package relay

import (
	"strings"
	"testing"
	"time"
)

func TestCodeGeneration(t *testing.T) {
	cm := NewCodeManager(30*time.Minute, 10)

	code, err := cm.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if len(code) != 10 {
		t.Errorf("Expected code length 10, got %d", len(code))
	}

	// 验证字符集
	validChars := "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	for _, c := range code {
		if !strings.ContainsRune(validChars, c) {
			t.Errorf("Invalid character in code: %c", c)
		}
	}

	// 验证没有易混淆字符
	confusing := "IiLlOo01"
	for _, c := range code {
		if strings.ContainsRune(confusing, c) {
			t.Errorf("Found confusing character: %c", c)
		}
	}
}

func TestCodeValidation(t *testing.T) {
	cm := NewCodeManager(30*time.Minute, 10)

	code, _ := cm.Generate()

	// 有效码
	info, ok := cm.Validate(code)
	if !ok {
		t.Error("Valid code should validate")
	}
	if info.Code != code {
		t.Error("Code mismatch")
	}

	// 带分隔符的码
	info, ok = cm.Validate(code[:4] + "-" + code[4:])
	if !ok {
		t.Error("Code with separator should validate")
	}

	// 带空格的码
	info, ok = cm.Validate(code[:4] + " " + code[4:])
	if !ok {
		t.Error("Code with space should validate")
	}

	// 无效码
	_, ok = cm.Validate("INVALID123")
	if ok {
		t.Error("Invalid code should not validate")
	}
}

func TestCodeExpiry(t *testing.T) {
	cm := NewCodeManager(100*time.Millisecond, 10)

	code, _ := cm.Generate()

	// 立即验证应该有效
	_, ok := cm.Validate(code)
	if !ok {
		t.Error("Code should be valid immediately")
	}

	// 等待过期
	time.Sleep(150 * time.Millisecond)

	// 应该失效
	_, ok = cm.Validate(code)
	if ok {
		t.Error("Code should be expired")
	}
}

func TestCodeUniqueness(t *testing.T) {
	cm := NewCodeManager(30*time.Minute, 10)
	codes := make(map[string]bool)

	for i := 0; i < 100; i++ {
		code, err := cm.Generate()
		if err != nil {
			t.Fatalf("Generate failed: %v", err)
		}
		if codes[code] {
			t.Errorf("Duplicate code: %s", code)
		}
		codes[code] = true
	}
}

func TestCodeInvalidation(t *testing.T) {
	cm := NewCodeManager(30*time.Minute, 10)

	code, _ := cm.Generate()

	cm.Invalidate(code)

	_, ok := cm.Validate(code)
	if ok {
		t.Error("Invalidated code should not validate")
	}
}

func TestFormatCode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ABCDEFGHIJ", "ABCD-EFGHIJ"},
		{"ABCD", "ABCD"},
		{"", ""},
	}

	for _, tt := range tests {
		result := FormatCode(tt.input)
		if result != tt.expected {
			t.Errorf("FormatCode(%s) = %s, want %s", tt.input, result, tt.expected)
		}
	}
}
