package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBootstrapCallToolNotConnectedError(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	_, err := b.CallTool(context.Background(), "read_file", json.RawMessage(`{"path":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "not_connected") {
		t.Fatalf("expected not_connected error, got %v", err)
	}
}

func TestBootstrapConnectRequiresCode(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	_, err := b.CallTool(context.Background(), "connect", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "code required") {
		t.Fatalf("expected code required error, got %v", err)
	}
}
