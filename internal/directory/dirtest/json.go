package dirtest

import (
	"encoding/json"
	"testing"
)

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustUnmarshal(t testing.TB, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

// MustJSON 供其它包的测试复用。
func MustJSON(t testing.TB, v any) []byte {
	t.Helper()
	return mustJSON(t, v)
}
