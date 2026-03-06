package relay

import (
	"strings"
	"testing"
)

func TestCodeGeneration(t *testing.T) {
	cm := NewCodeManager(10)

	code, err := cm.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if len(code) != 10 {
		t.Errorf("Expected code length 10, got %d", len(code))
	}

	// Verify character set
	validChars := "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
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
