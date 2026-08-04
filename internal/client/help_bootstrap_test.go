package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/mcp"
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

func TestBootstrapConnectReusesMatchingActiveSession(t *testing.T) {
	cfg := &Config{ServerAddr: "relay.example"}
	b := NewHelpMCPBootstrap(cfg)
	activeClient := NewClient(cfg)
	want := connectResult{
		Connected:   true,
		SessionID:   "session-existing",
		Server:      "relay.example:8443",
		P2P:         true,
		PeerVersion: "peer-version",
		HelpVersion: "help-version",
	}
	b.help = &HelpMode{client: activeClient}
	b.bridge = mcp.NewBridge(nil, [32]byte{})
	b.activeTarget = connectTarget{Code: "ABCDEF", Server: "relay.example:8443"}
	b.activeResult = want

	raw, err := b.doConnect(context.Background(), json.RawMessage(`{"code":"ABCD-EF"}`))
	if err != nil {
		t.Fatalf("重复 connect 应复用活动会话: %v", err)
	}
	var got connectResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got != want {
		t.Fatalf("result=%+v, want %+v", got, want)
	}
	if activeClient.IsClosed() {
		t.Fatal("重复 connect 关闭了仍健康的 client")
	}
}
