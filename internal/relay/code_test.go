package relay

import (
	"strings"
	"testing"
)

func TestCodeGeneration(t *testing.T) {
	cm := NewCodeManager(12)

	code, err := cm.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if len(code) != 12 {
		t.Errorf("Expected code length 12, got %d", len(code))
	}

	// Verify character set
	validChars := "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // 只允许大写字母和数字
	for _, c := range code {
		if !strings.ContainsRune(validChars, c) {
			t.Errorf("Invalid character in code: %c", c)
		}
	}

	// Verify no confusing characters
	confusing := "IiLlOo01"
	for _, c := range code {
		if strings.ContainsRune(confusing, c) {
			t.Errorf("Found confusing character: %c", c)
		}
	}
}

func TestCodeUniqueness(t *testing.T) {
	cm := NewCodeManager(10)
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

func TestNormalizeCode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ABCD-EFGHIJ", "ABCDEFGHIJ"},
		{"ABCD EFGHIJ", "ABCDEFGHIJ"},
		{"ABCD_EFGHIJ", "ABCDEFGHIJ"},
		{"ABCDEFGHIJ", "ABCDEFGHIJ"},
	}

	for _, tt := range tests {
		result := normalizeCode(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeCode(%s) = %s, want %s", tt.input, result, tt.expected)
		}
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

func TestDefaultCodeLength(t *testing.T) {
	cm := NewCodeManager(0)
	code, err := cm.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if len(code) != defaultCodeLength {
		t.Errorf("Expected default length %d, got %d", defaultCodeLength, len(code))
	}
}

// 协助码只能由大写字母和数字组成，任何位置都不能出现小写字母。
func TestCodeUppercaseAndDigitsOnly(t *testing.T) {
	cm := NewCodeManager(0)
	for i := 0; i < 2000; i++ {
		code, err := cm.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 12 {
			t.Fatalf("默认长度应为 12，得到 %d (%q)", len(code), code)
		}
		for _, c := range code {
			if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				t.Fatalf("协助码含非大写字母/数字字符 %q: %q", c, code)
			}
		}
	}
}
